// Package protocol defines the JSON messages exchanged between a client and
// the coordination server over WebSocket.
package protocol

import "encoding/json"

// Envelope wraps every message with a type tag so both sides can dispatch.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Message type constants.
const (
	TypeRegister          = "register"           // client -> server
	TypeRegistered        = "registered"         // server -> client
	TypeReportEndpoint    = "report_endpoint"    // client -> server
	TypeListPeers         = "list_peers"         // client -> server
	TypePeersList         = "peers_list"         // server -> client
	TypeConnectRequest    = "connect_request"    // client -> server
	TypeConnectCandidates = "connect_candidates" // server -> both peers
	TypeError             = "error"              // server -> client
)

// RegisterRequest is the first message a client sends.
type RegisterRequest struct {
	Name string `json:"name"`
}

// Registered acknowledges a successful registration.
type Registered struct {
	ClientID  string `json:"client_id"`
	VirtualIP string `json:"virtual_ip"`
	RelayAddr string `json:"relay_addr"` // server's UDP relay endpoint (M3)
	Peers     []Peer `json:"peers"`
}

// Candidate is one punchable address (public or LAN) of a client.
type Candidate struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Type string `json:"type"` // "public" | "lan"
}

// ReportEndpoint carries the result of the client's STUN probe plus the
// punch candidates for this socket.
type ReportEndpoint struct {
	PublicIP   string      `json:"public_ip"`
	PublicPort int         `json:"public_port"`
	NATType    string      `json:"nat_type"`
	Candidates []Candidate `json:"candidates"`
}

// ConnectRequest asks the server to connect us to another peer.
type ConnectRequest struct {
	PeerID string `json:"peer_id"`
}

// ConnectCandidates is sent by the server to both sides of a requested
// connection, carrying the other side's punch candidates.
type ConnectCandidates struct {
	PeerID     string      `json:"peer_id"`
	PeerName   string      `json:"peer_name"`
	Candidates []Candidate `json:"candidates"`
}

// PeersList is a full refresh of the online peer list.
type PeersList struct {
	Peers []Peer `json:"peers"`
}

// Peer describes another connected client.
type Peer struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	VirtualIP  string `json:"virtual_ip"`
	PublicIP   string `json:"public_ip"`
	PublicPort int    `json:"public_port"`
	NATType    string `json:"nat_type"`
	Online     bool   `json:"online"`
}

// Error is the payload of a server-side error message.
type Error struct {
	Message string `json:"message"`
}
