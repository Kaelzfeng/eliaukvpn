// Package agent runs the Eliauk VPN client core: identity loading, P2P socket +
// STUN probe, coordination-server registration, tunnel/virtual NIC/LAN-discovery
// setup, room membership, and automatic peer connection. It is shared by the
// interactive CLI (cmd/client) and the Windows tray GUI (cmd/gui), so both
// present the same agent underneath.
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/crypto"
	"eliaukvpn/internal/lan"
	"eliaukvpn/internal/p2p"
	"eliaukvpn/internal/protocol"
	"eliaukvpn/internal/stun"
	"eliaukvpn/internal/vnic"
)

// WebSocket heartbeat timing (see Run/messageLoop/pingLoop). The client pings
// the server every wsPingPeriod and requires a pong within wsPongWait, which
// (a) keeps an otherwise-idle link alive through an idle timeout such as
// Cloudflare's ~100s WebSocket cutoff and (b) detects a dead link so the agent
// tears down and reconnects.
const (
	wsPongWait   = 60 * time.Second
	wsPingPeriod = 20 * time.Second
	wsWriteWait  = 10 * time.Second
)

// Options configures the agent. Fields mirror cmd/client's flags.
type Options struct {
	Name          string
	Server        string
	StunPrimary   string
	StunSecondary string
	ForceRelay    bool
	UseVnic       bool
	VnicName      string
	LanEmu        bool
	DebugPackets  bool
	Keyfile       string
	// Info prints user-facing progress lines (identity, registration,
	// endpoints). Logf prints warnings and diagnostics.
	Info func(format string, args ...any)
	Logf func(format string, args ...any)
}

// Status is a point-in-time snapshot of the agent for the UI.
type Status struct {
	Name       string
	ID         string
	VirtualIP  string
	Public     string
	NAT        string
	Identity   string
	Registered bool
	// VnicMsg is non-empty when the virtual NIC could not be created (the
	// process was not elevated, the Wintun driver is missing, etc.). The GUI
	// shows it prominently instead of silently continuing without a NIC.
	VnicMsg string
	// LastErr is the last server error, and Room the room code if in one.
	LastErr string
	Room    string
}

// Agent is one running client core.
type Agent struct {
	opts Options

	mu        sync.Mutex
	conn      *websocket.Conn
	myID      string
	myVIP     string
	tunnel    *p2p.Tunnel
	adVnic    *vnic.Adapter
	routes    map[string]string // virtual ip -> peer id (M4)
	attempted map[string]bool   // peer ids we auto-connected to
	byID      map[string]protocol.Peer
	byName    map[string]protocol.Peer

	identity *crypto.Identity
	// friends is the *effective* tunnel whitelist: the current room members'
	// fingerprints (roomFP). Empty when not in a room.
	friends [][]byte
	roomFP  [][]byte // fingerprints of the room members (excluding self)
	probe   *stun.Result
	p2pConn *net.UDPConn

	room    *RoomState // current room, nil when not in one
	errNote string     // last server error, cleared on registration

	vnicErr string

	wsErr   chan error
	writeMu sync.Mutex // serializes WS writes (data + heartbeat ping)
}

// RoomState is the agent's view of the room it is in (nil when not in one).
type RoomState struct {
	Code    string
	Members []protocol.RoomMember
}

