// Command client is the Eliauk VPN agent that runs on each player's machine.
//
// M2 scope:
//   - register with the coordination server
//   - run a STUN probe on the P2P socket (its mapping is what we punch with)
//   - report punch candidates (public + LAN)
//   - interactive CLI: punch to a peer and establish a direct UDP tunnel
package main

import (
	"bufio"
	"encoding/json"
	"flag"
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

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		name          = flag.String("name", "", "your display name (required)")
		serverAddr    = flag.String("server", "ws://127.0.0.1:8080/ws", "coordination server WebSocket URL")
		stunPrimary   = flag.String("stun-primary", "stun.l.google.com:19302", "primary STUN server")
		stunSecondary = flag.String("stun-secondary", "stun.cloudflare.com:3478", "secondary STUN server (symmetry detection)")
		forceRelay    = flag.Bool("force-relay", false, "skip direct punching, always relay via server (testing / symmetric NAT)")
		useVnic       = flag.Bool("vnic", true, "create and use the Wintun virtual NIC")
		vnicName      = flag.String("vnic-name", "", "virtual NIC adapter name (default Eliauk-<name>)")
		lanEmu        = flag.Bool("lan", true, "emulate Minecraft LAN discovery (UDP 4445 broadcast fan-out)")
		headless      = flag.Bool("headless", false, "run as a background agent without the interactive command loop")
		debugPackets  = flag.Bool("debug-packets", false, "log every packet flowing between the virtual NIC and the tunnel")
		keyfile       = flag.String("keyfile", defaultKeyfile(), "path to the X25519 identity key (created on first run)")
	)
	flag.Parse()
	if *name == "" {
		return fmt.Errorf("--name is required (e.g. --name alice)")
	}

	// 0. Load (or create) the long-term X25519 identity. This authenticates the
	//    handshake to friends and encrypts all tunnel data (M6). Share the
	//    fingerprint printed below so friends can whitelist us.
	identity, err := crypto.LoadOrCreate(*keyfile)
	if err != nil {
		return fmt.Errorf("load identity %q: %w", *keyfile, err)
	}
	fmt.Printf("identity        : %s\n", identity.Fingerprint())

	// 1. Open the P2P socket FIRST — hole punching must use the same socket
	//    whose public mapping we advertise, so the STUN probe runs on it.
	p2pConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("open p2p socket: %w", err)
	}
	defer p2pConn.Close()

	probe := stunProbeOn(p2pConn, *stunPrimary, *stunSecondary)
	fmt.Printf("public endpoint : %s\n", probe.Mapped)
	fmt.Printf("NAT type        : %s\n", probe.NAT)

	// 2. Register with the coordination server.
	conn, _, err := websocket.DefaultDialer.Dial(*serverAddr, nil)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	defer conn.Close()

	if err := send(conn, protocol.TypeRegister, protocol.RegisterRequest{Name: *name}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	var (
		mu        sync.Mutex
		myID      string
		myVIP     string // our assigned virtual IP (M5: discovery source rewrite)
		tunnel    *p2p.Tunnel
		adVnic    *vnic.Adapter
		routes    = make(map[string]string) // virtual ip -> peer id (M4)
		attempted = make(map[string]bool)   // peer ids we auto-connected to
		byID      = make(map[string]protocol.Peer)
		byName    = make(map[string]protocol.Peer)
	)

	// ensurePeerRoute installs a /32 host route so traffic to a peer's virtual
	// IP is forced into our virtual NIC (M5). Requires an elevated process,
	// which wintun adapter creation already demands. Duplicate additions (route
	// already present on reconnect) are harmless.
	ensurePeerRoute := func(peerVIP string) {
		mu.Lock()
		a := adVnic
		mu.Unlock()
		if a == nil || peerVIP == "" {
			return
		}
		idx := a.IfIndex()
		if idx == 0 {
			return
		}
		out, err := exec.Command("route", "add", peerVIP, "mask", "255.255.255.255",
			"0.0.0.0", "IF", fmt.Sprint(idx)).CombinedOutput()
		if err != nil {
			log.Printf("route add %s via if%d: %v %s", peerVIP, idx, err, strings.TrimSpace(string(out)))
		}
	}

	// autoConnect updates the virtual-IP route table and sends a
	// connect_request for every online peer we haven't already tried, so the
	// virtual LAN is fully connected without manual `connect` commands.
	autoConnect := func(peers []protocol.Peer) {
		var fresh []protocol.Peer
		var routeVIPs []string
		mu.Lock()
		for _, p := range peers {
			if p.VirtualIP != "" {
				routes[p.VirtualIP] = p.ID
				routeVIPs = append(routeVIPs, p.VirtualIP)
			}
			// Only auto-connect to peers that have reported a punchable
			// endpoint — otherwise the server rejects the connect_request and
			// marking the peer attempted here would prevent a later retry.
			if p.PublicIP != "" && !attempted[p.ID] {
				attempted[p.ID] = true
				fresh = append(fresh, p)
			}
		}
		mu.Unlock()
		// Force traffic to each peer's virtual IP into our virtual NIC; without
		// the /32 host route the OS would pick an arbitrary interface for the
		// 10.0.0.0/24 destination (often the real NIC or another VPN adapter)
		// and the packet never reaches the wintun read loop.
		for _, vip := range routeVIPs {
			ensurePeerRoute(vip)
		}
		for _, p := range fresh {
			if err := send(conn, protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: p.ID}); err != nil {
				log.Printf("warning: auto-connect %s: %v", p.Name, err)
			}
		}
	}

	// forwardDiscovery fans a LAN-discovery advertisement out to every connected
	// peer, carrying OUR virtual IP as the source so a joining MC client dials
	// the host's virtual address (M5). It is reached from two places:
	//
	//   - sniffed (true): the local discovery listener delivered the raw UDP
	//     datagram — payload only, no IP header. It is wrapped in a fresh
	//     IPv4/UDP packet addressed to the discovery group and sourced from our
	//     virtual IP (lan.RewriteSource would no-op: it needs a full IP packet).
	//   - sniffed (false): a full IP packet read off the virtual NIC that is a
	//     discovery advertisement, e.g. a broadcast a local server routed into
	//     the adapter. Its source is rewritten to our virtual IP.
	//
	// Loop guard: any packet already carrying a virtual-subnet source is either
	// one we forwarded or a peer's echo — re-forwarding would loop, so it is
	// dropped regardless of which path it came in on.
	forwardDiscovery := func(sniffed bool, src net.IP, pkt []byte) {
		mu.Lock()
		t := tunnel
		vip := myVIP
		mu.Unlock()
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

	// forwardFromVnic routes one IP packet read off the virtual NIC to the
	// peer that owns its destination virtual IP. LAN-discovery advertisements
	// (broadcast/multicast to UDP 4445) are fanned out to every peer instead.
	forwardFromVnic := func(pkt []byte) {
		if lan.IsDiscovery(pkt) {
			forwardDiscovery(false, net.ParseIP(lan.IPv4Src(pkt)), pkt)
			return
		}
		dst := lan.IPv4Dst(pkt)
		if dst == "" {
			return
		}
		mu.Lock()
		peerID, ok := routes[dst]
		t := tunnel
		mu.Unlock()
		if !ok || t == nil {
			return
		}
		if *debugPackets {
			log.Printf("vnic->tunnel: %d B %s -> %s to %s", len(pkt), lan.IPv4Src(pkt), dst, peerID)
		}
		if err := t.SendData(peerID, pkt); err != nil {
			// Peer not connected yet — drop; upper-layer retries will flow
			// once the auto-connect handshake completes.
		}
	}

	// 3. WebSocket message loop.
	wsErr := make(chan error, 1)
	go func() {
		for {
			var env protocol.Envelope
			if err := conn.ReadJSON(&env); err != nil {
				wsErr <- fmt.Errorf("server connection: %w", err)
				return
			}
			switch env.Type {
			case protocol.TypeRegistered:
				var reg protocol.Registered
				_ = json.Unmarshal(env.Data, &reg)
				myID = reg.ClientID
				fmt.Printf("registered      : id=%s virtual_ip=%s\n", reg.ClientID, reg.VirtualIP)
				mu.Lock()
				myVIP = reg.VirtualIP
				tunnel = p2p.New(p2pConn, myID, log.Printf)
				tunnel.SetIdentity(identity)
				go tunnel.Run()
				if reg.RelayAddr != "" {
					if err := tunnel.SetRelay(reg.RelayAddr); err != nil {
						log.Printf("warning: bad relay addr %q: %v", reg.RelayAddr, err)
					} else {
						tunnel.Announce()
					}
				}
				if *forceRelay {
					tunnel.SetForceRelay(true)
				}
				// Decapsulated IP packets from peers go into the virtual NIC.
				// A LAN-discovery advertisement is delivered two ways (M5):
				//   - as the original multicast, reaching a joining client that
				//     joined the group on the Wintun interface;
				//   - as a unicast copy to our own virtual IP, reaching a client
				//     bound on 0.0.0.0:4445 even if it never joined the group on
				//     Wintun (which reports no multicast flag).
				// A local socket write to the group (LocalEmit) does NOT loop
				// back on Windows, so it is not used.
				tunnel.SetDataSink(func(pkt []byte) {
					mu.Lock()
					a := adVnic
					vip := myVIP
					mu.Unlock()
					if a == nil {
						return
					}
					if *debugPackets {
						log.Printf("tunnel->vnic: %d B %s -> %s", len(pkt), lan.IPv4Src(pkt), lan.IPv4Dst(pkt))
					}
					if lan.IsDiscovery(pkt) && vip != "" {
						if err := a.Write(pkt); err != nil {
							log.Printf("vnic: write discovery: %v", err)
						}
						if err := a.Write(lan.RewriteDest(pkt, net.ParseIP(vip))); err != nil {
							log.Printf("vnic: write discovery unicast: %v", err)
						}
						return
					}
					if err := a.Write(pkt); err != nil {
						log.Printf("vnic: write: %v", err)
					}
				})
				mergePeers(byID, byName, reg.Peers)
				mu.Unlock()

				if *lanEmu {
					// Listen binds UDP 4445 with SO_REUSEADDR and joins the
					// discovery group on every up interface (including the
					// Wintun adapter). On the hosting machine this sniffs the
					// MC server's broadcast so it can be re-broadcast through
					// the tunnel; on a joining machine the adapter injection
					// path doubles back here and the virtual-subnet loop guard
					// drops it.
					_, err := lan.Listen(func(pkt []byte, src net.IP) {
						forwardDiscovery(true, src, pkt)
					})
					if err != nil {
						log.Printf("warning: LAN discovery listener unavailable: %v", err)
					} else {
						fmt.Printf("lan discovery  : listening UDP %d/%s\n", lan.DiscoveryPort, lan.DiscoveryGroup)
					}
				}

				if *useVnic {
					adapterName := *vnicName
					if adapterName == "" {
						adapterName = "Eliauk-" + *name
					}
					a, err := vnic.Open(adapterName, reg.VirtualIP, "255.255.255.0")
					if err != nil {
						log.Printf("warning: virtual NIC unavailable: %v", err)
					} else {
						mu.Lock()
						adVnic = a
						mu.Unlock()
						fmt.Printf("virtual NIC    : %s (%s)\n", adapterName, reg.VirtualIP)
						go func() {
							for {
								pkt, err := a.Read()
								if err != nil {
									log.Printf("vnic: read: %v", err)
									return
								}
								forwardFromVnic(pkt)
								a.Release(pkt)
							}
						}()
					}
				}

				printPeers(reg.Peers)
				autoConnect(reg.Peers)
				reportEndpoint(conn, probe, p2pConn)
			case protocol.TypePeersList:
				var list protocol.PeersList
				_ = json.Unmarshal(env.Data, &list)
				printPeers(list.Peers)
				mu.Lock()
				mergePeers(byID, byName, list.Peers)
				mu.Unlock()
				autoConnect(list.Peers)
			case protocol.TypeConnectCandidates:
				var cc protocol.ConnectCandidates
				_ = json.Unmarshal(env.Data, &cc)
				mu.Lock()
				if tunnel != nil {
					tunnel.BeginConnect(cc.PeerID, cc.PeerName, toUDPAddrs(cc.Candidates))
				}
				mu.Unlock()
			case protocol.TypeError:
				var e protocol.Error
				_ = json.Unmarshal(env.Data, &e)
				fmt.Printf("server error    : %s\n", e.Message)
			}
		}
	}()

	// 4. Run the agent. Interactive mode reads commands from stdin; headless
	// mode just keeps the tunnel running until the server connection fails
	// (used for background/automation deployment).
	if *headless {
		for {
			if err := <-wsErr; err != nil {
				return err
			}
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("commands: peers | connect <name|id> | status | quit")
	for {
		select {
		case err := <-wsErr:
			return err
		default:
		}
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "quit", "exit":
			return nil
		case "peers":
			mu.Lock()
			peers := make([]protocol.Peer, 0, len(byID))
			for _, p := range byID {
				peers = append(peers, p)
			}
			mu.Unlock()
			printPeers(peers)
		case "connect":
			if len(fields) < 2 {
				fmt.Println("usage: connect <name|id>")
				continue
			}
			mu.Lock()
			peer, ok := resolvePeer(byID, byName, fields[1])
			t := tunnel
			mu.Unlock()
			if !ok {
				fmt.Printf("no such peer: %s\n", fields[1])
				continue
			}
			if t == nil {
				fmt.Println("not registered yet")
				continue
			}
			if err := send(conn, protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: peer.ID}); err != nil {
				fmt.Printf("send connect_request: %v\n", err)
				continue
			}
			fmt.Printf("requesting p2p connection to %s (%s)...\n", peer.Name, peer.ID)
		case "status":
			mu.Lock()
			t := tunnel
			mu.Unlock()
			if t == nil {
				fmt.Println("not registered yet")
				continue
			}
			snaps := t.Snapshot()
			if len(snaps) == 0 {
				fmt.Println("  (no connections yet)")
			}
			for _, s := range snaps {
				fmt.Printf("  %-12s %-10s %s\n", s.Name, s.State, s.Remote)
			}
		default:
			fmt.Println("unknown command")
		}
	}
	return scanner.Err()
}

