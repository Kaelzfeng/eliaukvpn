package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/protocol"
)

// These tests drive the real WebSocket handler end to end: account creation,
// friend visibility, presence, room membership, and the connect_request gate.

func newTestServer(t *testing.T) (*httptest.Server, *Registry, *AccountStore) {
	t.Helper()
	reg := NewRegistry()
	acct, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg.SetFriendCheck(acct.IsFriend)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HandleWS(reg, acct, "127.0.0.1:1", w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, reg, acct
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
// peers_list/presence broadcasts that arrive as a side effect of other clients
// registering or reporting endpoints.
func readEnvUntil(t *testing.T, conn *websocket.Conn, want string) protocol.Envelope {
	t.Helper()
	for {
		env := readEnv(t, conn)
		if env.Type == want {
			return env
		}
		if env.Type != protocol.TypePeersList && env.Type != protocol.TypePresence {
			t.Fatalf("got %q while waiting for %q", env.Type, want)
		}
	}
}

// regAccount registers (creating the account when create=true) and returns the
// registered payload.
func regAccount(t *testing.T, conn *websocket.Conn, name, pw, fp string, create bool) protocol.Registered {
	t.Helper()
	sendWS(t, conn, protocol.TypeRegister, protocol.RegisterRequest{
		Name: name, Account: name, Password: pw, PublicKey: fp, Create: create,
	})
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

func TestAccountRegisterAndLegacy(t *testing.T) {
	srv, _, _ := newTestServer(t)
	conn := dialWS(t, srv)

	reg := regAccount(t, conn, "host", "pw", "fpHost", true)
	if reg.Account != "host" || reg.KeyFP != "fpHost" || reg.Token == "" {
		t.Fatalf("registered: %+v", reg)
	}
	if len(reg.Friends) != 0 || reg.Room != "" {
		t.Fatalf("fresh account should have no friends/room: %+v", reg)
	}

	// Wrong password must be rejected with an error.
	conn2 := dialWS(t, srv)
	sendWS(t, conn2, protocol.TypeRegister, protocol.RegisterRequest{
		Name: "host", Account: "host", Password: "bad", PublicKey: "fpX",
	})
	if env := readEnv(t, conn2); env.Type != protocol.TypeError {
		t.Fatalf("wrong password: got %q, want error", env.Type)
	}

	// A cached token logs in without a password.
	conn4 := dialWS(t, srv)
	sendWS(t, conn4, protocol.TypeRegister, protocol.RegisterRequest{
		Name: "host", Account: "host", Token: reg.Token, PublicKey: "fpHost",
	})
	if env := readEnv(t, conn4); env.Type != protocol.TypeRegistered {
		t.Fatalf("token login: got %q", env.Type)
	}

	// Legacy anonymous clients still work.
	conn3 := dialWS(t, srv)
	sendWS(t, conn3, protocol.TypeRegister, protocol.RegisterRequest{Name: "anon"})
	if env := readEnv(t, conn3); env.Type != protocol.TypeRegistered {
		t.Fatalf("legacy register: got %q", env.Type)
	}
}

func TestFriendVisibilityPresence(t *testing.T) {
	srv, reg, _ := newTestServer(t)
	host := dialWS(t, srv)
	join := dialWS(t, srv)
	eve := dialWS(t, srv)

	regAccount(t, host, "host", "pw", "fpHost", true)
	regAccount(t, join, "join", "pw", "fpJoin", true)
	regAccount(t, eve, "eve", "pw", "fpEve", true)

	// Before being friends, host must not see join/eve in peers_list.
	sendWS(t, host, protocol.TypeListPeers, struct{}{})
	var list protocol.PeersList
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypePeersList).Data, &list)
	if len(list.Peers) != 0 {
		t.Fatalf("strangers visible before friendship: %+v", list.Peers)
	}

	// host adds join.
	sendWS(t, host, protocol.TypeFriendAdd, protocol.FriendAdd{Username: "join"})

	var ok protocol.FriendAddOk
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeFriendAddOk).Data, &ok)
	if ok.Friend.Username != "join" || !ok.Friend.Online || ok.Friend.KeyFP != "fpJoin" {
		t.Fatalf("friend_add_ok: %+v", ok.Friend)
	}
	var fl protocol.FriendList
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeFriendList).Data, &fl)
	if len(fl.Friends) != 1 || fl.Friends[0].Username != "join" || !fl.Friends[0].Online {
		t.Fatalf("friend_list: %+v", fl.Friends)
	}
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypePeersList).Data, &list)
	if len(list.Peers) != 1 || list.Peers[0].Name != "join" {
		t.Fatalf("visible peers after add: %+v", list.Peers)
	}

	// join gets a friend_list with host (symmetric add).
	var jfl protocol.FriendList
	_ = json.Unmarshal(readEnvUntil(t, join, protocol.TypeFriendList).Data, &jfl)
	if len(jfl.Friends) != 1 || jfl.Friends[0].Username != "host" {
		t.Fatalf("join friend_list: %+v", jfl.Friends)
	}

	// join disconnects -> host gets presence {join, offline}.
	join.Close()
	var pres protocol.Presence
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypePresence).Data, &pres)
	if pres.Username != "join" || pres.Online {
		t.Fatalf("presence: %+v", pres)
	}

	// eve is not a friend: host cannot connect to her (visibility gate).
	sendWS(t, host, protocol.TypeReportEndpoint, protocol.ReportEndpoint{
		PublicIP: "1.2.3.4", PublicPort: 1000, NATType: "cone",
		Candidates: []protocol.Candidate{{IP: "1.2.3.4", Port: 1000, Type: "public"}},
	})
	sendWS(t, eve, protocol.TypeReportEndpoint, protocol.ReportEndpoint{
		PublicIP: "5.6.7.8", PublicPort: 2000, NATType: "cone",
		Candidates: []protocol.Candidate{{IP: "5.6.7.8", Port: 2000, Type: "public"}},
	})
	eveClient, _ := reg.ClientByAccount("eve")
	sendWS(t, host, protocol.TypeConnectRequest, protocol.ConnectRequest{PeerID: eveClient.ID})
	var errMsg protocol.Error
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeError).Data, &errMsg)
	if errMsg.Message == "" {
		t.Fatal("expected an error for connecting to a non-friend")
	}
}

