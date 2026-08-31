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
	TypeRegister       = "register"        // client -> server
	TypeRegistered     = "registered"      // server -> client
	TypeReportEndpoint = "report_endpoint" // client -> server
	TypeListPeers      = "list_peers"      // client -> server
	TypePeersList      = "peers_list"      // server -> client
	TypeError          = "error"           // server -> client
)

// RegisterRequest is the first message a client sends.
type RegisterRequest struct {
	Name string `json:"name"`
}

// Registered acknowledges a successful registration.
type Registered struct {
	ClientID  string `json:"client_id"`
	VirtualIP string `json:"virtual_ip"`
	Peers     []Peer `json:"peers"`
}

// ReportEndpoint carries the result of the client's STUN probe.
type ReportEndpoint struct {
	PublicIP   string `json:"public_ip"`
	PublicPort int    `json:"public_port"`
	NATType    string `json:"nat_type"`
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