// New loads identity and friends, opens the P2P socket, runs the STUN probe,
// dials the coordination server, and sends registration. The message loop is
// started by Run.
func New(opts Options) (*Agent, error) {
	if opts.Info == nil {
		opts.Info = log.Printf
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.Server == "" {
		opts.Server = "ws://127.0.0.1:8080/ws"
	}
	if opts.StunPrimary == "" {
		opts.StunPrimary = "stun.l.google.com:19302"
	}
	if opts.StunSecondary == "" {
		opts.StunSecondary = "stun.cloudflare.com:3478"
	}

	// 0. Load (or create) the long-term X25519 identity. This authenticates
	//    the handshake to room members and encrypts all tunnel data (M6). Share
	//    the fingerprint below so the server can place us in a room.
	identity, err := crypto.LoadOrCreate(opts.Keyfile)
	if err != nil {
		return nil, fmt.Errorf("load identity %q: %w", opts.Keyfile, err)
	}
	opts.Info("identity        : %s", identity.Fingerprint())

	// 1. Open the P2P socket FIRST — hole punching must use the same socket
	//    whose public mapping we advertise, so the STUN probe runs on it.
	p2pConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("open p2p socket: %w", err)
	}
	probe := stunProbeOn(p2pConn, opts.StunPrimary, opts.StunSecondary)
	opts.Info("public endpoint : %s", probeString(probe))
	opts.Info("NAT type        : %s", probe.NAT)

	// 2. Register with the coordination server. The client identifies itself by
	//    its X25519 fingerprint (the stable identity rooms key on) and a display
	//    name.
	conn, _, err := websocket.DefaultDialer.Dial(opts.Server, nil)
	if err != nil {
		p2pConn.Close()
		return nil, fmt.Errorf("connect to server: %w", err)
	}
	regReq := protocol.RegisterRequest{
		Name:      opts.Name,
		PublicKey: identity.Fingerprint(),
	}
	if err := send(conn, protocol.TypeRegister, regReq); err != nil {
		p2pConn.Close()
		conn.Close()
		return nil, fmt.Errorf("register: %w", err)
	}

	return &Agent{
		opts:      opts,
		conn:      conn,
		p2pConn:   p2pConn,
		identity:  identity,
		probe:     probe,
		routes:    make(map[string]string),
		attempted: make(map[string]bool),
		byID:      make(map[string]protocol.Peer),
		byName:    make(map[string]protocol.Peer),
		wsErr:     make(chan error, 1),
	}, nil
}

// Run starts the WebSocket message loop and blocks until the context is
// cancelled or the server connection fails.
func (a *Agent) Run(ctx context.Context) error {
	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go a.messageLoop()
	go a.pingLoop(pingCtx)
	var err error
	select {
	case <-ctx.Done():
	case err = <-a.wsErr:
	}
	a.conn.Close()
	a.p2pConn.Close()
	return err
}

// Close tears down the agent. It is safe to call after Run returns.
func (a *Agent) Close() {
	a.conn.Close()
	a.p2pConn.Close()
}

// Status returns the current UI-relevant snapshot.
func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{
		Name:       a.opts.Name,
		ID:         a.myID,
		VirtualIP:  a.myVIP,
		Identity:   a.identity.Fingerprint(),
		Registered: a.myID != "",
		LastErr:    a.errNote,
	}
	if a.probe != nil {
		st.NAT = string(a.probe.NAT)
		if a.probe.Mapped.IP != nil {
			st.Public = fmt.Sprintf("%s:%d", a.probe.Mapped.IP, a.probe.Mapped.Port)
		}
	}
	if a.room != nil {
		st.Room = a.room.Code
	}
	st.VnicMsg = a.vnicErr
	return st
}

// Peers returns a copy of the known online peers, sorted by name.
func (a *Agent) Peers() []protocol.Peer {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]protocol.Peer, 0, len(a.byID))
	for _, p := range a.byID {
		out = append(out, p)
	}
	return out
}

// Snapshot returns the tunnel's per-peer connection states.
func (a *Agent) Snapshot() []p2p.Snapshot {
	a.mu.Lock()
	t := a.tunnel
	a.mu.Unlock()
	if t == nil {
		return nil
	}
	return t.Snapshot()
}

// Connect requests a P2P connection to a peer by name or id.
func (a *Agent) Connect(nameOrID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	peer, ok := resolvePeer(a.byID, a.byName, nameOrID)
	if !ok {
		return fmt.Errorf("no such peer: %s", nameOrID)
	}
	if err := a.writeJSON(protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: peer.ID}); err != nil {
		return fmt.Errorf("send connect_request: %w", err)
	}
	return nil
}

// Friends returns the current allowlist as base64 fingerprints (the same
// strings the user pastes when adding a friend).
func (a *Agent) Friends() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.friends))
	for _, k := range a.friends {
		out = append(out, base64.StdEncoding.EncodeToString(k))
	}
	return out
}

