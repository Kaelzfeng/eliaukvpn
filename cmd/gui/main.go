//go:build windows

// Command gui is the Eliauk VPN Windows tray application. It wraps the same
// headless agent as cmd/client (internal/agent) behind a notification-area
// icon: right-click shows the status menu (identity, virtual IP, NAT, peers
// with connection state, quit). The menu is rebuilt every second from the
// agent's live status.
//
// Build for a windowless release: go build -ldflags "-H windowsgui" ./cmd/gui
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"eliaukvpn/internal/agent"
	"eliaukvpn/internal/tray"
)

const actQuit = 1

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
		debugPackets  = flag.Bool("debug-packets", false, "log every packet flowing between the virtual NIC and the tunnel")
		keyfile       = flag.String("keyfile", agent.DefaultKeyfile(), "path to the X25519 identity key (created on first run)")
		friendsFile   = flag.String("friends", "", "path to the friends allowlist (base64 fingerprints, one per line; # comments)")
		exitAfter     = flag.Duration("exit-after", 0, "automation hook: quit the tray after this long (0 = run until quit)")
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
		Info:          log.Printf,
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

	tr, err := tray.New()
	if err != nil {
		cancel()
		return err
	}
	tr.SetTooltip("Eliauk VPN — connecting…")
	tr.SetMenu(buildMenu(ag))

	// Keep the tray menu in sync with the agent's live status.
	refreshDone := make(chan struct{})
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				tr.SetTooltip(statusTooltip(ag))
				tr.SetMenu(buildMenu(ag))
			case <-refreshDone:
				return
			}
		}
	}()
	defer close(refreshDone)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if err := tr.Run(func(id int) {
			if id == actQuit {
				log.Printf("tray: quit requested")
				cancel()
				tr.Stop()
			}
		}); err != nil {
			log.Printf("tray: %v", err)
			cancel()
		}
	}()

	if *exitAfter > 0 {
		log.Printf("tray: automation hook — will quit after %s", *exitAfter)
		time.AfterFunc(*exitAfter, func() {
			cancel()
			tr.Stop()
		})
	}

	// Exit when either the agent dies (server gone) or the user quits.
	select {
	case err := <-errc:
		if err != nil {
			log.Printf("agent: %v", err)
		}
		cancel()
		tr.Stop()
		<-runDone
		return err
	case <-runDone:
		cancel()
		return nil
	}
}

// statusTooltip is the one-line tray tooltip.
func statusTooltip(ag *agent.Agent) string {
	st := ag.Status()
	if !st.Registered {
		return "Eliauk VPN — connecting…"
	}
	return fmt.Sprintf("Eliauk VPN — %s (%s)", st.Name, st.VirtualIP)
}

// buildMenu snapshots the agent state into the tray menu model.
func buildMenu(ag *agent.Agent) []tray.Item {
	st := ag.Status()
	items := []tray.Item{
		{Label: "Eliauk VPN", Disabled: true},
		{Label: "Name: " + st.Name, Disabled: true},
	}
	if !st.Registered {
		return append(items, tray.Item{Label: "Status: connecting to server…", Disabled: true})
	}
	items = append(items,
		tray.Item{Label: "ID: " + st.ID, Disabled: true},
		tray.Item{Label: "Virtual IP: " + st.VirtualIP, Disabled: true},
		tray.Item{Label: "Public: " + st.Public, Disabled: true},
		tray.Item{Label: "NAT: " + st.NAT, Disabled: true},
		tray.Item{Label: "Identity: " + st.Identity, Disabled: true},
		tray.Item{Label: fmt.Sprintf("Friends: %d allowed", st.FriendCt), Disabled: true},
		tray.Item{Separator: true},
	)
	snaps := ag.Snapshot()
	peers := make([]tray.Item, 0, len(snaps)+1)
	if len(snaps) == 0 {
		peers = append(peers, tray.Item{Label: "(no connections yet)", Disabled: true})
	} else {
		for _, s := range snaps {
			peers = append(peers, tray.Item{
				Label:    fmt.Sprintf("%s — %s (%s)", s.Name, s.State, s.Remote),
				Disabled: true,
			})
		}
	}
	return append(items,
		tray.Item{Label: fmt.Sprintf("Peers (%d)", len(snaps)), Submenu: peers},
		tray.Item{Separator: true},
		tray.Item{Label: "Quit Eliauk VPN", ID: actQuit},
	)
}
