package agent

import (
	"testing"

	"eliaukvpn/internal/protocol"
)

// TestRoomWhitelist verifies the effective tunnel whitelist is exactly the
// current room's members (minus self), and that each refresh recomputes it.
func TestRoomWhitelist(t *testing.T) {
	a := &Agent{identity: newTestIdentity(t)}
	self := a.identity.Fingerprint()

	fpBob := newTestIdentity(t).Fingerprint()
	fpCarol := newTestIdentity(t).Fingerprint()

	// No room: empty whitelist.
	a.mu.Lock()
	a.syncWhitelistLocked()
	a.mu.Unlock()
	assertHasFP(t, a, fpBob, false)

	// Join a room: whitelist = members minus self.
	a.setRoom(&protocol.RoomJoined{Code: "ABC", Members: []protocol.RoomMember{
		{Username: "bob", KeyFP: fpBob},
		{Username: "carol", KeyFP: fpCarol},
		{Username: "host", KeyFP: self},
	}})
	assertHasFP(t, a, fpBob, true)
	assertHasFP(t, a, fpCarol, true)
	assertHasFP(t, a, self, false)

	// Leaving the room drops all room-sourced fingerprints.
	a.mu.Lock()
	a.room = nil
	a.roomFP = nil
	a.syncWhitelistLocked()
	a.mu.Unlock()
	assertHasFP(t, a, fpBob, false)
	assertHasFP(t, a, fpCarol, false)
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