// syncWhitelistLocked rebuilds the effective tunnel whitelist from the current
// room members (roomFP), then pushes it to the tunnel. Callers hold a.mu; the
// tunnel only exists after registration. With no room, the whitelist is empty.
func (a *Agent) syncWhitelistLocked() {
	a.friends = dedupFP(a.roomFP)
	if a.tunnel != nil {
		a.tunnel.SetFriends(a.friends)
	}
}

// dedupFP concatenates fingerprint byte-slices, deduplicating by base64 so a
// fingerprint appears once.
func dedupFP(lists ...[][]byte) [][]byte {
	var out [][]byte
	seen := make(map[string]bool)
	for _, list := range lists {
		for _, k := range list {
			key := base64.StdEncoding.EncodeToString(k)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, k)
		}
	}
	return out
}

// ---- rooms ----

// RoomState returns the current room (nil when not in one). A copy is returned
// so callers can hold it without the agent lock.
func (a *Agent) RoomState() *RoomState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.room == nil {
		return nil
	}
	r := *a.room
	r.Members = append([]protocol.RoomMember(nil), a.room.Members...)
	return &r
}

// CreateRoom asks the server for a new room and joins it. The room code and
// member list arrive asynchronously (room_created + room_joined).
func (a *Agent) CreateRoom() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeJSON(protocol.TypeRoomCreate, struct{}{})
}

// JoinRoom asks the server to add us to the room with the given code.
func (a *Agent) JoinRoom(code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return fmt.Errorf("房间码不能为空")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeJSON(protocol.TypeRoomJoin, protocol.RoomJoin{Code: code})
}

// LeaveRoom asks the server to remove us from the current room.
func (a *Agent) LeaveRoom() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeJSON(protocol.TypeRoomLeave, struct{}{})
}

// setRoom installs the current room and whitelists its members (excluding us).
func (a *Agent) setRoom(rj *protocol.RoomJoined) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.room = &RoomState{Code: rj.Code, Members: rj.Members}
	a.roomFP = memberFPs(a.identity.Fingerprint(), rj.Members)
	a.syncWhitelistLocked()
}

// updateRoomMembers refreshes the member list of the current room.
func (a *Agent) updateRoomMembers(members []protocol.RoomMember) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.room == nil {
		return
	}
	a.room.Members = members
	a.roomFP = memberFPs(a.identity.Fingerprint(), members)
	a.syncWhitelistLocked()
}

// clearRoom leaves the current room: drop the room state, the room-sourced
// whitelist entries, and the peers' /32 routes (the server has already removed
// us from the room, so every room peer's route is now stale).
func (a *Agent) clearRoom() {
	var del []string
	a.mu.Lock()
	a.room = nil
	a.roomFP = nil
	del = make([]string, 0, len(a.routes))
	for vip := range a.routes {
		del = append(del, vip)
	}
	a.routes = make(map[string]string)
	a.syncWhitelistLocked()
	a.mu.Unlock()
	for _, vip := range del {
		a.removePeerRoute(vip)
	}
}

// memberFPs extracts the fingerprints of every member except self (matched by
// fingerprint, not display name).
func memberFPs(self string, members []protocol.RoomMember) [][]byte {
	var out [][]byte
	for _, m := range members {
		if m.KeyFP == self {
			continue
		}
		if raw, err := crypto.ParseFingerprint(m.KeyFP); err == nil {
			out = append(out, raw)
		}
	}
	return out
}

