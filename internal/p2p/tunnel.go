// Package p2p implements the UDP tunnel between two clients. It drives the
// hole-punching handshake (M2) and will carry game traffic over the punched
// tunnel (M4+).
package p2p

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"eliaukvpn/internal/crypto"
)

// State of a peer connection.
type State string

const (
	StateIdle       State = "idle"
	StateConnecting State = "connecting"
	StateConnected  State = "connected"
	StateFailed     State = "failed"
)

// Frame types carried directly between clients over UDP.
const (
	frameHello    byte = 1 // punch probe
	frameHelloAck byte = 2 // punch confirmed
	framePing     byte = 3 // tunnel liveness probe
	framePong     byte = 4
	frameData     byte = 5 // an IP packet from the virtual NIC (M4)
)

const (
	frameMagic      = "ELK1"
	frameHeaderSize = 4 + 1 + 8 + 8 // magic + type + sender id + receiver id

	punchInterval = 100 * time.Millisecond
	punchTimeout  = 6 * time.Second
)

// Relay packet framing (client -> relay server). Matches internal/server/relay.go.
const (
	relayMagic   = "ELKR"
	relayHdrSize = 4 + 8 + 8 // magic + sender id + target id
	zeroID       = "0000000000000000"
)

// Peer holds the tunnel state for one remote client.
type Peer struct {
	ID     string
	Name   string
	State  State
	Remote *net.UDPAddr // working endpoint once the punch succeeds

	candidates []*net.UDPAddr
	relayed    bool // frames are carried through the relay server (M3)
	lastPing   time.Time

	// M6 encryption state. eph is our ephemeral key for this connection (fresh
	// per connection for forward secrecy), handshake is our public message
	// (eph||static) carried in hello/helloAck, and session is the negotiated
	// symmetric key once both handshake messages have been seen. The session
	// derivation is role-independent, so a simultaneous punch (both sides send
	// hello) still agrees on one key.
	eph       *ecdh.PrivateKey
	handshake *crypto.Handshake
	session   *crypto.Session
}

// Snapshot is a concurrency-safe view of a peer for display.
type Snapshot struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  State  `json:"state"`
	Remote string `json:"remote,omitempty"`
}

// Tunnel manages the UDP socket used for all direct peer connections and
// drives the hole-punching handshake for each peer.
type Tunnel struct {
	conn *net.UDPConn
	myID string // hex client id

	mu         sync.Mutex
	peers      map[string]*Peer
	relayAddr  *net.UDPAddr     // relay server endpoint (M3); nil = no relay
	forceRelay bool             // skip direct punching entirely (testing / NAT known symmetric)
	dataSink   func(pkt []byte) // receives IP packets decapsulated from peers (M4)
	logf       func(format string, args ...any)
	identity   *crypto.Identity // M6: long-term X25519 identity; nil = legacy cleartext
}

// New creates a tunnel on conn. myID is this client's hex id.
func New(conn *net.UDPConn, myID string, logf func(string, ...any)) *Tunnel {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Tunnel{conn: conn, myID: myID, peers: make(map[string]*Peer), logf: logf}
}

// Run reads tunnel packets until the socket is closed.
func (t *Tunnel) Run() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		f, err := parseFrame(buf[:n])
		if err != nil {
			continue
		}
		if f.target != t.myID {
			continue // a different connection on this socket
		}
		t.handleFrame(f, addr)
	}
}

// SetIdentity installs the long-term X25519 identity used to authenticate
// handshakes and encrypt data frames (M6). Until it is set, the tunnel runs in
// legacy cleartext mode. Install before any BeginConnect.
func (t *Tunnel) SetIdentity(id *crypto.Identity) {
	t.mu.Lock()
	t.identity = id
	t.mu.Unlock()
}

