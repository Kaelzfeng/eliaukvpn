package server

import (
	"path/filepath"
	"testing"
)

func TestAccountCreateAuthenticate(t *testing.T) {
	s, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Create("host", "hunter2", "fpHost")
	if err != nil {
		t.Fatal(err)
	}
	if a.Username != "host" || len(a.Devices) != 1 || a.Devices[0] != "fpHost" {
		t.Fatalf("create: %+v", a)
	}
	if _, err := s.Create("host", "x", "fpX"); err == nil {
		t.Fatal("duplicate username must fail")
	}
	if _, ok, _ := s.Authenticate("host", "hunter2", ""); !ok {
		t.Fatal("wrong path: correct password rejected")
	}
	if _, ok, _ := s.Authenticate("host", "wrong", ""); ok {
		t.Fatal("wrong password accepted")
	}
	if _, ok, _ := s.Authenticate("nobody", "hunter2", ""); ok {
		t.Fatal("unknown user accepted")
	}
}

func TestAccountSessionToken(t *testing.T) {
	s, _ := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if _, err := s.Create("a", "pw", "fp"); err != nil {
		t.Fatal(err)
	}
	acc, ok, _ := s.Authenticate("a", "pw", "")
	if !ok {
		t.Fatal("login failed")
	}
	tok, err := s.RotateToken(acc)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Authenticate("a", "", tok); !ok {
		t.Fatal("session token rejected")
	}
	if _, ok, _ := s.Authenticate("a", "", "bogus-token"); ok {
		t.Fatal("bogus token accepted")
	}
}

func TestAccountPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, _ := NewAccountStore(path)
	if _, err := s.Create("a", "pw", "fpA"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("b", "pw", "fpB"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddFriend("a", "b"); err != nil {
		t.Fatal(err)
	}

	s2, err := NewAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Count() != 2 {
		t.Fatalf("reload: %d accounts, want 2", s2.Count())
	}
	if !s2.IsFriend("a", "b") || !s2.IsFriend("b", "a") {
		t.Fatal("friend graph not symmetric after reload")
	}
	if err := s2.RemoveFriend("a", "b"); err != nil {
		t.Fatal(err)
	}
	if s2.IsFriend("a", "b") {
		t.Fatal("friend still present after remove")
	}
}

func TestVerifyPassword(t *testing.T) {
	hash, err := hashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword("secret", hash) {
		t.Fatal("correct password must verify")
	}
	if verifyPassword("Secret", hash) {
		t.Fatal("case difference must fail")
	}
	if verifyPassword("secret", "not-a-valid-format") {
		t.Fatal("malformed hash must fail closed")
	}
}

func TestPBKDF2SHA256KnownVector(t *testing.T) {
	// RFC 6070 PBKDF2-HMAC-SHA256 test vector (password="password",
	// salt="salt", c=1, dkLen=32).
	got := pbkdf2SHA256("password", []byte("salt"), 1, 32)
	want := "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if gotHex := toHex(got); gotHex != want {
		t.Fatalf("PBKDF2 vector: got %s, want %s", gotHex, want)
	}
}

func toHex(b []byte) string {
	const hexdig = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdig[v>>4]
		out[i*2+1] = hexdig[v&0xf]
	}
	return string(out)
}

func TestAccountStoreMissingFileIsFresh(t *testing.T) {
	s, err := NewAccountStore(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 {
		t.Fatal("missing file should load an empty store")
	}
}

func TestCreateRejectsEmpty(t *testing.T) {
	s, _ := NewAccountStore(filepath.Join(t.TempDir(), "a.json"))
	if _, err := s.Create("", "pw", "fp"); err == nil {
		t.Fatal("empty username must fail")
	}
	if _, err := s.Create("u", "", "fp"); err == nil {
		t.Fatal("empty password must fail")
	}
}
