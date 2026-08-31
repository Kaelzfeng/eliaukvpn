// Command client is the Eliauk VPN agent that runs on each player's machine.
//
// It shares the core agent (internal/agent) with the Windows tray GUI; this
// command adds an interactive command loop for testing and debugging:
// register, STUN probe, hole punching, and a `status`/`peers` view.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"eliaukvpn/internal/agent"
	"eliaukvpn/internal/p2p"
	"eliaukvpn/internal/protocol"
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
		keyfile       = flag.String("keyfile", agent.DefaultKeyfile(), "path to the X25519 identity key (created on first run)")
		friendsFile   = flag.String("friends", "", "path to the friends allowlist (base64 fingerprints, one per line; # comments)")
	)
	flag.Parse()
	if *name == "" {
		return fmt.Errorf("--name is required (e.g. --name alice)")
	}

	ag, err := agent.New(agent.Options{
		Name:          *name,
		Server:        *serverAddr,
		StunPrimary:   *stunPrimary,
		StunSecondary: *stunSecondary,
		ForceRelay:    *forceRelay,
		UseVnic:       *useVnic,
		VnicName:      *vnicName,
		LanEmu:        *lanEmu,
		DebugPackets:  *debugPackets,
		Keyfile:       *keyfile,
		FriendsFile:   *friendsFile,
		Info:          func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
		Logf:          log.Printf,
	})
	if err != nil {
		return err
	}
	defer ag.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- ag.Run(ctx) }()

	// Headless mode just keeps the agent running until the server connection
	// fails (used for background/automation deployment).
	if *headless {
		return <-errc
	}

	fmt.Println("commands: peers | connect <name|id> | status | quit")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		select {
		case err := <-errc:
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
			cancel()
			return <-errc
		case "peers":
			printPeers(ag.Peers())
		case "connect":
			if len(fields) < 2 {
				fmt.Println("usage: connect <name|id>")
				continue
			}
			if err := ag.Connect(fields[1]); err != nil {
				fmt.Println(err)
				continue
			}
			fmt.Printf("requesting p2p connection to %s...\n", fields[1])
		case "status":
			printStatus(ag.Status(), ag.Snapshot())
		default:
			fmt.Println("unknown command")
		}
	}
	return scanner.Err()
}

func printPeers(peers []protocol.Peer) {
	if len(peers) == 0 {
		fmt.Println("peers           : (none online)")
		return
	}
	fmt.Println("peers           :")
	for _, p := range peers {
		ep := "no endpoint yet"
		if p.PublicIP != "" {
			ep = fmt.Sprintf("%s:%d", p.PublicIP, p.PublicPort)
		}
		fmt.Printf("  - %-12s %-12s %s (%s)\n", p.Name, p.VirtualIP, ep, p.NATType)
	}
}

func printStatus(st agent.Status, snaps []p2p.Snapshot) {
	if !st.Registered {
		fmt.Println("status          : not registered yet")
		return
	}
	fmt.Printf("status          : id=%s vip=%s pub=%s nat=%s friends=%d\n",
		st.ID, st.VirtualIP, st.Public, st.NAT, st.FriendCt)
	if len(snaps) == 0 {
		fmt.Println("  (no connections yet)")
	}
	for _, s := range snaps {
		fmt.Printf("  %-12s %-10s %s\n", s.Name, s.State, s.Remote)
	}
}
