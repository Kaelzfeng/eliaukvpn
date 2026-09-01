package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/protocol"
)

// These tests drive the real WebSocket handler end to end: anonymous
// registration, room membership as the sole visibility gate, and the
// connect_request gate.

func newTestServer(t *testing.T) (*httptest.Server, *Registry) {
	t.Helper()
	reg := NewRegistry()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HandleWS(reg, "127.0.0.1:1", w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, reg
}

func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func sendWS(t *testing.T, conn *websocket.Conn, typ string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(protocol.Envelope{Type: typ, Data: raw}); err != nil {
		t.Fatal(err)
	}
}

func readEnv(t *testing.T, conn *websocket.Conn) protocol.Envelope {
	t.Helper()
	var env protocol.Envelope
	if err := conn.ReadJSON(&env); err != nil {
		t.Fatalf("read: %v", err)
	}
	return env
}

// readEnvUntil reads messages until one of the wanted type arrives, skipping
// peers_list broadcasts that arrive as a side effect of other clients
// registering or joining rooms.
func readEnvUntil(t *testing.T, conn *websocket.Conn, want string) protocol.Envelope {
	t.Helper()
	for {
		env := readEnv(t, conn)
		if env.Type == want {
			return env
		}
		if env.Type != protocol.TypePeersList {
			t.Fatalf("got %q while waiting for %q", env.Type, want)
		}
	}
}

// regClient registers an anonymous client (name + fingerprint) and returns the
// registered payload.
func regClient(t *testing.T, conn *websocket.Conn, name, fp string) protocol.Registered {
	t.Helper()
	sendWS(t, conn, protocol.TypeRegister, protocol.RegisterRequest{Name: name, PublicKey: fp})
	env := readEnv(t, conn)
	if env.Type != protocol.TypeRegistered {
		t.Fatalf("register %s: got %q", name, env.Type)
	}
	var reg protocol.Registered
	if err := json.Unmarshal(env.Data, &reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestAnonymousRegister(t *testing.T) {
	srv, reg := newTestServer(t)
	conn := dialWS(t, srv)

	regPayload := regClient(t, conn, "host", "fpHost")
	if regPayload.ClientID == "" || regPayload.VirtualIP == "" {
		t.Fatalf("registered: %+v", regPayload)
	}
	if regPayload.Room != "" {
		t.Fatalf("fresh client should not be in a room: %+v", regPayload)
	}
	if c, ok := reg.ClientByFP("fpHost"); !ok || c.Name != "host" {
		t.Fatalf("ClientByFP: %+v (ok=%v)", c, ok)
	}

	// A register missing name or fingerprint must be rejected.
	conn2 := dialWS(t, srv)
	sendWS(t, conn2, protocol.TypeRegister, protocol.RegisterRequest{PublicKey: "fpX"})
	if env := readEnv(t, conn2); env.Type != protocol.TypeError {
		t.Fatalf("missing name: got %q, want error", env.Type)
	}
	conn3 := dialWS(t, srv)
	sendWS(t, conn3, protocol.TypeRegister, protocol.RegisterRequest{Name: "noFP"})
	if env := readEnv(t, conn3); env.Type != protocol.TypeError {
		t.Fatalf("missing public key: got %q, want error", env.Type)
	}
}

func TestConnectGateOutsideRoom(t *testing.T) {
	srv, reg := newTestServer(t)
	host := dialWS(t, srv)
	eve := dialWS(t, srv)

	regClient(t, host, "host", "fpHost")
	regClient(t, eve, "eve", "fpEve")

	// Outside a room, host must not see eve (room membership is the only gate).
	sendWS(t, host, protocol.TypeListPeers, struct{}{})
	var list protocol.PeersList
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypePeersList).Data, &list)
	if len(list.Peers) != 0 {
		t.Fatalf("strangers visible outside a room: %+v", list.Peers)
	}

	sendWS(t, host, protocol.TypeReportEndpoint, protocol.ReportEndpoint{
		PublicIP: "1.2.3.4", PublicPort: 1000, NATType: "cone",
		Candidates: []protocol.Candidate{{IP: "1.2.3.4", Port: 1000, Type: "public"}},
	})
	sendWS(t, eve, protocol.TypeReportEndpoint, protocol.ReportEndpoint{
		PublicIP: "5.6.7.8", PublicPort: 2000, NATType: "cone",
		Candidates: []protocol.Candidate{{IP: "5.6.7.8", Port: 2000, Type: "public"}},
	})
	eveClient, _ := reg.ClientByFP("fpEve")
	sendWS(t, host, protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: eveClient.ID})
	var errMsg protocol.Error
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeError).Data, &errMsg)
	if errMsg.Message == "" {
		t.Fatal("expected an error for connecting to a stranger outside a room")
	}
}

