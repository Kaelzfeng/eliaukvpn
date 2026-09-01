package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/protocol"
)

var upgrader = websocket.Upgrader{
	// M1: accept any origin. Lock this down before any production use.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WebSocket heartbeat timing (server side; the client mirrors these). The
// server pings every serverPingPeriod and requires a pong within
// serverPongWait, keeping an idle link alive through an idle timeout such as
// Cloudflare's ~100s cutoff and detecting dead clients so their registration is
// released.
const (
	serverPongWait   = 60 * time.Second
	serverPingPeriod = 20 * time.Second
	serverWriteWait  = 10 * time.Second
)

// HandleWS upgrades one client connection, drives the register handshake (which
// in M7 authenticates an account or creates one) and then relays control
// messages for the lifetime of the connection. Game traffic does NOT flow
// through this server — that goes peer-to-peer (M2+), or through the UDP relay
// (M3) when NAT punching fails.
func HandleWS(reg *Registry, acct *AccountStore, relayAddr string, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade failed: %v", err)
		return
	}

	// The first frame must be a register request. For M7 accounts it carries
	// account/password/token (Create=true creates a new account); legacy
	// anonymous clients just send a name.
	var env protocol.Envelope
	if err := conn.ReadJSON(&env); err != nil {
		conn.Close()
		return
	}
	if env.Type != protocol.TypeRegister {
		sendError(conn, "first message must be 'register'")
		conn.Close()
		return
	}
	var req protocol.RegisterRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		sendError(conn, "malformed register")
		conn.Close()
		return
	}

	var acc *Account
	var keyFP string
	if req.Account != "" {
		// Account flow: create or authenticate.
		if req.Create {
			acc, err = acct.Create(req.Account, req.Password, req.PublicKey)
		} else {
			var ok bool
			acc, ok, err = acct.Authenticate(req.Account, req.Password, req.Token)
			if err == nil && ok {
				if err = acct.RegisterDevice(acc, req.PublicKey); err == nil {
					var token string
					token, err = acct.RotateToken(acc)
					acc.Token = token // reflect the fresh token in the reply
				}
			} else if err == nil {
				err = fmt.Errorf("账号或密码错误")
			}
		}
		if err != nil {
			sendError(conn, err.Error())
			conn.Close()
			return
		}
		keyFP = req.PublicKey
		if keyFP == "" && len(acc.Devices) > 0 {
			keyFP = acc.Devices[0]
		}
		if req.Name == "" {
			req.Name = req.Account
		}
	} else if req.Name == "" {
		sendError(conn, "register requires a non-empty 'name' (or an account)")
		conn.Close()
		return
	}

	client, err := reg.Add(req.Name, req.Account, keyFP, conn)
	if err != nil {
		sendError(conn, err.Error())
		conn.Close()
		return
	}
	account := client.Account
	defer func() {
		// Capture the room *before* Remove: Remove drops the account from inRoom,
		// so reg.Room() would return nil afterwards and the remaining members
		// would never learn the room shrank. notifyRoom runs after Remove, so its
		// member list already excludes this account.
		room := reg.Room(account)
		reg.Remove(client.ID)
		if account != "" {
			// Friends see me go offline; room members get the new member list.
			notifyPresence(reg, acct, account, false)
			if room != nil {
				notifyRoom(reg, room)
			}
		}
		broadcastPeers(reg, acct)
		log.Printf("client left: id=%s", client.ID)
	}()

	// Heartbeat: keep the link alive through an idle timeout and tear down a
	// dead client so its virtual IP / account binding are released. Registered
	// after the cleanup defer so close(done) runs first and the ping goroutine
	// stops before Remove/broadcastPeers touch the connection.
	conn.SetReadDeadline(time.Now().Add(serverPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(serverPongWait))
		return nil
	})
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(serverPingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				client.writeMu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(serverWriteWait))
				client.writeMu.Unlock()
				if err != nil {
					conn.Close() // force the read loop below to return
					return
				}
			}
		}
	}()

	log.Printf("client joined: id=%s name=%q account=%q virtual_ip=%s", client.ID, client.Name, account, client.VirtualIP)

	regMsg := protocol.Registered{
		ClientID:  client.ID,
		VirtualIP: client.VirtualIP,
		RelayAddr: relayAddr,
		Peers:     reg.VisiblePeers(client.ID, account),
	}
	if account != "" {
		regMsg.Account = account
		regMsg.KeyFP = keyFP
		regMsg.Token = acc.Token
		regMsg.Friends = buildFriendList(reg, acct, account)
		regMsg.Room = reg.RoomCodeOf(account)
	}
	_ = sendClient(client, protocol.TypeRegistered, regMsg)

	if account != "" {
		notifyPresence(reg, acct, account, true)
	}
	broadcastPeers(reg, acct)

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return // client disconnected
		}
		switch env.Type {
		case protocol.TypeReportEndpoint:
			var ep protocol.ReportEndpoint
			if err := json.Unmarshal(env.Data, &ep); err != nil {
				continue
			}
			reg.UpdateEndpoint(client.ID, ep.PublicIP, ep.PublicPort, ep.NATType, ep.Candidates)
			log.Printf("client %s endpoint: %s (%s)", client.ID, ep.PublicIP, ep.NATType)
			broadcastPeers(reg, acct)
		case protocol.TypeListPeers:
			_ = sendClient(client, protocol.TypePeersList, protocol.PeersList{Peers: reg.VisiblePeers(client.ID, account)})
		case protocol.TypeConnectRequest:
			handleConnectRequest(reg, acct, client, env.Data)
		case protocol.TypeFriendAdd:
			handleFriendAdd(reg, acct, client, env.Data)
		case protocol.TypeFriendRemove:
			handleFriendRemove(reg, acct, client, env.Data)
		case protocol.TypeRoomCreate:
			handleRoomCreate(reg, acct, client)
		case protocol.TypeRoomJoin:
			handleRoomJoin(reg, acct, client, env.Data)
		case protocol.TypeRoomLeave:
			handleRoomLeave(reg, acct, client)
		default:
			log.Printf("unknown message type %q from %s", env.Type, client.ID)
		}
	}
}

