// M7b rooms: a room is a short-code group of clients sharing one virtual LAN.
// Joining a room makes every member visible to you for hole punching (and vice
// versa), which is what makes "one-click join" work: the server hands out member
// fingerprints and the clients auto-whitelist and auto-connect to each other.
package server

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strings"

	"eliaukvpn/internal/protocol"
)

const roomCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1, no lowercase

// Room is one short-code group. Members are client fingerprints (KeyFP).
type Room struct {
	Code    string
	Host    string
	Members map[string]bool
}

// roomsByCode and inRoom live on Registry (see registry.go) so room membership
// and client state share one lock.

// CreateRoom makes a new room hosted by the given fingerprint and returns its
// code.
func (r *Registry) CreateRoom(keyFP string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inRoom[keyFP] != "" {
		return "", fmt.Errorf("已经在房间里了")
	}
	code := newRoomCode()
	for _, exists := r.rooms[code]; exists; _, exists = r.rooms[code] {
		code = newRoomCode()
	}
	room := &Room{Code: code, Host: keyFP, Members: map[string]bool{keyFP: true}}
	r.rooms[code] = room
	r.inRoom[keyFP] = code
	return code, nil
}

// JoinRoom adds the fingerprint to the room with the given code. It returns the
// room and the member list (as seen by everyone).
func (r *Registry) JoinRoom(keyFP, code string) (*Room, []protocol.RoomMember, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inRoom[keyFP] != "" {
		return nil, nil, fmt.Errorf("已经在房间里了")
	}
	room, ok := r.rooms[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return nil, nil, fmt.Errorf("房间不存在: %s", code)
	}
	room.Members[keyFP] = true
	r.inRoom[keyFP] = room.Code
	return room, r.roomMembersLocked(room), nil
}

// LeaveRoom removes the fingerprint from any room it is in and returns the
// affected room (nil if none).
func (r *Registry) LeaveRoom(keyFP string) *Room {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removeFromRoomLocked(keyFP)
}

// removeFromRoomLocked removes a fingerprint from its room (the caller holds
// r.mu), deleting the room when it empties and — if the departing member was
// the host — promoting the longest-registered remaining member so the room
// keeps a host (the game panel's "房主地址" stays meaningful after the original
// host leaves).
func (r *Registry) removeFromRoomLocked(keyFP string) *Room {
	code := r.inRoom[keyFP]
	if code == "" {
		return nil
	}
	room := r.rooms[code]
	delete(room.Members, keyFP)
	delete(r.inRoom, keyFP)
	if len(room.Members) == 0 {
		delete(r.rooms, code)
		return room
	}
	if room.Host == keyFP {
		room.Host = oldestMemberLocked(r, room)
	}
	return room
}

// oldestMemberLocked returns the room member whose client registered first —
// the natural successor when the original host leaves. "" when none of the
// remaining members resolves to an online client (should not happen: membership
// is cleaned up on disconnect).
func oldestMemberLocked(r *Registry, room *Room) string {
	var oldest string
	var oldestSeq int64
	for fp := range room.Members {
		c, ok := r.byFP[fp]
		if !ok {
			continue
		}
		if oldest == "" || c.Seq < oldestSeq {
			oldest = fp
			oldestSeq = c.Seq
		}
	}
	return oldest
}

// Room returns the room a fingerprint is currently in (nil if none).
func (r *Registry) Room(keyFP string) *Room {
	r.mu.Lock()
	defer r.mu.Unlock()
	code := r.inRoom[keyFP]
	if code == "" {
		return nil
	}
	return r.rooms[code]
}

// RoomMembers returns the current member list of the room keyFP is in.
func (r *Registry) RoomMembers(keyFP string) []protocol.RoomMember {
	r.mu.Lock()
	defer r.mu.Unlock()
	code := r.inRoom[keyFP]
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

// roomMembersLocked assembles the member list, resolving each fingerprint to
// its online client's display name and virtual IP. Room membership is cleaned up
// on disconnect (see Remove), so every fingerprint here has a live client.
func (r *Registry) roomMembersLocked(room *Room) []protocol.RoomMember {
	out := make([]protocol.RoomMember, 0, len(room.Members))
	for fp := range room.Members {
		c, ok := r.byFP[fp]
		if !ok {
			continue
		}
		out = append(out, protocol.RoomMember{
			Username:  c.Name,
			KeyFP:     c.KeyFP,
			VirtualIP: c.VirtualIP,
			Host:      fp == room.Host,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// RoomCodeOf returns the code of the room keyFP is in ("" if none).
func (r *Registry) RoomCodeOf(keyFP string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inRoom[keyFP]
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