// BeginConnect registers a peer and starts punching toward its candidates.
// Safe to call both when we initiated the connection and when a peer asked
// the coordination server to connect to us.
func (t *Tunnel) BeginConnect(peerID, peerName string, candidates []*net.UDPAddr) {
	t.mu.Lock()
	if _, ok := t.peers[peerID]; ok {
		t.mu.Unlock()
		return
	}
	p := &Peer{ID: peerID, Name: peerName, State: StateConnecting, candidates: candidates}
	if t.forceRelay {
		p.relayed = true
	}
	if t.identity != nil {
		// Generate the per-connection ephemeral key once and reuse it across
		// hello retransmissions — the responder must see a stable initiator key
		// so it can derive the same session on its first hello and reuse it on
		// the retransmits.
		eph, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.logf("p2p: generate ephemeral for %s: %v", peerID, err)
		} else {
			p.eph = eph
			p.handshake = &crypto.Handshake{
				Eph:  append([]byte(nil), eph.PublicKey().Bytes()...),
				Stat: t.identity.PublicKey(),
			}
		}
	}
	t.peers[peerID] = p
	t.mu.Unlock()
	go t.punchLoop(p)
}

// SetRelay configures the relay server endpoint used when direct punching
// fails. Call once after registration (before any BeginConnect).
func (t *Tunnel) SetRelay(addr string) error {
	a, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.relayAddr = a
	t.mu.Unlock()
	return nil
}

// SetForceRelay makes every connection use the relay path without attempting
// direct punching. Useful for testing and for clients known to sit behind a
// symmetric NAT.
func (t *Tunnel) SetForceRelay(v bool) {
	t.mu.Lock()
	t.forceRelay = v
	t.mu.Unlock()
}

// SetDataSink registers the callback that receives IP packets decapsulated
// from connected peers (M4). The client writes them into the virtual NIC.
func (t *Tunnel) SetDataSink(fn func([]byte)) {
	t.mu.Lock()
	t.dataSink = fn
	t.mu.Unlock()
}

// SendData sends an IP packet to a peer over the established tunnel (direct
// or relayed). It returns an error if the peer is unknown or not connected —
// the caller may drop the packet (upper layers will retry).
func (t *Tunnel) SendData(peerID string, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.peers[peerID]
	if !ok {
		return fmt.Errorf("unknown peer %s", peerID)
	}
	if p.State != StateConnected {
		return fmt.Errorf("peer %s not connected", peerID)
	}
	t.sendLocked(frameData, p, payload)
	return nil
}

// SendDataBroadcast sends an IP packet to every connected peer (M5). This is
// how software-layer broadcast emulation fans a LAN-discovery packet out — the
// virtual NIC has no broadcast/multicast of its own, so the client replicates
// it to all peers, and each peer's dataSink writes it into its own NIC.
func (t *Tunnel) SendDataBroadcast(payload []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.peers {
		if p.State != StateConnected {
			continue
		}
		t.sendLocked(frameData, p, payload)
	}
}

// HasPeer reports whether the tunnel knows about the given peer id.
func (t *Tunnel) HasPeer(peerID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.peers[peerID]
	return ok
}

// Announce tells the relay server the address we send from, so it can forward
// to us from the very first fallback packet. Call once after SetRelay.
func (t *Tunnel) Announce() {
	t.mu.Lock()
	ra := t.relayAddr
	t.mu.Unlock()
	if ra == nil {
		return
	}
	data, err := buildRelay(t.myID, zeroID, nil)
	if err != nil {
		return
	}
	if _, err := t.conn.WriteToUDP(data, ra); err != nil {
		t.logf("p2p: relay announce: %v", err)
	}
}

// Snapshot returns the tunnel state for all known peers.
func (t *Tunnel) Snapshot() []Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Snapshot, 0, len(t.peers))
	for _, p := range t.peers {
		s := Snapshot{ID: p.ID, Name: p.Name, State: p.State}
		if p.Remote != nil {
			s.Remote = p.Remote.String()
		}
		out = append(out, s)
	}
	return out
}

