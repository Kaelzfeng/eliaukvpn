package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileIsFresh(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if c.Name != "" || c.Server != "" || len(c.Friends) != 0 {
		t.Fatalf("expected empty config, got %+v", c)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c := &Config{
		Name:    "Alice",
		Server:  "ws://vps.example.com:9090/ws",
		Account: "alice",
		Token:   "cafebabe",
		Friends: []Friend{
			{Name: "Bob", Code: "AAAA=="},
			{Name: "", Code: "BBBB=="},
			{Name: "", User: "carol"},
		},
	}
	if err := c.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Name != "Alice" || got.Server != "ws://vps.example.com:9090/ws" {
		t.Fatalf("round-trip name/server mismatch: %+v", got)
	}
	if got.Account != "alice" || got.Token != "cafebabe" {
		t.Fatalf("round-trip account/token mismatch: %+v", got)
	}
	if len(got.Friends) != 3 || got.Friends[0].Code != "AAAA==" || got.Friends[1].Name != "" || got.Friends[2].User != "carol" {
		t.Fatalf("round-trip friends mismatch: %+v", got.Friends)
	}
}

func TestLoadCorruptIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("corrupt config should be an error")
	}
}

func TestAddRemoveFriend(t *testing.T) {
	c := &Config{}
	if !c.AddFriend("Bob", "  AAAA==  ") {
		t.Fatal("first add should succeed")
	}
	if c.AddFriend("Bob2", "AAAA==") {
		t.Fatal("duplicate code should not be added")
	}
	if c.AddFriend("Bob3", "   ") {
		t.Fatal("blank code should not be added")
	}
	if len(c.Friends) != 1 {
		t.Fatalf("expected 1 friend, got %d", len(c.Friends))
	}
	if c.Friends[0].Name != "Bob" {
		t.Fatalf("name should be trimmed, got %q", c.Friends[0].Name)
	}
	if !c.HasFriend("AAAA==") {
		t.Fatal("HasFriend should find AAAA==")
	}
	if c.HasFriend("XXXX==") {
		t.Fatal("HasFriend should not find XXXX==")
	}
	if !c.RemoveFriend("AAAA==") {
		t.Fatal("remove should succeed")
	}
	if len(c.Friends) != 0 {
		t.Fatalf("expected 0 friends after remove, got %d", len(c.Friends))
	}
	if c.RemoveFriend("AAAA==") {
		t.Fatal("second remove should fail")
	}
}

func TestAddRemoveFriendUser(t *testing.T) {
	c := &Config{}
	if !c.AddFriendUser("  carol  ") {
		t.Fatal("first user add should succeed")
	}
	if c.AddFriendUser("carol") {
		t.Fatal("duplicate user should not be added")
	}
	if c.AddFriendUser("   ") {
		t.Fatal("blank user should not be added")
	}
	if len(c.Friends) != 1 || c.Friends[0].User != "carol" {
		t.Fatalf("expected one user friend, got %+v", c.Friends)
	}
	if !c.HasFriendUser("carol") {
		t.Fatal("HasFriendUser should find carol")
	}
	if c.HasFriendUser("dave") {
		t.Fatal("HasFriendUser should not find dave")
	}
	if !c.RemoveFriendUser("carol") {
		t.Fatal("remove user should succeed")
	}
	if len(c.Friends) != 0 {
		t.Fatalf("expected 0 friends after remove, got %d", len(c.Friends))
	}
	if c.RemoveFriendUser("carol") {
		t.Fatal("second remove should fail")
	}
}

func TestFriendCodeTrimsNewline(t *testing.T) {
	c := &Config{}
	if !c.AddFriend("x", "AAAA==\r\n") {
		t.Fatal("code with trailing newline should be trimmed and added")
	}
	if !strings.HasSuffix(c.Friends[0].Code, "==") {
		t.Fatalf("code should not retain the newline: %q", c.Friends[0].Code)
	}
}
