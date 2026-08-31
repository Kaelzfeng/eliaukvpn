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
)

const (
	frameMagic      = "ELK1"
	frameHeaderSize = 4 + 1 + 8 + 8 // magic + type + sender id + receiver id

	punchInterval = 100 * time.Millisecond
	punchTimeout  = 6 * time.Second
)

// Peer holds the tunnel state for one remote client.
type Peer struct {
	ID     string
	Name   string
	State  State
	Remote *net.UDPAddr // working endpoint once the punch succeeds

	candidates []*net.UDPAddr
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

	mu    sync.Mutex
	peers map[string]*Peer
	logf  func(format string, args ...any)
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
	t.peers[peerID] = p
	t.mu.Unlock()
	go t.punchLoop(p)
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

// punchLoop keeps sending hello frames to every candidate until the peer
// acknowledges, times out, or is marked connected. Both sides run this loop
// simultaneously — that simultaneity is what opens the holes in both NATs.
func (t *Tunnel) punchLoop(p *Peer) {
	deadline := time.Now().Add(punchTimeout)
	for {
		t.mu.Lock()
		if p.State != StateConnecting {
			t.mu.Unlock()
			return
		}
		id := p.ID
		cands := append([]*net.UDPAddr(nil), p.candidates...)
		t.mu.Unlock()

		for _, c := range cands {
			t.sendFrame(frameHello, id, c)
		}

		if time.Now().After(deadline) {
			t.mu.Lock()
			if p.State == StateConnecting {
				p.State = StateFailed
				t.logf("p2p: hole punch to %s (%s) failed — likely symmetric NAT, relay needed (M3)", p.Name, p.ID)
			}
			t.mu.Unlock()
			return
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
		// Unknown sender. In M2 peers are only ever signalled through the
		// coordination server, so ignore stray packets.
		return
	}

	switch f.typ {
	case frameHello:
		p.Remote = addr
		if p.State != StateConnected {
			p.State = StateConnected
			t.logf("p2p: connected to %s (%s)", p.Name, addr)
		}
		t.sendFrameLocked(frameHelloAck, p)
		p.lastPing = time.Now()
		t.sendFrameLocked(framePing, p)
	case frameHelloAck:
		p.Remote = addr
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
	}
}

// sendFrameLocked writes a frame to the peer's working address. Caller holds t.mu.
func (t *Tunnel) sendFrameLocked(typ byte, p *Peer) {
	if p.Remote == nil {
		return
	}
	data, err := buildFrame(typ, t.myID, p.ID, nil)
	if err != nil {
		return
	}
	t.logf("p2p: send typ=%d to=%s", typ, p.Remote)
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
