package p2p

import (
	"net"
	"testing"
	"time"
)

// testPayload is a minimal valid IPv4 packet (like one read off the virtual
// NIC) — header + a few bytes of payload.
var testPayload = []byte{
	0x45, 0x00, 0x00, 0x1c, // version/IHL, TOS, total length
	0x00, 0x00, 0x00, 0x00, // id, flags/frag
	0x40, 0x01, 0x00, 0x00, // TTL, proto, checksum
	0x0a, 0x00, 0x00, 0x02, // src 10.0.0.2
	0x0a, 0x00, 0x00, 0x03, // dst 10.0.0.3
	0x68, 0x65, 0x6c, 0x6c, 0x6f, // "hello"
}

// newTunnel opens a loopback UDP socket and returns a ready tunnel.
func newTunnel(t *testing.T, id string) (*Tunnel, *net.UDPConn) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tun := New(conn, id, t.Logf)
	go tun.Run()
	t.Cleanup(func() { _ = conn.Close() })
	return tun, conn
}

// waitConnected polls until the peer reaches StateConnected or the timeout hits.
func waitConnected(t *testing.T, tun *Tunnel, peerID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, s := range tun.Snapshot() {
			if s.ID == peerID && s.State == StateConnected {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("peer %s never connected; snapshot=%v", peerID, tun.Snapshot())
}

// TestDataPlane verifies the M4 data path: one tunnel's SendData delivers the
// frame to the peer's dataSink. This is exactly the path the virtual NIC loop
// uses (forwardFromVnic -> SendData -> frameData -> dataSink -> vnic.Write).
func TestDataPlane(t *testing.T) {
	alice, aConn := newTunnel(t, "aaaaaaaaaaaaaaaa")
	bob, bConn := newTunnel(t, "bbbbbbbbbbbbbbbb")

	// Each side knows the other's loopback endpoint and starts punching.
	alice.BeginConnect("bbbbbbbbbbbbbbbb", "bob", []*net.UDPAddr{bConn.LocalAddr().(*net.UDPAddr)})
	bob.BeginConnect("aaaaaaaaaaaaaaaa", "alice", []*net.UDPAddr{aConn.LocalAddr().(*net.UDPAddr)})

	waitConnected(t, alice, "bbbbbbbbbbbbbbbb", 3*time.Second)
	waitConnected(t, bob, "aaaaaaaaaaaaaaaa", 3*time.Second)

	// Bob's dataSink captures decapsulated IP packets.
	got := make(chan []byte, 1)
	bob.SetDataSink(func(pkt []byte) {
		got <- append([]byte(nil), pkt...)
	})

	if err := alice.SendData("bbbbbbbbbbbbbbbb", testPayload); err != nil {
		t.Fatalf("SendData: %v", err)
	}

	select {
	case pkt := <-got:
		if string(pkt) != string(testPayload) {
			t.Fatalf("payload mismatch: got %v want %v", pkt, testPayload)
		}
		if len(pkt) < 20 || pkt[0]>>4 != 4 {
			t.Fatalf("not a well-formed IPv4 packet: %v", pkt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dataSink never received the packet")
	}
}

// TestSendDataBeforeConnect verifies SendData refuses unknown/unconnected peers
// (the client relies on this to drop packets safely).
func TestSendDataBeforeConnect(t *testing.T) {
	alice, _ := newTunnel(t, "aaaaaaaaaaaaaaaa")

	if err := alice.SendData("ffffffffffffffff", testPayload); err == nil {
		t.Fatal("SendData to unknown peer should error")
	}

	alice.BeginConnect("ffffffffffffffff", "nobody", nil)
	if err := alice.SendData("ffffffffffffffff", testPayload); err == nil {
		t.Fatal("SendData to connecting peer should error")
	}
}
