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

// Client is one connected peer in the virtual network. M7 accounts bind a
// client to an Account (Account holds the username; KeyFP is the fingerprint
// the P2P whitelist keys on).
type Client struct {
	ID         string
	Name       string
	Account    string // "" for legacy anonymous clients
	KeyFP      string // base64 X25519 fingerprint (M7)
	VirtualIP  string
	PublicIP   string
	PublicPort int
	NATType    string
	Candidates []protocol.Candidate
	Conn       *websocket.Conn
	writeMu    sync.Mutex // serializes writes so broadcasts don't interleave
}

// Registry keeps track of connected clients, rooms, and hands out virtual IPs.
type Registry struct {
	mu        sync.Mutex
	clients   map[string]*Client
	byAccount map[string]*Client // account username -> client (online accounts only)
	nextIP    int
	relayAddr map[string]*net.UDPAddr // client id -> address it relays from

	rooms  map[string]*Room    // room code -> room
	inRoom map[string]string   // account username -> room code

	isFriend func(user, friend string) bool // account friend graph (M7)
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		clients:   make(map[string]*Client),
		byAccount: make(map[string]*Client),
		nextIP:    firstClientIP,
		relayAddr: make(map[string]*net.UDPAddr),
		rooms:     make(map[string]*Room),
		inRoom:    make(map[string]string),
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

// Add registers a new client and assigns it a virtual IP. account and keyFP
// are empty for legacy anonymous clients.
func (r *Registry) Add(name, account, keyFP string, conn *websocket.Conn) (*Client, error) {
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
		Account:   account,
		KeyFP:     keyFP,
		VirtualIP: ip,
		Conn:      conn,
	}
	r.clients[id] = c
	if account != "" {
		r.byAccount[account] = c
	}
	return c, nil
}

// Remove deletes a client (e.g. on disconnect). It also drops the account from
// any room; the caller is responsible for notifying the remaining members.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[id]; ok && c.Account != "" {
		delete(r.byAccount, c.Account)
		if code := r.inRoom[c.Account]; code != "" {
			if room := r.rooms[code]; room != nil {
				delete(room.Members, c.Account)
				if len(room.Members) == 0 {
					delete(r.rooms, code)
				}
			}
			delete(r.inRoom, c.Account)
		}
	}
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

// Peers returns one protocol.Peer per connected client, excluding the given id
// (legacy: everyone is visible). Account-aware callers use VisiblePeers.
func (r *Registry) Peers(excludeID string) []protocol.Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peersAllLocked(excludeID)
}

func (r *Registry) peersAllLocked(excludeID string) []protocol.Peer {
	peers := make([]protocol.Peer, 0, len(r.clients))
	for _, c := range r.clients {
		if c.ID == excludeID {
			continue
		}
		peers = append(peers, peerOf(c))
	}
	return peers
}

// VisiblePeers is the M7 visibility rule: for account clients, only friends and
// same-room members are visible (that is the entire P2P connectivity surface —
// you can punch toward, and be discovered by, exactly the people you can see).
// Legacy anonymous clients still see everyone (no friend check configured).
func (r *Registry) VisiblePeers(excludeID, account string) []protocol.Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if account == "" || r.isFriend == nil {
		return r.peersAllLocked(excludeID)
	}
	code := r.inRoom[account]
	peers := make([]protocol.Peer, 0, len(r.clients))
	for _, c := range r.clients {
		if c.ID == excludeID || c.Account == account {
			continue
		}
		if code != "" && r.inRoom[c.Account] == code {
			peers = append(peers, peerOf(c))
			continue
		}
		if r.isFriend(account, c.Account) {
			peers = append(peers, peerOf(c))
		}
	}
	return peers
}

// VisibleTo reports whether src may ask to connect to dst (friends or same
// room). Legacy clients (no account) are visible to everyone.
func (r *Registry) VisibleTo(src, dst *Client) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if src.Account == "" || dst.Account == "" || r.isFriend == nil {
		return true
	}
	if src.Account == dst.Account {
		return false
	}
	if code := r.inRoom[src.Account]; code != "" && r.inRoom[dst.Account] == code {
		return true
	}
	return r.isFriend(src.Account, dst.Account)
}

// SetFriendCheck installs the account friend-graph lookup used for visibility.
func (r *Registry) SetFriendCheck(f func(user, friend string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.isFriend = f
}

// ClientByAccount returns the online client of an account (nil if offline).
func (r *Registry) ClientByAccount(username string) (*Client, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.byAccount[username]
	return c, ok
}

func peerOf(c *Client) protocol.Peer {
	return protocol.Peer{
		ID:         c.ID,
		Name:       c.Name,
		VirtualIP:  c.VirtualIP,
		PublicIP:   c.PublicIP,
		PublicPort: c.PublicPort,
		NATType:    c.NATType,
		Online:     true,
	}
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
