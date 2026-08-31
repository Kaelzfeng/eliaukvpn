// Package agent runs the Eliauk VPN client core: identity/friends loading,
// P2P socket + STUN probe, coordination-server registration, tunnel/virtual
// NIC/LAN-discovery setup, and automatic peer connection. It is shared by the
// interactive CLI (cmd/client) and the Windows tray GUI (cmd/gui), so both
// present the same agent underneath.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	FriendsFile   string
	// Friends is the allowlist as raw 32-byte X25519 public keys, used by the
	// GUI (which manages friends in its config) instead of a friends file.
	// If FriendsFile is set it takes precedence.
	Friends [][]byte
	// M7 account login. When Account is set the agent registers an account
	// client: it sends the username plus either a password (first login or
	// account creation) or a cached session token (Password empty), and binds
	// its X25519 fingerprint to the account. Create=true registers a brand-new
	// account. An empty Account keeps the legacy anonymous mode. The fresh
	// session token comes back in Registered.Token and should be cached so the
	// next start needs no password.
	Account  string
	Password string
	Token    string
	Create   bool
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
	FriendCt   int
	Registered bool
	// VnicMsg is non-empty when the virtual NIC could not be created (the
	// process was not elevated, the Wintun driver is missing, etc.). The GUI
	// shows it prominently instead of silently continuing without a NIC.
	VnicMsg string
	// M7: the logged-in account username ("" for legacy anonymous clients),
	// the last server error (auth failures, etc.) and the room code if in one.
	Account string
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
	// friends is the *effective* tunnel whitelist, recomputed as the union of
	// baseFP (legacy config friends), the M7 account friend directory (byUser)
	// and the current room members (roomFP).
	friends  [][]byte
	baseFP   [][]byte // legacy config friends (from Options / friends file)
	roomFP   [][]byte // fingerprints of the room members (M7b)
	probe    *stun.Result
	p2pConn  *net.UDPConn

	// M7 account state.
	myAccount string                     // my username ("" = legacy)
	myToken   string                     // fresh session token from the server
	myKeyFP   string                     // fingerprint the server bound to my account
	byUser    map[string]protocol.Friend // account friend directory (username -> friend)
	room      *RoomState                 // current room, nil when not in one
	errNote   string                     // last server error, cleared on registration

	vnicErr string

	wsErr chan error
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
	//    the handshake to friends and encrypts all tunnel data (M6). Share the
	//    fingerprint below so friends can whitelist us.
	identity, err := crypto.LoadOrCreate(opts.Keyfile)
	if err != nil {
		return nil, fmt.Errorf("load identity %q: %w", opts.Keyfile, err)
	}
	opts.Info("identity        : %s", identity.Fingerprint())

	// 0b. Load the friends allowlist (M6b): only peers whose static keys are
	//     listed here may establish a session. Each line is one base64
	//     fingerprint copied from a friend's "identity:" line.
	var friends [][]byte
	if opts.FriendsFile != "" {
		f, err := os.Open(opts.FriendsFile)
		if err != nil {
			return nil, fmt.Errorf("open friends list: %w", err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			raw, err := crypto.ParseFingerprint(line)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("friends list %q line %q: %w", opts.FriendsFile, line, err)
			}
			friends = append(friends, raw)
		}
		if err := sc.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("read friends list: %w", err)
		}
		f.Close()
		opts.Info("friends         : %d allowed", len(friends))
	} else if len(opts.Friends) > 0 {
		// GUI mode: friends come from the config, already parsed.
		friends = append(friends, opts.Friends...)
		opts.Info("friends         : %d allowed", len(friends))
	}

	// 1. Open the P2P socket FIRST — hole punching must use the same socket
	//    whose public mapping we advertise, so the STUN probe runs on it.
	p2pConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("open p2p socket: %w", err)
	}
	probe := stunProbeOn(p2pConn, opts.StunPrimary, opts.StunSecondary)
	opts.Info("public endpoint : %s", probeString(probe))
	opts.Info("NAT type        : %s", probe.NAT)

	// 2. Register with the coordination server. M7 account clients authenticate
	//    with a username plus a password (first login / creation) or a cached
	//    session token, and bind their identity fingerprint to the account.
	conn, _, err := websocket.DefaultDialer.Dial(opts.Server, nil)
	if err != nil {
		p2pConn.Close()
		return nil, fmt.Errorf("connect to server: %w", err)
	}
	regReq := protocol.RegisterRequest{
		Name:      opts.Name,
		PublicKey: identity.Fingerprint(),
		Account:   opts.Account,
		Password:  opts.Password,
		Token:     opts.Token,
		Create:    opts.Create,
	}
	if regReq.Name == "" {
		regReq.Name = regReq.Account
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
		identity:   identity,
		friends:    friends,
		baseFP:     friends,
		myAccount:  opts.Account,
		myToken:    opts.Token,
		byUser:     make(map[string]protocol.Friend),
		probe:      probe,
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
	go a.messageLoop()
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
		FriendCt:   len(a.friends),
		Registered: a.myID != "",
		Account:    a.myAccount,
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
	if err := send(a.conn, protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: peer.ID}); err != nil {
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

// AddFriend validates and appends a fingerprint to the legacy config allowlist
// (baseFP), then re-syncs the tunnel's whitelist immediately (M6b). It is
// idempotent: adding an existing friend is a no-op. Safe to call before
// registration — the tunnel picks the updated list up when it is created.
func (a *Agent) AddFriend(fp string) error {
	raw, err := crypto.ParseFingerprint(fp)
	if err != nil {
		return fmt.Errorf("invalid friend code: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, k := range a.baseFP {
		if bytes.Equal(k, raw) {
			return nil // already a friend
		}
	}
	a.baseFP = append(a.baseFP, raw)
	a.syncWhitelistLocked()
	if a.opts.Info != nil {
		a.opts.Info("friends         : %d allowed", len(a.friends))
	}
	return nil
}

// RemoveFriend deletes a fingerprint from the legacy config allowlist and
// re-syncs the tunnel. An already-established session is not torn down (the
// whitelist is enforced at handshake time); future connections are refused.
func (a *Agent) RemoveFriend(fp string) error {
	raw, err := crypto.ParseFingerprint(fp)
	if err != nil {
		return fmt.Errorf("invalid friend code: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, k := range a.baseFP {
		if bytes.Equal(k, raw) {
			a.baseFP = append(a.baseFP[:i], a.baseFP[i+1:]...)
			a.syncWhitelistLocked()
			if a.opts.Info != nil {
				a.opts.Info("friends         : %d allowed", len(a.friends))
			}
			return nil
		}
	}
	return fmt.Errorf("friend not in list")
}

// syncWhitelistLocked rebuilds the effective tunnel whitelist as the union of
// the legacy config friends (baseFP), the M7 account friend directory (byUser)
// and the current room members (roomFP), then pushes it to the tunnel. Callers
// hold a.mu; the tunnel only exists after registration.
func (a *Agent) syncWhitelistLocked() {
	a.friends = dedupFP(append(append(append([][]byte(nil), a.baseFP...), a.directoryFPsLocked()...), a.roomFP...))
	if a.tunnel != nil {
		a.tunnel.SetFriends(a.friends)
	}
}

// directoryFPsLocked returns the fingerprints of every friend in the M7
// account directory (empty KeyFP entries — friends who never logged in with an
// identity — are skipped). Callers hold a.mu.
func (a *Agent) directoryFPsLocked() [][]byte {
	var out [][]byte
	for _, f := range a.byUser {
		if raw, err := crypto.ParseFingerprint(f.KeyFP); err == nil {
			out = append(out, raw)
		}
	}
	return out
}

// dedupFP concatenates fingerprint byte-slices, deduplicating by base64 so a
// fingerprint that is both a friend and a room member appears once.
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

// ---- M7 account directory & rooms ----

// Account returns the logged-in username ("" for legacy anonymous clients).
func (a *Agent) Account() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.myAccount
}

// Token returns the session token from the last successful registration. Cache
// it so the next start can log in without a password.
func (a *Agent) Token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.myToken
}

// FriendDirectory returns the account friend directory with presence, sorted
// by username. Empty for legacy anonymous clients.
func (a *Agent) FriendDirectory() []protocol.Friend {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]protocol.Friend, 0, len(a.byUser))
	for _, f := range a.byUser {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// AddFriendByName asks the server to add a friend by username (symmetric on
// the server). The friend directory refresh arrives asynchronously.
func (a *Agent) AddFriendByName(username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("用户名不能为空")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.myAccount == "" {
		return fmt.Errorf("请先登录账号")
	}
	return send(a.conn, protocol.TypeFriendAdd, protocol.FriendAdd{Username: username})
}

// RemoveFriendByName asks the server to drop a friend by username (symmetric).
func (a *Agent) RemoveFriendByName(username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("用户名不能为空")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.myAccount == "" {
		return fmt.Errorf("请先登录账号")
	}
	return send(a.conn, protocol.TypeFriendRemove, protocol.FriendRemove{Username: username})
}

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
	if a.myAccount == "" {
		return fmt.Errorf("请先登录账号")
	}
	return send(a.conn, protocol.TypeRoomCreate, struct{}{})
}

