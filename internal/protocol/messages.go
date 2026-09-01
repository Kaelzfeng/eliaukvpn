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

	// Rooms.
	TypeRoomCreate = "room_create"  // client -> server
	TypeRoomCreated = "room_created" // server -> creator: the room code
	TypeRoomJoin   = "room_join"    // client -> server
	TypeRoomJoined = "room_joined"  // server -> joiner: room code + members
	TypeRoomLeave  = "room_leave"   // client -> server
	TypeRoomUpdate = "room_update"  // server -> every member: current member list
	TypeRoomLeft   = "room_left"    // server -> the leaver: you are out of your room
)

// RegisterRequest is the first message a client sends. The client identifies
// itself by its X25519 fingerprint (PublicKey), which the server uses as the
// stable identity key for rooms; Name is just the display name.
type RegisterRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key,omitempty"` // base64 fingerprint
}

// Registered acknowledges a successful registration.
type Registered struct {
	ClientID  string `json:"client_id"`
	VirtualIP string `json:"virtual_ip"`
	RelayAddr string `json:"relay_addr"` // server's UDP relay endpoint (M3)
	Peers     []Peer `json:"peers"`      // visible peers: same-room members
	Room      string `json:"room,omitempty"`
}

// RoomCreated is the reply to room_create.
type RoomCreated struct {
	Code string `json:"code"`
}

// RoomJoin asks to join a room by its code.
type RoomJoin struct {
	Code string `json:"code"`
}

// RoomMember is one member of a room: KeyFP is the P2P-whitelist key and
// VirtualIP the virtual-LAN address. Username is the member's display name.
// Host marks the room creator — the one most likely running a game server
// guests should join.
type RoomMember struct {
	Username  string `json:"username"`
	KeyFP     string `json:"key_fp,omitempty"`
	VirtualIP string `json:"virtual_ip,omitempty"`
	Host      bool   `json:"host,omitempty"`
}

// RoomJoined is the reply to a successful room_join: the code and every current
// member.
type RoomJoined struct {
	Code    string       `json:"code"`
	Members []RoomMember `json:"members"`
}

// RoomUpdate is pushed to every member whenever the member list changes
// (someone joined, left, or disconnected).
type RoomUpdate struct {
	Members []RoomMember `json:"members"`
}

// RoomLeft is sent to a client that left its room so it can drop the room
// state (and the room-sourced P2P whitelist entries) on its own side. The
// remaining members get a room_update instead.
type RoomLeft struct {
	Code string `json:"code"`
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