// handleConnectRequest wires two clients together for hole punching: both
// sides receive the other's punch candidates and start punching on their own.
// M7: the two must be visible to each other (friends or same room).
func handleConnectRequest(reg *Registry, acct *AccountStore, client *Client, data json.RawMessage) {
	var req protocol.ConnectRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "bad connect_request"})
		return
	}
	target, ok := reg.Client(req.PeerID)
	if !ok {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "peer not found: " + req.PeerID})
		return
	}
	if target.ID == client.ID {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "cannot connect to yourself"})
		return
	}
	if !reg.VisibleTo(client, target) {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "peer is not your friend or in your room"})
		return
	}
	srcCands := reg.Candidates(client.ID)
	dstCands := reg.Candidates(target.ID)
	if len(srcCands) == 0 || len(dstCands) == 0 {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "peer has not reported candidates yet"})
		return
	}
	_ = sendClient(client, protocol.TypeConnectCandidates, protocol.ConnectCandidates{
		PeerID:     target.ID,
		PeerName:   target.Name,
		Candidates: dstCands,
	})
	_ = sendClient(target, protocol.TypeConnectCandidates, protocol.ConnectCandidates{
		PeerID:     client.ID,
		PeerName:   client.Name,
		Candidates: srcCands,
	})
	log.Printf("punch: %s(%s) <-> %s(%s)", client.Name, client.ID, target.Name, target.ID)
}

// handleFriendAdd makes two accounts friends (symmetric on the server), then
// refreshes the visible peer list and presence on both sides.
func handleFriendAdd(reg *Registry, acct *AccountStore, client *Client, data json.RawMessage) {
	var req protocol.FriendAdd
	if err := json.Unmarshal(data, &req); err != nil || client.Account == "" {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "好友添加需要账号"})
		return
	}
	if err := acct.AddFriend(client.Account, req.Username); err != nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: err.Error()})
		return
	}
	log.Printf("friend: %s + %s", client.Account, req.Username)
	_ = sendClient(client, protocol.TypeFriendAddOk, protocol.FriendAddOk{Friend: friendProto(reg, acct, req.Username)})
	if fc, ok := reg.ClientByAccount(req.Username); ok {
		// The new friend is online: they now see us, and we both get fresh lists.
		_ = sendClient(fc, protocol.TypeFriendList, protocol.FriendList{Friends: buildFriendList(reg, acct, req.Username)})
		_ = sendClient(fc, protocol.TypePeersList, protocol.PeersList{Peers: reg.VisiblePeers(fc.ID, fc.Account)})
	}
	_ = sendClient(client, protocol.TypeFriendList, protocol.FriendList{Friends: buildFriendList(reg, acct, client.Account)})
	_ = sendClient(client, protocol.TypePeersList, protocol.PeersList{Peers: reg.VisiblePeers(client.ID, client.Account)})
}

// handleFriendRemove drops a friendship (symmetric) and refreshes visibility.
func handleFriendRemove(reg *Registry, acct *AccountStore, client *Client, data json.RawMessage) {
	var req protocol.FriendRemove
	if err := json.Unmarshal(data, &req); err != nil || client.Account == "" {
		return
	}
	_ = acct.RemoveFriend(client.Account, req.Username)
	if fc, ok := reg.ClientByAccount(req.Username); ok {
		_ = sendClient(fc, protocol.TypeFriendList, protocol.FriendList{Friends: buildFriendList(reg, acct, req.Username)})
		_ = sendClient(fc, protocol.TypePeersList, protocol.PeersList{Peers: reg.VisiblePeers(fc.ID, fc.Account)})
	}
	_ = sendClient(client, protocol.TypeFriendList, protocol.FriendList{Friends: buildFriendList(reg, acct, client.Account)})
	_ = sendClient(client, protocol.TypePeersList, protocol.PeersList{Peers: reg.VisiblePeers(client.ID, client.Account)})
}