// punchLoop keeps sending hello frames until the peer acknowledges, times
// out, or is marked connected. Both sides run this loop simultaneously — that
// simultaneity is what opens the holes in both NATs.
//
// If the direct punch times out (symmetric NAT), the loop falls back to
// carrying the same handshake through the relay server (M3).
func (t *Tunnel) punchLoop(p *Peer) {
	directDeadline := time.Now().Add(punchTimeout)
	relayDeadline := time.Time{}
	inRelay := false
	for {
		t.mu.Lock()
		if p.State != StateConnecting {
			t.mu.Unlock()
			return
		}
		id := p.ID
		cands := append([]*net.UDPAddr(nil), p.candidates...)
		relayed := p.relayed
		var helloPayload []byte
		if p.handshake != nil {
			helloPayload = p.handshake.Marshal()
		}
		t.mu.Unlock()

		if relayed {
			if !inRelay {
				inRelay = true
				relayDeadline = time.Now().Add(punchTimeout)
			}
			t.sendRelayFrame(frameHello, id, helloPayload)
			if time.Now().After(relayDeadline) {
				t.mu.Lock()
				if p.State == StateConnecting {
					p.State = StateFailed
					t.logf("p2p: relay to %s (%s) timed out", p.Name, p.ID)
				}
				t.mu.Unlock()
				return
			}
		} else {
			for _, c := range cands {
				t.sendFrame(frameHello, id, c, helloPayload)
			}
			if time.Now().After(directDeadline) {
				t.mu.Lock()
				if p.State == StateConnecting && !p.relayed {
					if t.relayAddr != nil {
						p.relayed = true
						t.logf("p2p: direct punch to %s failed, switching to relay (%s)", p.Name, t.relayAddr)
					} else {
						p.State = StateFailed
						t.logf("p2p: hole punch to %s (%s) failed — no relay available", p.Name, p.ID)
					}
				}
				t.mu.Unlock()
			}
		}
		time.Sleep(punchInterval)
	}
}

func (t *Tunnel) handleFrame(f *frame, addr *net.UDPAddr) {
	t.logf("p2p: recv typ=%d sender=%s target=%s src=%s", f.typ, f.sender, f.target, addr)
	t.mu.Lock()
	defer t.mu.Unlock()

	p, ok := t.peers[f.sender]
	if !ok {
		// Unknown sender. Peers are only ever signalled through the
		// coordination server, so ignore stray packets.
		return
	}

	// A frame whose source is the relay server was forwarded by it (M3):
	// use the relay path for replies and don't treat the server's address as
	// a direct peer endpoint. A frame from anywhere else is a direct
	// connection, which we prefer over relay.
	fromRelay := t.relayAddr != nil && addr.IP.Equal(t.relayAddr.IP) && addr.Port == t.relayAddr.Port
	if fromRelay {
		p.relayed = true
	} else {
		p.relayed = false
		p.Remote = addr
	}

	switch f.typ {
	case frameHello:
		// M6: as the responder, derive the session from the initiator's hello
		// (ephemeral||static). Retransmitted hellos are idempotent — the session
		// is kept from the first one and reused. If identity is nil (legacy
		// mode) the handshake is skipped and helloAck carries no payload.
		var ackPayload []byte
		if t.identity != nil {
			if err := t.responderHandshake(p, f.payload); err != nil {
				t.logf("p2p: handshake with %s rejected: %v", p.Name, err)
				return
			}
			if p.handshake != nil {
				ackPayload = p.handshake.Marshal()
			}
		}
		if p.State != StateConnected {
			p.State = StateConnected
			where := "direct"
			if p.relayed {
				where = "relay"
			}
			t.logf("p2p: connected to %s (%s, %s)", p.Name, addr, where)
		}
		t.sendFrameLocked(frameHelloAck, p, ackPayload)
		p.lastPing = time.Now()
		t.sendFrameLocked(framePing, p, nil)
	case frameHelloAck:
		// M6: as the initiator, complete the session with the responder's
		// helloAck payload. Retransmitted acks are idempotent (session set).
		if t.identity != nil && p.session == nil {
			if err := t.initiatorHandshake(p, f.payload); err != nil {
				t.logf("p2p: handshake with %s rejected: %v", p.Name, err)
				return
			}
		}
		if p.State != StateConnected {
			p.State = StateConnected
			t.logf("p2p: connected to %s (%s)", p.Name, addr)
			p.lastPing = time.Now()
			t.sendFrameLocked(framePing, p, nil)
		}
	case framePing:
		t.sendFrameLocked(framePong, p, nil)
	case framePong:
		rtt := time.Since(p.lastPing)
		t.logf("p2p: tunnel to %s RTT=%s", p.Name, rtt.Round(time.Microsecond))
	case frameData:
		pkt := f.payload
		if p.session != nil {
			var err error
			pkt, err = p.session.Open(peerAAD(f.sender, f.target), f.payload)
			if err != nil {
				t.logf("p2p: decrypt from %s: %v", p.Name, err)
				return
			}
		}
		if t.dataSink != nil {
			t.dataSink(pkt)
		}
	}
}

