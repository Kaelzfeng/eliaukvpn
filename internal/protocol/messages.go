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

	// M7 accounts / friends / presence.
	TypeFriendAdd    = "friend_add"     // client -> server: add friend by username
	TypeFriendAddOk  = "friend_add_ok"  // server -> client: friend added (target resolved)
	TypeFriendRemove = "friend_remove"  // client -> server: remove friend by username
	TypeFriendList   = "friend_list"    // server -> client: full friend list with presence
	TypePresence     = "presence"       // server -> client: one friend came online/offline

	// M7 rooms.
	TypeRoomCreate = "room_create"  // client -> server
	TypeRoomCreated = "room_created" // server -> creator: the room code
	TypeRoomJoin   = "room_join"    // client -> server
	TypeRoomJoined = "room_joined"  // server -> joiner: room code + members
	TypeRoomLeave  = "room_leave"   // client -> server
	TypeRoomUpdate = "room_update"  // server -> every member: current member list
)

// RegisterRequest is the first message a client sends. M7 accounts extend it:
// account clients carry their username/password (or a cached session token)
// plus the X25519 fingerprint so the server can bind the identity to the
// account; Create=true registers a brand-new account.
type RegisterRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key,omitempty"` // base64 fingerprint (M7)
	Account   string `json:"account,omitempty"`    // username for login/create
	Password  string `json:"password,omitempty"`   // password (login) or new password (create)
	Token     string `json:"token,omitempty"`      // cached session token (re-login without password)
	Create    bool   `json:"create,omitempty"`     // create a new account
}

// Registered acknowledges a successful registration. For account clients it
// also carries the account fingerprint, the fresh session token (cache it so
// the next start needs no password) and the full friend list with presence.
type Registered struct {
	ClientID  string   `json:"client_id"`
	VirtualIP string   `json:"virtual_ip"`
	RelayAddr string   `json:"relay_addr"` // server's UDP relay endpoint (M3)
	Peers     []Peer   `json:"peers"`      // visible peers: friends + room members (M7)
	Account   string   `json:"account,omitempty"`
	KeyFP     string   `json:"key_fp,omitempty"`
	Token     string   `json:"token,omitempty"`
	Friends   []Friend `json:"friends,omitempty"`
	Room      string   `json:"room,omitempty"`
}

// Friend is one friend in the account directory: username plus the fingerprint
// the P2P whitelist needs. Online is current presence (only meaningful while
// the account is logged in somewhere).
type Friend struct {
	Username string `json:"username"`
	KeyFP    string `json:"key_fp,omitempty"`
	Online   bool   `json:"online"`
}

// FriendAdd asks to become friends with the named account (symmetric on the
// server: adding someone also adds you to their list).
type FriendAdd struct {
	Username string `json:"username"`
}

// FriendAddOk reports the resolved friend after a successful add.
type FriendAddOk struct {
	Friend Friend `json:"friend"`
}

// FriendRemove asks to drop a friend by username (symmetric).
type FriendRemove struct {
	Username string `json:"username"`
}

// FriendList is a full refresh of the account's friends with presence.
type FriendList struct {
	Friends []Friend `json:"friends"`
}

// Presence reports one friend's online/offline transition.
type Presence struct {
	Username string `json:"username"`
	Online   bool   `json:"online"`
}

// RoomCreated is the reply to room_create.
type RoomCreated struct {
	Code string `json:"code"`
}

// RoomJoin asks to join a room by its code.
type RoomJoin struct {
	Code string `json:"code"`
}

// RoomMember is one member of a room, enough for the P2P whitelist (KeyFP) and
// the virtual LAN (VirtualIP).
type RoomMember struct {
	Username  string `json:"username"`
	KeyFP     string `json:"key_fp,omitempty"`
	VirtualIP string `json:"virtual_ip,omitempty"`
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
