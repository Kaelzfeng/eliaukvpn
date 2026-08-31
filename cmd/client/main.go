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
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/p2p"
	"eliaukvpn/internal/protocol"
	"eliaukvpn/internal/stun"
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
	)
	flag.Parse()
	if *name == "" {
		return fmt.Errorf("--name is required (e.g. --name alice)")
	}

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
		mu     sync.Mutex
		myID   string
		tunnel *p2p.Tunnel
		byID   = make(map[string]protocol.Peer)
		byName = make(map[string]protocol.Peer)
	)

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
				tunnel = p2p.New(p2pConn, myID, log.Printf)
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
				mergePeers(byID, byName, reg.Peers)
				mu.Unlock()
				printPeers(reg.Peers)
				reportEndpoint(conn, probe, p2pConn)
			case protocol.TypePeersList:
				var list protocol.PeersList
				_ = json.Unmarshal(env.Data, &list)
				printPeers(list.Peers)
				mu.Lock()
				mergePeers(byID, byName, list.Peers)
				mu.Unlock()
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

	// 4. Interactive command loop.
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