func TestRoomLifecycle(t *testing.T) {
	srv, reg, _ := newTestServer(t)
	host := dialWS(t, srv)
	join := dialWS(t, srv)
	eve := dialWS(t, srv)

	regAccount(t, host, "host", "pw", "fpHost", true)
	regAccount(t, join, "join", "pw", "fpJoin", true)
	regAccount(t, eve, "eve", "pw", "fpEve", true)

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

	// Now host and join see each other even though they are not friends.
	sendWS(t, host, protocol.TypeListPeers, struct{}{})
	var list protocol.PeersList
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

	// Same-room members can be wired even though they are not friends.
	sendWS(t, host, protocol.TypeReportEndpoint, protocol.ReportEndpoint{
		PublicIP: "1.2.3.4", PublicPort: 1000, NATType: "cone",
		Candidates: []protocol.Candidate{{IP: "1.2.3.4", Port: 1000, Type: "public"}},
	})
	sendWS(t, eve, protocol.TypeReportEndpoint, protocol.ReportEndpoint{
		PublicIP: "5.6.7.8", PublicPort: 2000, NATType: "cone",
		Candidates: []protocol.Candidate{{IP: "5.6.7.8", Port: 2000, Type: "public"}},
	})
	eveClient, _ := reg.ClientByAccount("eve")
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
	// gone, join left). host and eve are not friends so no presence is sent.
	eve.Close()
	_ = json.Unmarshal(readEnvUntil(t, host, protocol.TypeRoomUpdate).Data, &joined)
	if len(joined.Members) != 1 || joined.Members[0].Username != "host" {
		t.Fatalf("after eve disconnect: %d members, want 1", len(joined.Members))
	}
}
