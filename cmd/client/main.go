// Command client is the Eliauk VPN agent that runs on each player's machine.
//
// M1 scope:
//   - register with the coordination server
//   - run a STUN probe to discover its public endpoint and NAT type
//   - report the endpoint to the server and print the online peer list
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"

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
	)
	flag.Parse()
	if *name == "" {
		return fmt.Errorf("--name is required (e.g. --name alice)")
	}

	// 1. Discover our public endpoint + NAT type.
	probe := stunProbe(*stunPrimary, *stunSecondary)
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

	readErr := make(chan error, 1)
	go func() {
		for {
			var env protocol.Envelope
			if err := conn.ReadJSON(&env); err != nil {
				readErr <- fmt.Errorf("server connection: %w", err)
				return
			}
			switch env.Type {
			case protocol.TypeRegistered:
				var reg protocol.Registered
				_ = json.Unmarshal(env.Data, &reg)
				fmt.Printf("registered      : id=%s virtual_ip=%s\n", reg.ClientID, reg.VirtualIP)
				printPeers(reg.Peers)
				reportEndpoint(conn, probe)
			case protocol.TypePeersList:
				var list protocol.PeersList
				_ = json.Unmarshal(env.Data, &list)
				printPeers(list.Peers)
			case protocol.TypeError:
				var e protocol.Error
				_ = json.Unmarshal(env.Data, &e)
				fmt.Printf("server error    : %s\n", e.Message)
				readErr <- fmt.Errorf("server error: %s", e.Message)
				return
			}
		}
	}()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	select {
	case <-interrupt:
		fmt.Println("\nbye")
		return nil
	case err := <-readErr:
		return err
	}
}

// stunProbe runs NAT detection, degrading gracefully if STUN is unreachable
// (registration can still work; hole punching just won't be available).
func stunProbe(primaryHost, secondaryHost string) *stun.Result {
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
	probe, err := stun.Detect(primary, secondary, 3*time.Second)
	if err != nil {
		log.Printf("warning: STUN probe failed: %v", err)
		return &stun.Result{NAT: stun.NATUnreachable}
	}
	return probe
}

// reportEndpoint tells the server the public endpoint we discovered, so other
// peers can reach us (used for hole punching in M2+).
func reportEndpoint(conn *websocket.Conn, probe *stun.Result) {
	ep := protocol.ReportEndpoint{NATType: string(probe.NAT)}
	if probe.Mapped.IP != nil {
		ep.PublicIP = probe.Mapped.IP.String()
		ep.PublicPort = probe.Mapped.Port
	}
	if err := send(conn, protocol.TypeReportEndpoint, ep); err != nil {
		log.Printf("warning: report endpoint: %v", err)
	}
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