// messageLoop consumes server messages and drives the tunnel, virtual NIC,
// and LAN-discovery emulation. It runs until the connection dies.
func (a *Agent) messageLoop() {
	// Arm the read deadline and refresh it on every pong the server answers
	// with (gorilla auto-pongs our pings). If the link goes silent for
	// wsPongWait — a dead server or an idle tunnel drop — ReadJSON returns a
	// timeout and Run surfaces it so the caller reconnects.
	a.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	a.conn.SetPongHandler(func(string) error {
		a.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	for {
		var env protocol.Envelope
		if err := a.conn.ReadJSON(&env); err != nil {
			a.wsErr <- fmt.Errorf("server connection: %w", err)
			return
		}
		switch env.Type {
		case protocol.TypeRegistered:
			a.onRegistered(env)
		case protocol.TypePeersList:
			var list protocol.PeersList
			_ = json.Unmarshal(env.Data, &list)
			a.mergePeers(list.Peers)
			a.autoConnect(list.Peers)
		case protocol.TypeConnectCandidates:
			var cc protocol.ConnectCandidates
			_ = json.Unmarshal(env.Data, &cc)
			a.mu.Lock()
			t := a.tunnel
			a.mu.Unlock()
			if t != nil {
				t.BeginConnect(cc.PeerID, cc.PeerName, toUDPAddrs(cc.Candidates))
			}
		case protocol.TypeRoomCreated:
			// The server follows room_created with a room_joined for the creator.
		case protocol.TypeRoomJoined:
			var rj protocol.RoomJoined
			_ = json.Unmarshal(env.Data, &rj)
			a.setRoom(&rj)
		case protocol.TypeRoomUpdate:
			var ru protocol.RoomUpdate
			_ = json.Unmarshal(env.Data, &ru)
			a.updateRoomMembers(ru.Members)
		case protocol.TypeRoomLeft:
			// The server removed us from our room: drop the room state and the
			// room-sourced whitelist entries so we stop punching room members.
			a.clearRoom()
		case protocol.TypeError:
			var e protocol.Error
			_ = json.Unmarshal(env.Data, &e)
			a.mu.Lock()
			a.errNote = e.Message
			a.mu.Unlock()
			a.opts.Info("server error    : %s", e.Message)
		}
	}
}

// onRegistered handles the first server reply: it reports our endpoint, sets
// up the tunnel (with identity + friends), the virtual NIC, the LAN-discovery
// listener, and auto-connects to every online peer.
func (a *Agent) onRegistered(env protocol.Envelope) {
	var reg protocol.Registered
	_ = json.Unmarshal(env.Data, &reg)

	a.mu.Lock()
	a.myID = reg.ClientID
	a.myVIP = reg.VirtualIP
	a.errNote = ""
	a.opts.Info("registered      : id=%s virtual_ip=%s", reg.ClientID, reg.VirtualIP)

	tunnel := p2p.New(a.p2pConn, a.myID, a.opts.Logf)
	tunnel.SetIdentity(a.identity)
	tunnel.SetFriends(a.friends)
	go tunnel.Run()
	if reg.RelayAddr != "" {
		if err := tunnel.SetRelay(reg.RelayAddr); err != nil {
			a.opts.Logf("warning: bad relay addr %q: %v", reg.RelayAddr, err)
		} else {
			tunnel.Announce()
		}
	}
	if a.opts.ForceRelay {
		tunnel.SetForceRelay(true)
	}
	a.tunnel = tunnel
	a.mergePeersLocked(reg.Peers)
	a.mu.Unlock()

	// Decapsulated IP packets from peers go into the virtual NIC. A
	// LAN-discovery advertisement is delivered two ways (M5): as the original
	// multicast (reaching a joining client joined to the group on Wintun) and
	// as a unicast copy to our own virtual IP (reaching a client bound on
	// 0.0.0.0:4445 that never joined the group). A local socket write to the
	// group (LocalEmit) does not loop back on Windows, so it is not used.
	tunnel.SetDataSink(func(pkt []byte) {
		a.mu.Lock()
		adVnic := a.adVnic
		vip := a.myVIP
		a.mu.Unlock()
		if adVnic == nil {
			return
		}
		if a.opts.DebugPackets {
			a.opts.Logf("tunnel->vnic: %d B %s -> %s", len(pkt), lan.IPv4Src(pkt), lan.IPv4Dst(pkt))
		}
		if lan.IsDiscovery(pkt) && vip != "" {
			if err := adVnic.Write(pkt); err != nil {
				a.opts.Logf("vnic: write discovery: %v", err)
			}
			if err := adVnic.Write(lan.RewriteDest(pkt, net.ParseIP(vip))); err != nil {
				a.opts.Logf("vnic: write discovery unicast: %v", err)
			}
			return
		}
		if err := adVnic.Write(pkt); err != nil {
			a.opts.Logf("vnic: write: %v", err)
		}
	})

	if a.opts.LanEmu {
		// Listen binds UDP 4445 with SO_REUSEADDR and joins the discovery
		// group on every up interface (including the Wintun adapter). On the
		// hosting machine this sniffs the MC server's broadcast so it can be
		// re-broadcast through the tunnel; on a joining machine the adapter
		// injection path doubles back here and the virtual-subnet loop guard
		// drops it.
		_, err := lan.Listen(func(pkt []byte, src net.IP) {
			a.forwardDiscovery(true, src, pkt)
		})
		if err != nil {
			a.opts.Logf("warning: LAN discovery listener unavailable: %v", err)
		} else {
			a.opts.Info("lan discovery  : listening UDP %d/%s", lan.DiscoveryPort, lan.DiscoveryGroup)
		}
	}

	if a.opts.UseVnic {
		adapterName := a.opts.VnicName
		if adapterName == "" {
			adapterName = "Eliauk-" + a.opts.Name
		}
		adVnic, err := vnic.Open(adapterName, reg.VirtualIP, "255.255.255.0")
		if err != nil {
			a.mu.Lock()
			a.vnicErr = err.Error()
			a.mu.Unlock()
			a.opts.Logf("warning: virtual NIC unavailable: %v", err)
		} else {
			a.mu.Lock()
			a.adVnic = adVnic
			a.vnicErr = ""
			a.mu.Unlock()
			a.opts.Info("virtual NIC    : %s (%s)", adapterName, reg.VirtualIP)
			go func() {
				for {
					pkt, err := adVnic.Read()
					if err != nil {
						a.opts.Logf("vnic: read: %v", err)
						return
					}
					a.forwardFromVnic(pkt)
					adVnic.Release(pkt)
				}
			}()
		}
	}

	a.printPeers(reg.Peers)
	a.autoConnect(reg.Peers)
	a.reportEndpoint()
}

// forwardFromVnic routes one IP packet read off the virtual NIC to the peer
// that owns its destination virtual IP. LAN-discovery advertisements
// (broadcast/multicast to UDP 4445) are fanned out to every peer instead.
func (a *Agent) forwardFromVnic(pkt []byte) {
	if lan.IsDiscovery(pkt) {
		a.forwardDiscovery(false, net.ParseIP(lan.IPv4Src(pkt)), pkt)
		return
	}
	dst := lan.IPv4Dst(pkt)
	if dst == "" {
		return
	}
	a.mu.Lock()
	peerID, ok := a.routes[dst]
	t := a.tunnel
	a.mu.Unlock()
	if !ok || t == nil {
		return
	}
	if a.opts.DebugPackets {
		a.opts.Logf("vnic->tunnel: %d B %s -> %s to %s", len(pkt), lan.IPv4Src(pkt), dst, peerID)
	}
	if err := t.SendData(peerID, pkt); err != nil {
		// Peer not connected yet — drop; upper-layer retries will flow once
		// the auto-connect handshake completes.
	}
}

// forwardDiscovery fans a LAN-discovery advertisement out to every connected
// peer, carrying OUR virtual IP as the source so a joining MC client dials
// the host's virtual address (M5). sniffed=true means the listener delivered
// a raw UDP payload (wrapped by lan.BuildDiscovery); otherwise the packet is
// a full IP packet read off the virtual NIC (source rewritten). Any packet
// already carrying a virtual-subnet source is a peer echo or our own
// re-forward — it is dropped to prevent loops.
func (a *Agent) forwardDiscovery(sniffed bool, src net.IP, pkt []byte) {
	a.mu.Lock()
	t := a.tunnel
	vip := a.myVIP
	a.mu.Unlock()
	if t == nil || vip == "" {
		return
	}
	if lan.InVirtualSubnet(src) {
		return
	}
	virtual := net.ParseIP(vip)
	var out []byte
	if sniffed {
		out = lan.BuildDiscovery(virtual, pkt)
	} else {
		out = lan.RewriteSource(pkt, virtual)
	}
	if out == nil {
		return
	}
	t.SendDataBroadcast(out)
}

// ensurePeerRoute installs a /32 host route so traffic to a peer's virtual IP
// is forced into our virtual NIC (M5). Requires an elevated process, which
// wintun adapter creation already demands. Duplicate additions (route already
// present on reconnect) are harmless.
func (a *Agent) ensurePeerRoute(peerVIP string) {
	a.mu.Lock()
	adVnic := a.adVnic
	a.mu.Unlock()
	if adVnic == nil || peerVIP == "" {
		return
	}
	idx := adVnic.IfIndex()
	if idx == 0 {
		return
	}
	out, err := exec.Command("route", "add", peerVIP, "mask", "255.255.255.255",
		"0.0.0.0", "IF", fmt.Sprint(idx)).CombinedOutput()
	if err != nil {
		a.opts.Logf("route add %s via if%d: %v %s", peerVIP, idx, err, strings.TrimSpace(string(out)))
	}
}

// removePeerRoute deletes the /32 host route for a peer's virtual IP, undoing
// ensurePeerRoute when the peer leaves the room or the network. The add and
// delete agree on the interface index, which the Wintun adapter keeps stable
// for the lifetime of the process.
func (a *Agent) removePeerRoute(peerVIP string) {
	a.mu.Lock()
	adVnic := a.adVnic
	a.mu.Unlock()
	if adVnic == nil || peerVIP == "" {
		return
	}
	idx := adVnic.IfIndex()
	if idx == 0 {
		return
	}
	out, err := exec.Command("route", "delete", peerVIP, "mask", "255.255.255.255",
		"0.0.0.0", "IF", fmt.Sprint(idx)).CombinedOutput()
	if err != nil {
		a.opts.Logf("route delete %s via if%d: %v %s", peerVIP, idx, err, strings.TrimSpace(string(out)))
	}
}

// autoConnect updates the virtual-IP route table and sends a connect_request
// for every online peer we haven't already tried, so the virtual LAN is fully
// connected without manual `connect` commands.
func (a *Agent) autoConnect(peers []protocol.Peer) {
	var fresh []protocol.Peer
	var addRoute, delRoute []string
	a.mu.Lock()
	// Reconcile the route table against the current peer list: a peer that left
	// the room (or the network) drops out of the visible list, so its /32 route
	// has to go too — otherwise packets to its stale virtual IP are silently
	// dropped into the tunnel (M5 route-cleanup).
	want := make(map[string]string, len(peers))
	for _, p := range peers {
		if p.VirtualIP != "" {
			want[p.VirtualIP] = p.ID
		}
	}
	for vip, id := range want {
		if a.routes[vip] != id {
			a.routes[vip] = id
			addRoute = append(addRoute, vip)
		}
	}
	for vip, id := range a.routes {
		if want[vip] != id {
			delete(a.routes, vip)
			delRoute = append(delRoute, vip)
		}
	}
	for _, p := range peers {
		// Only auto-connect to peers that have reported a punchable endpoint —
		// otherwise the server rejects the connect_request and marking the peer
		// attempted here would prevent a later retry.
		if p.PublicIP != "" && !a.attempted[p.ID] {
			a.attempted[p.ID] = true
			fresh = append(fresh, p)
		}
	}
	a.mu.Unlock()
	for _, vip := range addRoute {
		a.ensurePeerRoute(vip)
	}
	for _, vip := range delRoute {
		a.removePeerRoute(vip)
	}
	for _, p := range fresh {
		if err := a.writeJSON(protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: p.ID}); err != nil {
			a.opts.Logf("warning: auto-connect %s: %v", p.Name, err)
		}
	}
}

// reportEndpoint tells the server our public endpoint and punch candidates
// for this socket, so peers can reach us.
func (a *Agent) reportEndpoint() {
	ep := protocol.ReportEndpoint{
		NATType:    string(a.probe.NAT),
		Candidates: gatherCandidates(a.probe, a.p2pConn),
	}
	if a.probe.Mapped.IP != nil {
		ep.PublicIP = a.probe.Mapped.IP.String()
		ep.PublicPort = a.probe.Mapped.Port
	}
	if err := a.writeJSON(protocol.TypeReportEndpoint, ep); err != nil {
		a.opts.Logf("warning: report endpoint: %v", err)
	}
}

// mergePeers merges a peer list into the byID/byName indexes (with lock).
func (a *Agent) mergePeers(peers []protocol.Peer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mergePeersLocked(peers)
}

func (a *Agent) mergePeersLocked(peers []protocol.Peer) {
	for _, p := range peers {
		a.byID[p.ID] = p
		a.byName[p.Name] = p
	}
}

// printPeers writes the online peer table via Info.
func (a *Agent) printPeers(peers []protocol.Peer) {
	if len(peers) == 0 {
		a.opts.Info("peers           : (none online)")
		return
	}
	a.opts.Info("peers           :")
	for _, p := range peers {
		a.opts.Info("  - %-12s %-12s %s (%s)", p.Name, p.VirtualIP, endpointString(p), p.NATType)
	}
}

// DefaultKeyfile returns the default identity key path for this user. On
// Windows this is %AppData%\Eliauk\identity.key.
func DefaultKeyfile() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "Eliauk", "identity.key")
	}
	return "identity.key"
}