func TestRoomLifecycle(t *testing.T) {
	srv, reg := newTestServer(t)
	host := dialWS(t, srv)
	join := dialWS(t, srv)
	eve := dialWS(t, srv)

	regClient(t, host, "host", "fpHost")
	regClient(t, join, "join", "fpJoin")
	regClient(t, eve, "eve", "fpEve")

	// Before any room, host sees no peers.
	sendWS(t, host, protocol.TypeListPeers, struct{}{})
	var list protocol.PeersList
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypePeersList).Data, &list)
	if len(list.Peers) != 0 {
		t.Fatalf("strangers visible before a room: %+v", list.Peers)
	}

	// host creates a room.
	sendWS(t, host, protocol.TypeRoomCreate, struct{}{})
	var created protocol.RoomCreated
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeRoomCreated).Data, &created)
	if created.Code == "" {
		t.Fatal("empty room code")
	}
	env := readEnvUntil(t, host, protocol.TypeRoomJoined)
	if env.Type != protocol.TypeRoomJoined {
		t.Fatalf("got %q, want room_joined", env.Type)
	}

	// join joins by code -> room_joined [host, join]; host gets room_update.
	sendWS(t, join, protocol.TypeRoomJoin, protocol.RoomJoin{Code: created.Code})
	var joined protocol.RoomJoined
	_ = json.Unmarshal(readEnvUntil(t, join, protocol.TypeRoomJoined).Data, &joined)
	if joined.Code != created.Code || len(joined.Members) != 2 {
		t.Fatalf("room_joined: %+v", joined)
	}
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeRoomUpdate).Data, &joined)
	if len(joined.Members) != 2 {
		t.Fatalf("host room_update: %+v", joined.Members)
	}
	// notifyRoom sends the update to every member, join included, so its queue
	// also carries one (must be drained before the next room_update assertion).
	_ = json.Unmarshal(readEnvUntil(t, join, protocol.TypeRoomUpdate).Data, &joined)
	if len(joined.Members) != 2 {
		t.Fatalf("join room_update: %+v", joined.Members)
	}

	// Now host and join see each other (same room).
	sendWS(t, host, protocol.TypeListPeers, struct{}{})
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypePeersList).Data, &list)
	if len(list.Peers) != 1 || list.Peers[0].Name != "join" {
		t.Fatalf("room members not visible: %+v", list.Peers)
	}

	// eve joins the room too.
	sendWS(t, eve, protocol.TypeRoomJoin, protocol.RoomJoin{Code: created.Code})
	_ = json.Unmarshal(readEnvUntil(t, eve, protocol.TypeRoomJoined).Data, &joined)
	if len(joined.Members) != 3 {
		t.Fatalf("eve room_joined: %+v", joined.Members)
	}
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeRoomUpdate).Data, &joined)
	if len(joined.Members) != 3 {
		t.Fatalf("host room_update: %d members, want 3", len(joined.Members))
	}
	_ = json.Unmarshal(readEnvUntil(t, join, protocol.TypeRoomUpdate).Data, &joined)
	if len(joined.Members) != 3 {
		t.Fatalf("join room_update: %d members, want 3", len(joined.Members))
	}

	// Same-room members can be wired.
	sendWS(t, host, protocol.TypeReportEndpoint, protocol.ReportEndpoint{
		PublicIP: "1.2.3.4", PublicPort: 1000, NATType: "cone",
		Candidates: []protocol.Candidate{{IP: "1.2.3.4", Port: 1000, Type: "public"}},
	})
	sendWS(t, eve, protocol.TypeReportEndpoint, protocol.ReportEndpoint{
		PublicIP: "5.6.7.8", PublicPort: 2000, NATType: "cone",
		Candidates: []protocol.Candidate{{IP: "5.6.7.8", Port: 2000, Type: "public"}},
	})
	eveClient, _ := reg.ClientByFP("fpEve")
	sendWS(t, host, protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: eveClient.ID})
	var cc protocol.ConnectCandidates
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeConnectCandidates).Data, &cc)
	if cc.PeerID == "" {
		t.Fatal("host got empty connect_candidates for same-room eve")
	}

	// join leaves -> host gets room_update with 2 members.
	sendWS(t, join, protocol.TypeRoomLeave, struct{}{})
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeRoomUpdate).Data, &joined)
	if len(joined.Members) != 2 {
		t.Fatalf("after leave: %d members, want 2", len(joined.Members))
	}

	// eve disconnects -> room_update now lists only online members (host; eve is
	// gone, join left).
	eve.Close()
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeRoomUpdate).Data, &joined)
	if len(joined.Members) != 1 || joined.Members[0].Username != "host" {
		t.Fatalf("after eve disconnect: %d members, want 1", len(joined.Members))
	}
}

