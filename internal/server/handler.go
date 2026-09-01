package server

import (
	"encoding/json"
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

// HandleWS upgrades one client connection, drives the register handshake and
// then relays control messages for the lifetime of the connection. Game traffic
// does NOT flow through this server — that goes peer-to-peer (M2+), or through
// the UDP relay (M3) when NAT punching fails.
func HandleWS(reg *Registry, relayAddr string, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade failed: %v", err)
		return
	}

	// The first frame must be a register request carrying the client's display
	// name and its X25519 fingerprint (its stable identity for room membership).
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
	if req.Name == "" {
		sendError(conn, "register requires a non-empty 'name'")
		conn.Close()
		return
	}
	keyFP := req.PublicKey
	if keyFP == "" {
		sendError(conn, "register requires a non-empty 'public_key'")
		conn.Close()
		return
	}

	client, err := reg.Add(req.Name, keyFP, conn)
	if err != nil {
		sendError(conn, err.Error())
		conn.Close()
		return
	}
	defer func() {
		// Capture the room *before* Remove: Remove drops the client from inRoom,
		// so reg.Room() would return nil afterwards and the remaining members
		// would never learn the room shrank. notifyRoom runs after Remove, so its
		// member list already excludes this client.
		room := reg.Room(keyFP)
		reg.Remove(client.ID)
		if room != nil {
			notifyRoom(reg, room)
		}
		broadcastPeers(reg)
		log.Printf("client left: id=%s", client.ID)
	}()

	// Heartbeat: keep the link alive through an idle timeout and tear down a
	// dead client so its virtual IP / room membership are released. Registered
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

	log.Printf("client joined: id=%s name=%q virtual_ip=%s", client.ID, client.Name, client.VirtualIP)

	regMsg := protocol.Registered{
		ClientID:  client.ID,
		VirtualIP: client.VirtualIP,
		RelayAddr: relayAddr,
		Peers:     reg.VisiblePeers(client.ID, client.KeyFP),
		Room:      reg.RoomCodeOf(client.KeyFP),
	}
	_ = sendClient(client, protocol.TypeRegistered, regMsg)

	broadcastPeers(reg)

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
			broadcastPeers(reg)
		case protocol.TypeListPeers:
			_ = sendClient(client, protocol.TypePeersList, protocol.PeersList{Peers: reg.VisiblePeers(client.ID, client.KeyFP)})
		case protocol.TypeConnectRequest:
			handleConnectRequest(reg, client, env.Data)
		case protocol.TypeRoomCreate:
			handleRoomCreate(reg, client)
		case protocol.TypeRoomJoin:
			handleRoomJoin(reg, client, env.Data)
		case protocol.TypeRoomLeave:
			handleRoomLeave(reg, client)
		default:
			log.Printf("unknown message type %q from %s", env.Type, client.ID)
		}
	}
}

// handleConnectRequest wires two clients together for hole punching: both
// sides receive the other's punch candidates and start punching on their own.
// The two must be in the same room.
func handleConnectRequest(reg *Registry, client *Client, data json.RawMessage) {
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
		sendClient(client, protocol.TypeError, protocol.Error{Message: "peer is not in your room"})
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

// handleRoomCreate makes a new room and joins the creator.
func handleRoomCreate(reg *Registry, client *Client) {
	code, err := reg.CreateRoom(client.KeyFP)
	if err != nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: err.Error()})
		return
	}
	log.Printf("room: %s created %s", client.Name, code)
	_ = sendClient(client, protocol.TypeRoomCreated, protocol.RoomCreated{Code: code})
	members := reg.RoomMembers(client.KeyFP)
	_ = sendClient(client, protocol.TypeRoomJoined, protocol.RoomJoined{Code: code, Members: members})
}

// handleRoomJoin joins a room by code and notifies all members.
func handleRoomJoin(reg *Registry, client *Client, data json.RawMessage) {
	var req protocol.RoomJoin
	if err := json.Unmarshal(data, &req); err != nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "bad room_join"})
		return
	}
	room, members, err := reg.JoinRoom(client.KeyFP, req.Code)
	if err != nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: err.Error()})
		return
	}
	log.Printf("room: %s joined %s", client.Name, room.Code)
	_ = sendClient(client, protocol.TypeRoomJoined, protocol.RoomJoined{Code: room.Code, Members: members})
	notifyRoom(reg, room)
	broadcastPeers(reg)
}

// handleRoomLeave leaves the current room (if any) and notifies the rest. The
// leaver gets a direct room_left so it can drop its room state and the
// room-sourced P2P whitelist entries; the remaining members get a room_update.
func handleRoomLeave(reg *Registry, client *Client) {
	room := reg.LeaveRoom(client.KeyFP)
	if room == nil {
		sendClient(client, protocol.TypeError, protocol.Error{Message: "不在房间里"})
		return
	}
	log.Printf("room: %s left %s", client.Name, room.Code)
	_ = sendClient(client, protocol.TypeRoomLeft, protocol.RoomLeft{Code: room.Code})
	notifyRoom(reg, room)
	broadcastPeers(reg)
}

// notifyRoom pushes the current member list to every online member of a room.
func notifyRoom(reg *Registry, room *Room) {
	members := reg.RoomMembersList(room)
	for fp := range room.Members {
		if mc, ok := reg.ClientByFP(fp); ok {
			_ = sendClient(mc, protocol.TypeRoomUpdate, protocol.RoomUpdate{Members: members})
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
func broadcastPeers(reg *Registry) {
	for _, c := range reg.All() {
		_ = sendClient(c, protocol.TypePeersList, protocol.PeersList{Peers: reg.VisiblePeers(c.ID, c.KeyFP)})
	}
}

func sendError(conn *websocket.Conn, msg string) {
	raw, _ := json.Marshal(protocol.Error{Message: msg})
	_ = conn.WriteJSON(protocol.Envelope{Type: protocol.TypeError, Data: raw})
}
