package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/protocol"
)

var upgrader = websocket.Upgrader{
	// M1: accept any origin. Lock this down before any production use.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// HandleWS upgrades one client connection, drives the register handshake and
// then relays control messages for the lifetime of the connection. Game
// traffic does NOT flow through this server — that goes peer-to-peer (M2+).
func HandleWS(reg *Registry, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade failed: %v", err)
		return
	}

	// The first frame must be a register request.
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
	if err := json.Unmarshal(env.Data, &req); err != nil || req.Name == "" {
		sendError(conn, "register requires a non-empty 'name'")
		conn.Close()
		return
	}

	client, err := reg.Add(req.Name, conn)
	if err != nil {
		sendError(conn, err.Error())
		conn.Close()
		return
	}
	defer func() {
		reg.Remove(client.ID)
		broadcastPeers(reg)
		log.Printf("client left: id=%s", client.ID)
	}()

	log.Printf("client joined: id=%s name=%q virtual_ip=%s", client.ID, client.Name, client.VirtualIP)

	_ = sendClient(client, protocol.TypeRegistered, protocol.Registered{
		ClientID:  client.ID,
		VirtualIP: client.VirtualIP,
		Peers:     reg.Peers(client.ID),
	})
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
			_ = sendClient(client, protocol.TypePeersList, protocol.PeersList{Peers: reg.Peers(client.ID)})
		case protocol.TypeConnectRequest:
			handleConnectRequest(reg, client, env.Data)
		default:
			log.Printf("unknown message type %q from %s", env.Type, client.ID)
		}
	}
}

// handleConnectRequest wires two clients together for hole punching: both
// sides receive the other's punch candidates and start punching on their own.
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

// broadcastPeers pushes the current peer list to every connected client
// (each list excluding the recipient itself).
func broadcastPeers(reg *Registry) {
	for _, c := range reg.All() {
		_ = sendClient(c, protocol.TypePeersList, protocol.PeersList{Peers: reg.Peers(c.ID)})
	}
}

func sendError(conn *websocket.Conn, msg string) {
	raw, _ := json.Marshal(protocol.Error{Message: msg})
	_ = conn.WriteJSON(protocol.Envelope{Type: protocol.TypeError, Data: raw})
}