// JoinRoom asks the server to add us to the room with the given code.
func (a *Agent) JoinRoom(code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return fmt.Errorf("房间码不能为空")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.myAccount == "" {
		return fmt.Errorf("请先登录账号")
	}
	return send(a.conn, protocol.TypeRoomJoin, protocol.RoomJoin{Code: code})
}

// LeaveRoom asks the server to remove us from the current room.
func (a *Agent) LeaveRoom() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.myAccount == "" {
		return fmt.Errorf("请先登录账号")
	}
	return send(a.conn, protocol.TypeRoomLeave, struct{}{})
}

// setFriendDirectory replaces the whole account friend directory (full refresh
// after registration, a friend add/remove) and re-syncs the whitelist.
func (a *Agent) setFriendDirectory(friends []protocol.Friend) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.byUser = make(map[string]protocol.Friend, len(friends))
	for _, f := range friends {
		a.byUser[f.Username] = f
	}
	a.syncWhitelistLocked()
}

// upsertFriend merges one resolved friend into the directory (the server sends
// friend_add_ok right before a full friend_list refresh, so the UI updates
// immediately).
func (a *Agent) upsertFriend(f protocol.Friend) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if f.Username == "" {
		return
	}
	a.byUser[f.Username] = f
	a.syncWhitelistLocked()
}

