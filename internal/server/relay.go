package server

import (
	"encoding/hex"
	"errors"
	"log"
	"net"
)

// Relay implements a minimal TURN-style packet relay for clients whose NAT
// cannot be hole-punched (symmetric NAT). It is deliberately dumb: each relay
// packet carries the sender and target client ids in its header, the server
// learns where each id sends from, and forwards the enclosed payload to the
// target's last known address.
//
// Client -> server packet:
//
//	"ELKR" | sender-id (8B hex) | target-id (8B hex) | payload
//
// A target-id of all zero bytes marks an announce: the server records the
// sender's address but forwards nothing (used right after registration so
// relays work from the first packet).
const (
	relayMagic   = "ELKR"
	relayHdrSize = 4 + 8 + 8
	zeroID       = "0000000000000000"
)

// Relay is the UDP relay endpoint. It learns client addresses from the first
// packet each client sends and forwards frames between known ids.
type Relay struct {
	conn *net.UDPConn
	reg  *Registry
}

// NewRelay binds a UDP socket on addr and starts forwarding in a goroutine.
// The returned Relay's Addr is what clients should send relay traffic to.
func NewRelay(reg *Registry, addr string) (*Relay, error) {
	a, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", a)
	if err != nil {
		return nil, err
	}
	r := &Relay{conn: conn, reg: reg}
	go r.run()
	return r, nil
}

// Addr returns the socket address clients should send relay traffic to.
func (r *Relay) Addr() string { return r.conn.LocalAddr().String() }

// Close stops the relay.
func (r *Relay) Close() error { return r.conn.Close() }

func (r *Relay) run() {
	buf := make([]byte, 4096)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if err := r.handle(buf[:n], src); err != nil {
			log.Printf("relay: drop from %s: %v", src, err)
		}
	}
}

func (r *Relay) handle(pkt []byte, src *net.UDPAddr) error {
	if len(pkt) < relayHdrSize || string(pkt[0:4]) != relayMagic {
		return errors.New("bad relay packet")
	}
	sender := hex.EncodeToString(pkt[4:12])
	target := hex.EncodeToString(pkt[12:20])
	payload := pkt[relayHdrSize:]

	r.reg.SetRelayAddr(sender, src)
	if target == zeroID {
		return nil // announce only
	}
	dst, ok := r.reg.RelayAddr(target)
	if !ok {
		return errors.New("target " + target + " not learned yet")
	}
	if _, err := r.conn.WriteToUDP(payload, dst); err != nil {
		return err
	}
	return nil
}
