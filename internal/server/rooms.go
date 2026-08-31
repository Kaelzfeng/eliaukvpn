// M7 rooms: a room is a short-code group of accounts sharing one virtual LAN.
// Joining a room makes every member visible to you for hole punching (and vice
// versa) regardless of friendship, which is what makes "one-click join" work:
// the server hands out member fingerprints and the clients auto-whitelist and
// auto-connect to each other.
package server

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strings"

	"eliaukvpn/internal/protocol"
)

const roomCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1, no lowercase

// Room is one short-code group. Members are account usernames.
type Room struct {
	Code    string
	Host    string
	Members map[string]bool
}

// roomsByCode and inRoom live on Registry (see registry.go) so room membership
// and client state share one lock.

// CreateRoom makes a new room hosted by username and returns its code.
func (r *Registry) CreateRoom(username string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inRoom[username] != "" {
		return "", fmt.Errorf("已经在房间里了")
	}
	code := newRoomCode()
	for _, exists := r.rooms[code]; exists; _, exists = r.rooms[code] {
		code = newRoomCode()
	}
	room := &Room{Code: code, Host: username, Members: map[string]bool{username: true}}
	r.rooms[code] = room
	r.inRoom[username] = code
	return code, nil
}

// JoinRoom adds username to the room with the given code. It returns the room
// and the member list (as seen by everyone).
func (r *Registry) JoinRoom(username, code string) (*Room, []protocol.RoomMember, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inRoom[username] != "" {
		return nil, nil, fmt.Errorf("已经在房间里了")
	}
	room, ok := r.rooms[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return nil, nil, fmt.Errorf("房间不存在: %s", code)
	}
	room.Members[username] = true
	r.inRoom[username] = room.Code
	return room, r.roomMembersLocked(room), nil
}

// LeaveRoom removes username from any room it is in and returns the affected
// room (nil if none). Empty rooms are deleted.
func (r *Registry) LeaveRoom(username string) *Room {
	r.mu.Lock()
	defer r.mu.Unlock()
	code := r.inRoom[username]
	if code == "" {
		return nil
	}
	room := r.rooms[code]
	delete(room.Members, username)
	delete(r.inRoom, username)
	if len(room.Members) == 0 {
		delete(r.rooms, code)
	}
	return room
}

// Room returns the room an account is currently in (nil if none).
func (r *Registry) Room(account string) *Room {
	r.mu.Lock()
	defer r.mu.Unlock()
	code := r.inRoom[account]
	if code == "" {
		return nil
	}
	return r.rooms[code]
}

// RoomMembers returns the current member list of the room username is in.
func (r *Registry) RoomMembers(username string) []protocol.RoomMember {
	r.mu.Lock()
	defer r.mu.Unlock()
	code := r.inRoom[username]
	if code == "" {
		return nil
	}
	return r.roomMembersLocked(r.rooms[code])
}

// RoomMembersList returns the member list of an existing room.
func (r *Registry) RoomMembersList(room *Room) []protocol.RoomMember {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.roomMembersLocked(room)
}

// roomMembersLocked assembles the member list (online members only), resolving
// each account's fingerprint and the online member's virtual IP.
func (r *Registry) roomMembersLocked(room *Room) []protocol.RoomMember {
	names := make([]string, 0, len(room.Members))
	for u := range room.Members {
		names = append(names, u)
	}
	sort.Strings(names)
	out := make([]protocol.RoomMember, 0, len(names))
	for _, u := range names {
		m := protocol.RoomMember{Username: u, Host: u == room.Host}
		if c, ok := r.byAccount[u]; ok {
			m.KeyFP = c.KeyFP
			m.VirtualIP = c.VirtualIP
		}
		out = append(out, m)
	}
	return out
}

// RoomCodeOf returns the code of the room username is in ("" if none).
func (r *Registry) RoomCodeOf(username string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inRoom[username]
}

func newRoomCode() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	var sb strings.Builder
	for _, x := range b {
		sb.WriteByte(roomCodeAlphabet[int(x)%len(roomCodeAlphabet)])
	}
	return sb.String()
}
