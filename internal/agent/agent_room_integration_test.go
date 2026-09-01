package agent

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"eliaukvpn/internal/p2p"
	"eliaukvpn/internal/protocol"
	"eliaukvpn/internal/server"
)

// startTestServer launches a real coordination server on an ephemeral local
// port and returns its WebSocket URL.
func startTestServer(t *testing.T) string {
	t.Helper()
	reg := server.NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.HandleWS(reg, "", w, r)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go http.Serve(ln, mux)
	t.Cleanup(func() { ln.Close() })
	return "ws://" + ln.Addr().String() + "/ws"
}

// startTestStun serves minimal RFC 5389 Binding Responses so the agents have a
// real public endpoint to punch with. It reports the request's source address
// as the mapped address (both test agents share the host, so the punch then
// lands on each other's 0.0.0.0 socket).
func startTestStun(t *testing.T) string {
	t.Helper()
	// Bind the loopback address (not 0.0.0.0): LocalAddr would otherwise hand
	// the agents an unspecified destination that never routes.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("stun listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			// Only binding requests are served; ignore short/garbled datagrams.
			if n < 20 || binary.BigEndian.Uint16(buf[0:2]) != 0x0001 {
				continue
			}
			resp := buildStunBindingResponse(buf[:n], src)
			_, _ = conn.WriteToUDP(resp, src)
		}
	}()
	return conn.LocalAddr().String()
}

// buildStunBindingResponse echoes the request's transaction ID and answers with
// a Binding Success Response whose XOR-MAPPED-ADDRESS is the request's source.
func buildStunBindingResponse(req []byte, src *net.UDPAddr) []byte {
	const magicCookie = 0x2112A442
	const attrXORMappedAddress = 0x0020

	// Attribute value: reserved(1) family(1) xor-port(2) xor-ip(4).
	val := make([]byte, 8)
	val[1] = 0x01 // IPv4
	binary.BigEndian.PutUint16(val[2:4], uint16(src.Port)^0x2112)
	var ip4 [4]byte
	copy(ip4[:], src.IP.To4())
	binary.BigEndian.PutUint32(val[4:8], binary.BigEndian.Uint32(ip4[:])^magicCookie)

	attr := make([]byte, 4+8)
	binary.BigEndian.PutUint16(attr[0:2], attrXORMappedAddress)
	binary.BigEndian.PutUint16(attr[2:4], 8)
	copy(attr[4:], val)

	hdr := make([]byte, 20)
	binary.BigEndian.PutUint16(hdr[0:2], 0x0101) // Binding Success Response
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(attr)))
	binary.BigEndian.PutUint32(hdr[4:8], magicCookie)
	copy(hdr[8:20], req[8:20]) // echo the transaction ID
	return append(hdr, attr...)
}

// startTestAgent runs an anonymous agent (name + auto-created identity) until
// registration completes.
func startTestAgent(t *testing.T, wsURL, stunAddr, user string) *Agent {
	t.Helper()
	opts := Options{
		Name:          user,
		Server:        wsURL,
		UseVnic:       false,
		LanEmu:        false,
		StunPrimary:   stunAddr,
		StunSecondary: stunAddr,
		Keyfile:       filepath.Join(t.TempDir(), "key-"+user),
		Info:          t.Logf,
		Logf:          t.Logf,
	}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("new agent %s: %v", user, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	t.Cleanup(func() { cancel(); a.Close() })
	waitFor(t, 10*time.Second, func() bool {
		return a.Status().Registered
	}, user+" registration")
	return a
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func hasFingerprint(list []string, fp string) bool {
	for _, f := range list {
		if f == fp {
			return true
		}
	}
	return false
}

func hasPeer(list []protocol.Peer, name string) bool {
	for _, p := range list {
		if p.Name == name {
			return true
		}
	}
	return false
}

// TestRoomIntegration verifies the one-click-join promise end to end: two
// anonymous agents connect to a real server, the host creates a room, the guest
// joins by code, and they instantly become mutually visible and whitelisted
// for P2P — the room code is the sole gate.
func TestRoomIntegration(t *testing.T) {
	wsURL := startTestServer(t)
	stunAddr := startTestStun(t)
	host := startTestAgent(t, wsURL, stunAddr, "host")
	guest := startTestAgent(t, wsURL, stunAddr, "guest")

	// Host creates a room; the agent learns the code from room_joined.
	must(t, host.CreateRoom())
	var code string
	waitFor(t, 10*time.Second, func() bool {
		if r := host.RoomState(); r != nil && r.Code != "" {
			code = r.Code
			return true
		}
		return false
	}, "host room code")

	// Guest joins by code -> room_joined carries both members.
	must(t, guest.JoinRoom(code))
	waitFor(t, 10*time.Second, func() bool {
		r := guest.RoomState()
		return r != nil && r.Code == code && len(r.Members) == 2
	}, "guest in room with 2 members")

	hostFP := host.Status().Identity
	guestFP := guest.Status().Identity

	// The whitelist union includes room members (not friends): each side must
	// whitelist the other's identity fingerprint.
	waitFor(t, 10*time.Second, func() bool {
		return hasFingerprint(guest.Friends(), hostFP) && hasFingerprint(host.Friends(), guestFP)
	}, "mutual room-member whitelist")

	// Same-room members are visible to each other in the peer list (the server
	// broadcasts peers_list after the join), so auto-connect can fire.
	waitFor(t, 10*time.Second, func() bool {
		return hasPeer(host.Peers(), "guest") && hasPeer(guest.Peers(), "host")
	}, "mutual peer visibility")

	// The tunnel actually wires them up: on localhost the hole punch succeeds,
	// and once connected the room lets both directions carry traffic.
	waitFor(t, 15*time.Second, func() bool {
		for _, s := range host.Snapshot() {
			if s.Name == "guest" && s.State == p2p.StateConnected {
				return true
			}
		}
		return false
	}, "host<->guest P2P tunnel connected")

	// Leaving the room must drop the room-sourced fingerprints on BOTH sides
	// (the remaining member via room_update, the leaver via room_left) and clear
	// the leaver's room state so the GUI returns to the join buttons.
	must(t, guest.LeaveRoom())
	waitFor(t, 10*time.Second, func() bool {
		return !hasFingerprint(host.Friends(), guestFP) && !hasFingerprint(guest.Friends(), hostFP)
	}, "room-sourced fingerprints dropped after leave")
	if guest.RoomState() != nil {
		t.Fatal("guest still reports a room after leaving")
	}
}
