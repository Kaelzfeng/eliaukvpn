package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/protocol"
)

// Virtual network layout. The gateway owns .1; clients get .2 onwards.
const (
	virtualSubnet  = "10.0.0.0/24"
	virtualGateway = "10.0.0.1"
	firstClientIP  = 2
)

// Client is one connected peer in the virtual network.
type Client struct {
	ID         string
	Name       string
	VirtualIP  string
	PublicIP   string
	PublicPort int
	NATType    string
	Candidates []protocol.Candidate
	Conn       *websocket.Conn
	writeMu    sync.Mutex // serializes writes so broadcasts don't interleave
}

// Registry keeps track of connected clients and hands out virtual IPs.
type Registry struct {
	mu        sync.Mutex
	clients   map[string]*Client
	nextIP    int
	relayAddr map[string]*net.UDPAddr // client id -> address it relays from
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		clients:   make(map[string]*Client),
		nextIP:    firstClientIP,
		relayAddr: make(map[string]*net.UDPAddr),
	}
}

// SetRelayAddr records the UDP address a client sends relay traffic from.
func (r *Registry) SetRelayAddr(id string, addr *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.relayAddr[id] = addr
}

// RelayAddr returns the relay address learned for a client id.
func (r *Registry) RelayAddr(id string) (*net.UDPAddr, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.relayAddr[id]
	return a, ok
}

// Add registers a new client and assigns it a virtual IP.
func (r *Registry) Add(name string, conn *websocket.Conn) (*Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ip, err := r.allocVirtualIP()
	if err != nil {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	c := &Client{
		ID:        id,
		Name:      name,
		VirtualIP: ip,
		Conn:      conn,
	}
	r.clients[id] = c
	return c, nil
}

// Remove deletes a client (e.g. on disconnect).
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, id)
}

// UpdateEndpoint records the public endpoint a client reported via STUN plus
// its punch candidates.
func (r *Registry) UpdateEndpoint(id, publicIP string, publicPort int, natType string, candidates []protocol.Candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[id]; ok {
		c.PublicIP = publicIP
		c.PublicPort = publicPort
		c.NATType = natType
		c.Candidates = candidates
	}
}

// Peers returns one protocol.Peer per connected client, excluding the given id.
func (r *Registry) Peers(excludeID string) []protocol.Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	peers := make([]protocol.Peer, 0, len(r.clients))
	for _, c := range r.clients {
		if c.ID == excludeID {
			continue
		}
		peers = append(peers, protocol.Peer{
			ID:         c.ID,
			Name:       c.Name,
			VirtualIP:  c.VirtualIP,
			PublicIP:   c.PublicIP,
			PublicPort: c.PublicPort,
			NATType:    c.NATType,
			Online:     true,
		})
	}
	return peers
}

// Client returns the client with the given id.
func (r *Registry) Client(id string) (*Client, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.clients[id]
	return c, ok
}

// Candidates returns the punch candidates reported by a client.
func (r *Registry) Candidates(id string) []protocol.Candidate {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[id]; ok {
		return c.Candidates
	}
	return nil
}

// All returns every connected client.
func (r *Registry) All() []*Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := make([]*Client, 0, len(r.clients))
	for _, c := range r.clients {
		all = append(all, c)
	}
	return all
}

// allocVirtualIP finds the lowest free virtual IP in the subnet.
func (r *Registry) allocVirtualIP() (string, error) {
	for i := 0; i < 253; i++ {
		addr := fmt.Sprintf("10.0.0.%d", r.nextIP)
		r.nextIP++
		if r.nextIP > 254 {
			r.nextIP = firstClientIP
		}
		if !r.ipTaken(addr) {
			return addr, nil
		}
	}
	return "", fmt.Errorf("virtual network full")
}

func (r *Registry) ipTaken(addr string) bool {
	for _, c := range r.clients {
		if c.VirtualIP == addr {
			return true
		}
	}
	return false
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