// defaultKeyfile returns the default identity key path for this user. On
// Windows this is %AppData%\Eliauk\identity.key.
func defaultKeyfile() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "Eliauk", "identity.key")
	}
	return "identity.key"
}

// stunProbeOn runs NAT detection on an existing socket, degrading gracefully
// if STUN is unreachable.
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

// reportEndpoint tells the server our public endpoint and punch candidates
// for this socket, so peers can reach us.
func reportEndpoint(conn *websocket.Conn, probe *stun.Result, p2pConn *net.UDPConn) {
	ep := protocol.ReportEndpoint{
		NATType:    string(probe.NAT),
		Candidates: gatherCandidates(probe, p2pConn),
	}
	if probe.Mapped.IP != nil {
		ep.PublicIP = probe.Mapped.IP.String()
		ep.PublicPort = probe.Mapped.Port
	}
	if err := send(conn, protocol.TypeReportEndpoint, ep); err != nil {
		log.Printf("warning: report endpoint: %v", err)
	}
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

func mergePeers(byID, byName map[string]protocol.Peer, peers []protocol.Peer) {
	for _, p := range peers {
		byID[p.ID] = p
		byName[p.Name] = p
	}
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

func printPeers(peers []protocol.Peer) {
	if len(peers) == 0 {
		fmt.Println("peers           : (none online)")
		return
	}
	fmt.Println("peers           :")
	for _, p := range peers {
		fmt.Printf("  - %-12s %-12s %s (%s)\n", p.Name, p.VirtualIP, endpointString(p), p.NATType)
	}
}

func endpointString(p protocol.Peer) string {
	if p.PublicIP == "" {
		return "no endpoint yet"
	}
	return fmt.Sprintf("%s:%d", p.PublicIP, p.PublicPort)
}
