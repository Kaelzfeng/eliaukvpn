package agent

import (
	"encoding/base64"
	"testing"

	"eliaukvpn/internal/crypto"
)

// TestFriendList verifies the runtime friend allowlist API (used by the GUI)
// without needing a live server: AddFriend validates + dedups + round-trips
// through Friends(), RemoveFriend deletes, and invalid codes are rejected.
func TestFriendList(t *testing.T) {
	a := &Agent{}
	id1 := newTestIdentity(t)

	fp1 := id1.Fingerprint()
	if err := a.AddFriend(fp1); err != nil {
		t.Fatalf("AddFriend: %v", err)
	}
	if got := a.Friends(); len(got) != 1 || got[0] != fp1 {
		t.Fatalf("Friends() = %v, want [%s]", got, fp1)
	}

	// Dedup: adding the same friend again is a no-op.
	if err := a.AddFriend(fp1); err != nil {
		t.Fatalf("AddFriend (dup): %v", err)
	}
	if got := a.Friends(); len(got) != 1 {
		t.Fatalf("duplicate add grew the list: %v", got)
	}

	// Invalid codes are rejected.
	if err := a.AddFriend("not-base64!!"); err == nil {
		t.Fatal("invalid fingerprint should be rejected")
	}
	if err := a.AddFriend(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("wrong-length fingerprint should be rejected")
	}

	// Remove.
	if err := a.RemoveFriend(fp1); err != nil {
		t.Fatalf("RemoveFriend: %v", err)
	}
	if got := a.Friends(); len(got) != 0 {
		t.Fatalf("Friends() after remove = %v, want empty", got)
	}
	if err := a.RemoveFriend(fp1); err == nil {
		t.Fatal("removing a missing friend should error")
	}

	// Add two, remove the first, keep the second.
	fp2 := newTestIdentity(t).Fingerprint()
	must(t, a.AddFriend(fp1))
	must(t, a.AddFriend(fp2))
	must(t, a.RemoveFriend(fp1))
	if got := a.Friends(); len(got) != 1 || got[0] != fp2 {
		t.Fatalf("Friends() after remove-first = %v, want [%s]", got, fp2)
	}
}

// TestFriendsKeptBeforeRegistration makes sure AddFriend before the tunnel
// exists is safe and still reflected in Friends() (onRegistered re-syncs the
// tunnel with a.friends).
func TestFriendsKeptBeforeRegistration(t *testing.T) {
	a := &Agent{}
	fp := newTestIdentity(t).Fingerprint()
	must(t, a.AddFriend(fp)) // tunnel == nil here; must not panic
	if got := a.Friends(); len(got) != 1 || got[0] != fp {
		t.Fatalf("friend lost before registration: %v", got)
	}
}

func newTestIdentity(t *testing.T) *crypto.Identity {
	t.Helper()
	id, err := crypto.GenerateIdentity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