// responderHandshake derives our side of the session from the peer's hello
// payload. Called on every hello, but the session is established once and
// reused for retransmitted hellos.
//
// The ephemeral is NOT regenerated here: BeginConnect created one per
// connection, and both sides use that single key for their hello, their
// helloAck, and the session derivation. In a simultaneous punch both sides run
// this on the other's hello, and because the derivation is role-independent
// they arrive at the same key. Generating a second ephemeral here would break
// the agreement (each side would use a different combination of keys).
func (t *Tunnel) responderHandshake(p *Peer, payload []byte) error {
	if p.session != nil {
		return nil // already established
	}
	if p.eph == nil {
		return errors.New("no local ephemeral")
	}
	var peerHS crypto.Handshake
	if err := peerHS.Unmarshal(payload); err != nil {
		return fmt.Errorf("malformed hello handshake: %w", err)
	}
	if err := t.checkPeerStatic(peerHS.Stat); err != nil {
		return err
	}
	sess, err := crypto.NewSession(t.identity, p.eph, peerHS.Eph, peerHS.Stat)
	if err != nil {
		return err
	}
	p.session = sess
	t.logf("p2p: session established with %s (responder)", p.Name)
	return nil
}

// initiatorHandshake completes the session with the responder's helloAck
// payload. Only runs once per connection (the initiator's ephemeral was created
// in BeginConnect and its session is set on the first ack).
func (t *Tunnel) initiatorHandshake(p *Peer, payload []byte) error {
	if p.eph == nil {
		return errors.New("no initiator ephemeral")
	}
	var peerHS crypto.Handshake
	if err := peerHS.Unmarshal(payload); err != nil {
		return fmt.Errorf("malformed helloAck handshake: %w", err)
	}
	if err := t.checkPeerStatic(peerHS.Stat); err != nil {
		return err
	}
	sess, err := crypto.NewSession(t.identity, p.eph, peerHS.Eph, peerHS.Stat)
	if err != nil {
		return err
	}
	p.session = sess
	t.logf("p2p: session established with %s (initiator)", p.Name)
	return nil
}

// checkPeerStatic validates a peer's static identity key. Whitelist enforcement
// (M6b) is added here.
func (t *Tunnel) checkPeerStatic(stat []byte) error {
	if _, err := ecdh.X25519().NewPublicKey(stat); err != nil {
		return fmt.Errorf("peer static key invalid: %w", err)
	}
	return nil
}

// peerAAD returns the authenticated data bound to each data frame: the two
// client IDs in a canonical (sorted) order, so both endpoints derive the same
// value regardless of which side sent first. Binding the peer pair to the
// ciphertext prevents a captured frame from being replayed at a different peer.
func peerAAD(a, b string) []byte {
	if a > b {
		a, b = b, a
	}
	return []byte("ELK1|" + a + "|" + b)
}

// sendFrameLocked writes a control frame to the peer. Caller holds t.mu.
func (t *Tunnel) sendFrameLocked(typ byte, p *Peer, payload []byte) {
	t.sendLocked(typ, p, payload)
}

