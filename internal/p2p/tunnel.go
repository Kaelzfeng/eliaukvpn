// Package p2p implements the UDP tunnel between two clients. It drives the
// hole-punching handshake (M2) and will carry game traffic over the punched
// tunnel (M4+).
package p2p

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
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

	mu        sync.Mutex
	peers     map[string]*Peer
	relayAddr *net.UDPAddr // relay server endpoint (M3); nil = no relay
	forceRelay bool        // skip direct punching entirely (testing / NAT known symmetric)
	dataSink  func(pkt []byte) // receives IP packets decapsulated from peers (M4)
	logf      func(format string, args ...any)
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
		t.mu.Unlock()

		if relayed {
			if !inRelay {
				inRelay = true
				relayDeadline = time.Now().Add(punchTimeout)
			}
			t.sendRelayFrame(frameHello, id)
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
				t.sendFrame(frameHello, id, c)
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
		if p.State != StateConnected {
			p.State = StateConnected
			where := "direct"
			if p.relayed {
				where = "relay"
			}
			t.logf("p2p: connected to %s (%s, %s)", p.Name, addr, where)
		}
		t.sendFrameLocked(frameHelloAck, p)
		p.lastPing = time.Now()
		t.sendFrameLocked(framePing, p)
	case frameHelloAck:
		if p.State != StateConnected {
			p.State = StateConnected
			t.logf("p2p: connected to %s (%s)", p.Name, addr)
			p.lastPing = time.Now()
			t.sendFrameLocked(framePing, p)
		}
	case framePing:
		t.sendFrameLocked(framePong, p)
	case framePong:
		rtt := time.Since(p.lastPing)
		t.logf("p2p: tunnel to %s RTT=%s", p.Name, rtt.Round(time.Microsecond))
	case frameData:
		if t.dataSink != nil {
			t.dataSink(f.payload)
		}
	}
}

// sendFrameLocked writes a control frame to the peer. Caller holds t.mu.
func (t *Tunnel) sendFrameLocked(typ byte, p *Peer) {
	t.sendLocked(typ, p, nil)
}

// sendLocked writes a frame to the peer — directly if we have a working
// endpoint, or wrapped in a relay packet to the relay server (M3). Caller
// holds t.mu.
func (t *Tunnel) sendLocked(typ byte, p *Peer, payload []byte) {
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
func (t *Tunnel) sendFrame(typ byte, peerID string, addr *net.UDPAddr) {
	data, err := buildFrame(typ, t.myID, peerID, nil)
	if err != nil {
		return
	}
	t.logf("p2p: send typ=%d to=%s", typ, addr)
	if _, err := t.conn.WriteToUDP(data, addr); err != nil {
		t.logf("p2p: send to %s: %v", addr, err)
	}
}

// sendRelayFrame sends a hello for peerID through the relay server (M3).
func (t *Tunnel) sendRelayFrame(typ byte, peerID string) {
	data, err := buildFrame(typ, t.myID, peerID, nil)
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