// setFriendPresence updates one friend's online flag.
func (a *Agent) setFriendPresence(user string, online bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if f, ok := a.byUser[user]; ok {
		f.Online = online
		a.byUser[user] = f
	}
}

// setRoom installs the current room and whitelists its members (excluding us).
func (a *Agent) setRoom(rj *protocol.RoomJoined) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.room = &RoomState{Code: rj.Code, Members: rj.Members}
	a.roomFP = memberFPs(a.myAccount, rj.Members)
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
	a.roomFP = memberFPs(a.myAccount, members)
	a.syncWhitelistLocked()
}

// memberFPs extracts the fingerprints of every member except self.
func memberFPs(self string, members []protocol.RoomMember) [][]byte {
	var out [][]byte
	for _, m := range members {
		if m.Username == self {
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
		case protocol.TypeFriendList:
			var fl protocol.FriendList
			_ = json.Unmarshal(env.Data, &fl)
			a.setFriendDirectory(fl.Friends)
		case protocol.TypeFriendAddOk:
			var ok protocol.FriendAddOk
			_ = json.Unmarshal(env.Data, &ok)
			a.upsertFriend(ok.Friend)
		case protocol.TypePresence:
			var p protocol.Presence
			_ = json.Unmarshal(env.Data, &p)
			a.setFriendPresence(p.Username, p.Online)
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
	if reg.Account != "" {
		// M7: bind the account, cache the fresh session token (so the next
		// start needs no password) and load the friend directory with presence.
		a.myAccount = reg.Account
		a.myToken = reg.Token
		a.myKeyFP = reg.KeyFP
		a.byUser = make(map[string]protocol.Friend, len(reg.Friends))
		for _, f := range reg.Friends {
			a.byUser[f.Username] = f
		}
		if reg.Room != "" {
			a.room = &RoomState{Code: reg.Room}
		}
		// Recompute the whitelist so the directory and any room members are
		// present before the tunnel is created below.
		a.syncWhitelistLocked()
	}
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

// autoConnect updates the virtual-IP route table and sends a connect_request
// for every online peer we haven't already tried, so the virtual LAN is fully
// connected without manual `connect` commands.
func (a *Agent) autoConnect(peers []protocol.Peer) {
	var fresh []protocol.Peer
	var routeVIPs []string
	a.mu.Lock()
	for _, p := range peers {
		if p.VirtualIP != "" {
			a.routes[p.VirtualIP] = p.ID
			routeVIPs = append(routeVIPs, p.VirtualIP)
		}
		// Only auto-connect to peers that have reported a punchable endpoint —
		// otherwise the server rejects the connect_request and marking the peer
		// attempted here would prevent a later retry.
		if p.PublicIP != "" && !a.attempted[p.ID] {
			a.attempted[p.ID] = true
			fresh = append(fresh, p)
		}
	}
	a.mu.Unlock()
	for _, vip := range routeVIPs {
		a.ensurePeerRoute(vip)
	}
	for _, p := range fresh {
		if err := send(a.conn, protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: p.ID}); err != nil {
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
	if err := send(a.conn, protocol.TypeReportEndpoint, ep); err != nil {
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