// handleRoomCreate makes a new room and joins the creator.
func handleRoomCreate(reg *Registry, acct *AccountStore, client *Client) {
	if client.Account == "" {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "建房需要账号"})
		return
	}
	code, err := reg.CreateRoom(client.Account)
	if err != nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: err.Error()})
		return
	}
	log.Printf("room: %s created %s", client.Account, code)
	_ = sendClient(client, protocol.TypeRoomCreated, protocol.RoomCreated{Code: code})
	members := reg.RoomMembers(client.Account)
	_ = sendClient(client, protocol.TypeRoomJoined, protocol.RoomJoined{Code: code, Members: members})
}

// handleRoomJoin joins a room by code and notifies all members.
func handleRoomJoin(reg *Registry, acct *AccountStore, client *Client, data json.RawMessage) {
	var req protocol.RoomJoin
	if err := json.Unmarshal(data, &req); err != nil || client.Account == "" {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "加入房间需要账号"})
		return
	}
	room, members, err := reg.JoinRoom(client.Account, req.Code)
	if err != nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: err.Error()})
		return
	}
	log.Printf("room: %s joined %s", client.Account, room.Code)
	_ = sendClient(client, protocol.TypeRoomJoined, protocol.RoomJoined{Code: room.Code, Members: members})
	notifyRoom(reg, room)
	broadcastPeers(reg, acct)
}

// handleRoomLeave leaves the current room (if any) and notifies the rest. The
// leaver gets a direct room_left so it can drop its room state and the
// room-sourced P2P whitelist entries; the remaining members get a room_update.
func handleRoomLeave(reg *Registry, acct *AccountStore, client *Client) {
	if client.Account == "" {
		return
	}
	room := reg.LeaveRoom(client.Account)
	if room == nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "不在房间里"})
		return
	}
	log.Printf("room: %s left %s", client.Account, room.Code)
	_ = sendClient(client, protocol.TypeRoomLeft, protocol.RoomLeft{Code: room.Code})
	notifyRoom(reg, room)
	broadcastPeers(reg, acct)
}

// notifyRoom pushes the current member list to every online member of a room.
func notifyRoom(reg *Registry, room *Room) {
	members := reg.RoomMembersList(room)
	for u := range room.Members {
		if mc, ok := reg.ClientByAccount(u); ok {
			_ = sendClient(mc, protocol.TypeRoomUpdate, protocol.RoomUpdate{Members: members})
		}
	}
}

// buildFriendList resolves an account's friends to protocol.Friend entries with
// presence and a fingerprint for the P2P whitelist.
func buildFriendList(reg *Registry, acct *AccountStore, username string) []protocol.Friend {
	var out []protocol.Friend
	for _, f := range acct.FriendList(username) {
		out = append(out, friendProto(reg, acct, f))
	}
	return out
}

func friendProto(reg *Registry, acct *AccountStore, username string) protocol.Friend {
	f := protocol.Friend{Username: username}
	if a, ok := acct.Get(username); ok && len(a.Devices) > 0 {
		f.KeyFP = a.Devices[0]
	}
	if _, ok := reg.ClientByAccount(username); ok {
		f.Online = true
	}
	return f
}

// notifyPresence tells every online friend of username that it came/goes
// online.
func notifyPresence(reg *Registry, acct *AccountStore, username string, online bool) {
	for _, friend := range acct.FriendList(username) {
		if fc, ok := reg.ClientByAccount(friend); ok {
			_ = sendClient(fc, protocol.TypePresence, protocol.Presence{Username: username, Online: online})
		}
	}
}

// sendClient writes an envelope to one client, serialized by the client's
// write mutex so concurrent broadcasts from other handlers can't interleave.
func sendClient(c *Client, typ string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.Conn.WriteJSON(protocol.Envelope{Type: typ, Data: raw})
}

// broadcastPeers pushes the current *visible* peer list to every connected
// client (each list excluding the recipient itself).
func broadcastPeers(reg *Registry, acct *AccountStore) {
	for _, c := range reg.All() {
		_ = sendClient(c, protocol.TypePeersList, protocol.PeersList{Peers: reg.VisiblePeers(c.ID, c.Account)})
	}
}

func sendError(conn *websocket.Conn, msg string) {
	raw, _ := json.Marshal(protocol.Error{Message: msg})
	_ = conn.WriteJSON(protocol.Envelope{Type: protocol.TypeError, Data: raw})
}
