package agent

import (
	"testing"

	"eliaukvpn/internal/protocol"
)

// TestWhitelistUnion verifies the effective tunnel whitelist is the union of
// the legacy config friends (baseFP), the M7 account directory and the current
// room members (minus self), and that each refresh recomputes it.
func TestWhitelistUnion(t *testing.T) {
	a := &Agent{myAccount: "host"}

	fpLegacy := newTestIdentity(t).Fingerprint()
	fpBob := newTestIdentity(t).Fingerprint()
	fpCarol := newTestIdentity(t).Fingerprint()
	fpSelf := newTestIdentity(t).Fingerprint()

	// Legacy config friend.
	must(t, a.AddFriend(fpLegacy))

	// Account directory: bob is a friend with fpBob.
	a.setFriendDirectory([]protocol.Friend{{Username: "bob", KeyFP: fpBob, Online: true}})
	assertHasFP(t, a, fpLegacy, true)
	assertHasFP(t, a, fpBob, true)

	// Room members: bob (already whitelisted), carol (new), and host with a
	// bogus self-fingerprint that must NOT be whitelisted.
	a.setRoom(&protocol.RoomJoined{Code: "ABC", Members: []protocol.RoomMember{
		{Username: "bob", KeyFP: fpBob},
		{Username: "carol", KeyFP: fpCarol},
		{Username: "host", KeyFP: fpSelf},
	}})
	assertHasFP(t, a, fpCarol, true)
	assertHasFP(t, a, fpBob, true)
	assertHasFP(t, a, fpLegacy, true)
	assertHasFP(t, a, fpSelf, false)

	// Leaving the room drops only the room-sourced fingerprints.
	a.mu.Lock()
	a.room = nil
	a.roomFP = nil
	a.syncWhitelistLocked()
	a.mu.Unlock()
	assertHasFP(t, a, fpCarol, false)
	assertHasFP(t, a, fpBob, true)
	assertHasFP(t, a, fpLegacy, true)

	// Friend leaving the directory drops its fingerprint too.
	a.setFriendDirectory(nil)
	assertHasFP(t, a, fpBob, false)
	assertHasFP(t, a, fpLegacy, true)
}

// TestFriendDirectoryPresence checks the directory + presence handlers.
func TestFriendDirectoryPresence(t *testing.T) {
	a := &Agent{}
	a.setFriendDirectory([]protocol.Friend{
		{Username: "zed", KeyFP: newTestIdentity(t).Fingerprint(), Online: true},
		{Username: "amy", KeyFP: newTestIdentity(t).Fingerprint()},
	})
	dir := a.FriendDirectory()
	if len(dir) != 2 || dir[0].Username != "amy" || dir[1].Username != "zed" {
		t.Fatalf("FriendDirectory not sorted: %+v", dir)
	}

	a.upsertFriend(protocol.Friend{Username: "mid", KeyFP: newTestIdentity(t).Fingerprint()})
	if dir := a.FriendDirectory(); len(dir) != 3 {
		t.Fatalf("upsertFriend did not add: %+v", dir)
	}

	a.setFriendPresence("zed", false)
	byUser := make(map[string]protocol.Friend)
	for _, f := range a.FriendDirectory() {
		byUser[f.Username] = f
	}
	if f := byUser["zed"]; f.Online {
		t.Fatal("setFriendPresence(false) did not take effect")
	}
	if f := byUser["amy"]; f.Online {
		t.Fatal("fresh friend should be offline")
	}
}

// TestAccountGating verifies every M7 action refuses to run without a logged-in
// account (before any connection exists).
func TestAccountGating(t *testing.T) {
	a := &Agent{} // no account, no conn
	if err := a.AddFriendByName("bob"); err == nil {
		t.Fatal("AddFriendByName should require a logged-in account")
	}
	if err := a.RemoveFriendByName("bob"); err == nil {
		t.Fatal("RemoveFriendByName should require a logged-in account")
	}
	if err := a.CreateRoom(); err == nil {
		t.Fatal("CreateRoom should require a logged-in account")
	}
	if err := a.JoinRoom("ABC"); err == nil {
		t.Fatal("JoinRoom should require a logged-in account")
	}
	if err := a.LeaveRoom(); err == nil {
		t.Fatal("LeaveRoom should require a logged-in account")
	}
}

func assertHasFP(t *testing.T, a *Agent, fp string, want bool) {
	t.Helper()
	for _, f := range a.Friends() {
		if f == fp {
			if !want {
				t.Fatalf("fingerprint %s unexpectedly in whitelist", fp)
			}
			return
		}
	}
	if want {
		t.Fatalf("fingerprint %s missing from whitelist: %v", fp, a.Friends())
	}
}