// TestHostMigration verifies that when the room host leaves while members
// remain, the longest-registered remaining member is promoted to host — so the
// game panel's "房主地址" keeps resolving to a live member.
func TestHostMigration(t *testing.T) {
	srv, _ := newTestServer(t)
	host := dialWS(t, srv)
	join := dialWS(t, srv)
	eve := dialWS(t, srv)

	regClient(t, host, "host", "fpHost")
	regClient(t, join, "join", "fpJoin")
	regClient(t, eve, "eve", "fpEve")

	// host creates a room; join and eve join it.
	sendWS(t, host, protocol.TypeRoomCreate, struct{}{})
	var created protocol.RoomCreated
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeRoomCreated).Data, &created)
	readEnvUntil(t, host, protocol.TypeRoomJoined)
	sendWS(t, join, protocol.TypeRoomJoin, protocol.RoomJoin{Code: created.Code})
	readEnvUntil(t, join, protocol.TypeRoomJoined)
	readEnvUntil(t, host, protocol.TypeRoomUpdate)
	readEnvUntil(t, join, protocol.TypeRoomUpdate)
	sendWS(t, eve, protocol.TypeRoomJoin, protocol.RoomJoin{Code: created.Code})
	readEnvUntil(t, eve, protocol.TypeRoomJoined)
	readEnvUntil(t, host, protocol.TypeRoomUpdate)
	readEnvUntil(t, join, protocol.TypeRoomUpdate)
	readEnvUntil(t, eve, protocol.TypeRoomUpdate)

	// host disconnects -> join (the oldest remaining member) becomes host.
	host.Close()
	var joined protocol.RoomJoined
	_ = json.Unmarshal(readEnvUntil(t, join, protocol.TypeRoomUpdate).Data, &joined)
	if len(joined.Members) != 2 {
		t.Fatalf("after host disconnect: %d members, want 2", len(joined.Members))
	}
	var joinIsHost, eveIsHost bool
	for _, m := range joined.Members {
		switch m.Username {
		case "join":
			joinIsHost = m.Host
		case "eve":
			eveIsHost = m.Host
		}
	}
	if !joinIsHost {
		t.Fatal("join should have been promoted to host after host disconnect")
	}
	if eveIsHost {
		t.Fatal("eve must not become host; join registered first")
	}
	// eve receives the same update.
	_ = json.Unmarshal(readEnvUntil(t, eve, protocol.TypeRoomUpdate).Data, &joined)
}