// sendLocked writes a frame to the peer — directly if we have a working
// endpoint, or wrapped in a relay packet to the relay server (M3). Caller
// holds t.mu.
func (t *Tunnel) sendLocked(typ byte, p *Peer, payload []byte) {
	// M6: encrypt data frames under the negotiated session key. Control frames
	// (hello/helloAck/ping/pong) stay cleartext. AAD binds the peer pair.
	if typ == frameData && p.session != nil {
		payload = p.session.Seal(peerAAD(t.myID, p.ID), payload)
	}
	data, err := buildFrame(typ, t.myID, p.ID, payload)
	if err != nil {
		return
	}
	if p.relayed {
		if t.relayAddr == nil {
			return
		}
		rp, err := buildRelay(t.myID, p.ID, data)
		if err != nil {
			return
		}
		if typ != frameData {
			t.logf("p2p: send typ=%d to=%s (relay)", typ, t.relayAddr)
		}
		if _, err := t.conn.WriteToUDP(rp, t.relayAddr); err != nil {
			t.logf("p2p: relay send: %v", err)
		}
		return
	}
	if p.Remote == nil {
		return
	}
	if typ != frameData {
		t.logf("p2p: send typ=%d to=%s", typ, p.Remote)
	}
	if _, err := t.conn.WriteToUDP(data, p.Remote); err != nil {
		t.logf("p2p: send to %s: %v", p.Remote, err)
	}
}

// sendFrame writes a frame to an arbitrary address (used while punching,
// before the working address is known).
func (t *Tunnel) sendFrame(typ byte, peerID string, addr *net.UDPAddr, payload []byte) {
	data, err := buildFrame(typ, t.myID, peerID, payload)
	if err != nil {
		return
	}
	t.logf("p2p: send typ=%d to=%s", typ, addr)
	if _, err := t.conn.WriteToUDP(data, addr); err != nil {
		t.logf("p2p: send to %s: %v", addr, err)
	}
}

// sendRelayFrame sends a hello for peerID through the relay server (M3).
func (t *Tunnel) sendRelayFrame(typ byte, peerID string, payload []byte) {
	data, err := buildFrame(typ, t.myID, peerID, payload)
	if err != nil {
		return
	}
	rp, err := buildRelay(t.myID, peerID, data)
	if err != nil {
		return
	}
	t.mu.Lock()
	ra := t.relayAddr
	t.mu.Unlock()
	if ra == nil {
		return
	}
	t.logf("p2p: send typ=%d to=%s (relay)", typ, ra)
	if _, err := t.conn.WriteToUDP(rp, ra); err != nil {
		t.logf("p2p: relay send: %v", err)
	}
}

// --- frame encoding ---

type frame struct {
	typ     byte
	sender  string // hex client id
	target  string // hex client id
	payload []byte
}

func buildFrame(typ byte, myID, peerID string, payload []byte) ([]byte, error) {
	me, err := hex.DecodeString(myID)
	if err != nil || len(me) != 8 {
		return nil, fmt.Errorf("bad local id %q", myID)
	}
	peer, err := hex.DecodeString(peerID)
	if err != nil || len(peer) != 8 {
		return nil, fmt.Errorf("bad peer id %q", peerID)
	}
	buf := make([]byte, frameHeaderSize+len(payload))
	copy(buf[0:4], frameMagic)
	buf[4] = typ
	copy(buf[5:13], me)
	copy(buf[13:21], peer)
	copy(buf[21:], payload)
	return buf, nil
}

func parseFrame(buf []byte) (*frame, error) {
	if len(buf) < frameHeaderSize || string(buf[0:4]) != frameMagic {
		return nil, errors.New("bad frame")
	}
	return &frame{
		typ:     buf[4],
		sender:  hex.EncodeToString(buf[5:13]),
		target:  hex.EncodeToString(buf[13:21]),
		payload: buf[frameHeaderSize:],
	}, nil
}

// buildRelay wraps a frame in the relay envelope for the relay server:
// "ELKR" | sender id | target id | frame. A zero target marks an announce.
func buildRelay(senderID, targetID string, frame []byte) ([]byte, error) {
	me, err := hex.DecodeString(senderID)
	if err != nil || len(me) != 8 {
		return nil, fmt.Errorf("bad sender id %q", senderID)
	}
	peer, err := hex.DecodeString(targetID)
	if err != nil || len(peer) != 8 {
		return nil, fmt.Errorf("bad target id %q", targetID)
	}
	buf := make([]byte, relayHdrSize+len(frame))
	copy(buf[0:4], relayMagic)
	copy(buf[4:12], me)
	copy(buf[12:20], peer)
	copy(buf[20:], frame)
	return buf, nil
}