// ---- small helpers shared with the CLI ----

func stunProbeOn(conn *net.UDPConn, primaryHost, secondaryHost string) *stun.Result {
	primary, err := net.ResolveUDPAddr("udp", primaryHost)
	if err != nil {
		log.Printf("warning: resolve primary STUN %q: %v", primaryHost, err)
		return &stun.Result{NAT: stun.NATUnreachable}
	}
	secondary, err := net.ResolveUDPAddr("udp", secondaryHost)
	if err != nil {
		log.Printf("warning: resolve secondary STUN %q: %v", secondaryHost, err)
		secondary = nil
	}
	probe, err := stun.DetectOn(conn, primary, secondary, 3*time.Second)
	if err != nil {
		log.Printf("warning: STUN probe failed: %v", err)
		return &stun.Result{NAT: stun.NATUnreachable}
	}
	return probe
}

func probeString(probe *stun.Result) string {
	if probe.Mapped.IP == nil {
		return "(unreachable)"
	}
	return fmt.Sprintf("%s", probe.Mapped)
}

// gatherCandidates collects every punchable address for this socket: the
// public mapping from STUN first, then each LAN interface address using the
// socket's local port.
func gatherCandidates(probe *stun.Result, p2pConn *net.UDPConn) []protocol.Candidate {
	var cands []protocol.Candidate
	if probe.Mapped.IP != nil {
		cands = append(cands, protocol.Candidate{
			IP: probe.Mapped.IP.String(), Port: probe.Mapped.Port, Type: "public",
		})
	}
	localPort := p2pConn.LocalAddr().(*net.UDPAddr).Port
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		cands = append(cands, protocol.Candidate{
			IP: ip4.String(), Port: localPort, Type: "lan",
		})
	}
	return cands
}

