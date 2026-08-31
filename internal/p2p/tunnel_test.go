package p2p

import (
	"net"
	"testing"
	"time"

	"eliaukvpn/internal/crypto"
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

// sessionFor returns a peer's negotiated session, or nil.
func (tun *Tunnel) sessionFor(t *testing.T, peerID string) *crypto.Session {
	t.Helper()
	tun.mu.Lock()
	defer tun.mu.Unlock()
	if p, ok := tun.peers[peerID]; ok {
		return p.session
	}
	return nil
}

// connectPair punches alice and bob together on loopback, both with identities,
// and waits until both are connected.
func connectEncryptedPair(t *testing.T, alice, bob *Tunnel, aConn, bConn *net.UDPConn) {
	t.Helper()
	alice.BeginConnect("bbbbbbbbbbbbbbbb", "bob", []*net.UDPAddr{bConn.LocalAddr().(*net.UDPAddr)})
	bob.BeginConnect("aaaaaaaaaaaaaaaa", "alice", []*net.UDPAddr{aConn.LocalAddr().(*net.UDPAddr)})
	waitConnected(t, alice, "bbbbbbbbbbbbbbbb", 3*time.Second)
	waitConnected(t, bob, "aaaaaaaaaaaaaaaa", 3*time.Second)
}

// TestEncryptedDataPlane runs the M6 handshake over the real UDP path: both
// sides install identities, punch, negotiate a session, and then a data frame
// crosses and is decrypted by the peer. A session being present on both sides
// (combined with the crypto package's Seal/Open tests proving the ciphertext
// differs from the plaintext) means the on-wire frame is AES-GCM encrypted.
func TestEncryptedDataPlane(t *testing.T) {
	alice, aConn := newTunnel(t, "aaaaaaaaaaaaaaaa")
	bob, bConn := newTunnel(t, "bbbbbbbbbbbbbbbb")

	aliceID, err := crypto.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	bobID, err := crypto.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	alice.SetIdentity(aliceID)
	bob.SetIdentity(bobID)

	connectEncryptedPair(t, alice, bob, aConn, bConn)

	if alice.sessionFor(t, "bbbbbbbbbbbbbbbb") == nil {
		t.Fatal("alice has no session after connecting")
	}
	if bob.sessionFor(t, "aaaaaaaaaaaaaaaa") == nil {
		t.Fatal("bob has no session after connecting")
	}

	got := make(chan []byte, 1)
	bob.SetDataSink(func(pkt []byte) { got <- append([]byte(nil), pkt...) })

	if err := alice.SendData("bbbbbbbbbbbbbbbb", testPayload); err != nil {
		t.Fatalf("SendData: %v", err)
	}
	select {
	case pkt := <-got:
		if string(pkt) != string(testPayload) {
			t.Fatalf("decrypted payload mismatch: got %v want %v", pkt, testPayload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("encrypted data never arrived")
	}
}

// TestMixedIdentityRejected verifies that a client with an identity refuses to
// complete a handshake with a legacy (cleartext, no identity) peer — a crypto
// downgrade must not be silent. The initiator stays connecting and eventually
// fails; it must never report the peer as connected.
func TestMixedIdentityRejected(t *testing.T) {
	alice, aConn := newTunnel(t, "aaaaaaaaaaaaaaaa")
	bob, bConn := newTunnel(t, "bbbbbbbbbbbbbbbb")

	aliceID, err := crypto.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	alice.SetIdentity(aliceID) // bob stays legacy (no identity)

	alice.BeginConnect("bbbbbbbbbbbbbbbb", "bob", []*net.UDPAddr{bConn.LocalAddr().(*net.UDPAddr)})
	bob.BeginConnect("aaaaaaaaaaaaaaaa", "alice", []*net.UDPAddr{aConn.LocalAddr().(*net.UDPAddr)})

	// alice must not connect to a legacy peer.
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, s := range alice.Snapshot() {
			if s.ID == "bbbbbbbbbbbbbbbb" && s.State == StateConnected {
				t.Fatal("alice connected to a legacy peer (crypto downgrade)")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if alice.sessionFor(t, "bbbbbbbbbbbbbbbb") != nil {
		t.Fatal("alice established a session with a legacy peer")
	}
}

// TestWhitelistRejectsUnknownPeer verifies M6b: when a friends allowlist is
// installed, a peer whose static key is not in it can never establish a session
// or become connected.
func TestWhitelistRejectsUnknownPeer(t *testing.T) {
	alice, aConn := newTunnel(t, "aaaaaaaaaaaaaaaa")
	bob, bConn := newTunnel(t, "bbbbbbbbbbbbbbbb")
	aliceID, _ := crypto.GenerateIdentity()
	bobID, _ := crypto.GenerateIdentity()
	otherID, _ := crypto.GenerateIdentity()
	alice.SetIdentity(aliceID)
	alice.SetFriends([][]byte{otherID.PublicKey()}) // bob is NOT whitelisted
	bob.SetIdentity(bobID)

	alice.BeginConnect("bbbbbbbbbbbbbbbb", "bob", []*net.UDPAddr{bConn.LocalAddr().(*net.UDPAddr)})
	bob.BeginConnect("aaaaaaaaaaaaaaaa", "alice", []*net.UDPAddr{aConn.LocalAddr().(*net.UDPAddr)})

	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, s := range alice.Snapshot() {
			if s.ID == "bbbbbbbbbbbbbbbb" && s.State == StateConnected {
				t.Fatal("alice connected to a non-whitelisted peer")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if alice.sessionFor(t, "bbbbbbbbbbbbbbbb") != nil {
		t.Fatal("alice established a session with a non-whitelisted peer")
	}
}

// TestWhitelistAcceptsFriend verifies that two peers who whitelist each other
// connect and establish a session normally.
func TestWhitelistAcceptsFriend(t *testing.T) {
	alice, aConn := newTunnel(t, "aaaaaaaaaaaaaaaa")
	bob, bConn := newTunnel(t, "bbbbbbbbbbbbbbbb")
	aliceID, _ := crypto.GenerateIdentity()
	bobID, _ := crypto.GenerateIdentity()
	alice.SetIdentity(aliceID)
	alice.SetFriends([][]byte{bobID.PublicKey()})
	bob.SetIdentity(bobID)
	bob.SetFriends([][]byte{aliceID.PublicKey()})

	connectEncryptedPair(t, alice, bob, aConn, bConn)
	if alice.sessionFor(t, "bbbbbbbbbbbbbbbb") == nil {
		t.Fatal("alice has no session with a whitelisted friend")
	}
	if bob.sessionFor(t, "aaaaaaaaaaaaaaaa") == nil {
		t.Fatal("bob has no session with a whitelisted friend")
	}
}