func toUDPAddrs(cands []protocol.Candidate) []*net.UDPAddr {
	var out []*net.UDPAddr
	for _, c := range cands {
		if ip := net.ParseIP(c.IP); ip != nil {
			out = append(out, &net.UDPAddr{IP: ip, Port: c.Port})
		}
	}
	return out
}

func resolvePeer(byID, byName map[string]protocol.Peer, key string) (protocol.Peer, bool) {
	if p, ok := byID[key]; ok {
		return p, true
	}
	if p, ok := byName[key]; ok {
		return p, true
	}
	return protocol.Peer{}, false
}

// writeJSON serializes and writes an envelope under the write lock so user
// actions, auto-connect and the heartbeat ping never interleave (gorilla allows
// only one concurrent writer).
func (a *Agent) writeJSON(typ string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.conn.WriteJSON(protocol.Envelope{Type: typ, Data: raw})
}

// pingLoop sends a periodic WebSocket ping to keep an idle coordination link
// alive (e.g. through Cloudflare, which drops idle tunnels after ~100s) and to
// detect a dead peer. A failed write tears the connection down via wsErr so Run
// returns and the caller reconnects.
func (a *Agent) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.writeMu.Lock()
			err := a.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait))
			a.writeMu.Unlock()
			if err != nil {
				select {
				case a.wsErr <- err:
				default:
				}
				return
			}
		}
	}
}

func send(conn *websocket.Conn, typ string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return conn.WriteJSON(protocol.Envelope{Type: typ, Data: raw})
}

func endpointString(p protocol.Peer) string {
	if p.PublicIP == "" {
		return "no endpoint yet"
	}
	return fmt.Sprintf("%s:%d", p.PublicIP, p.PublicPort)
}
