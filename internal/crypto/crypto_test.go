package crypto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// newKeypair returns a fresh ephemeral keypair for one side of a handshake.
func newKeypair(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ephemeral: %v", err)
	}
	return priv
}

// handshakeMsg builds the public half of one side's handshake contribution.
func handshakeMsg(t *testing.T, id *Identity, eph *ecdh.PrivateKey) *Handshake {
	t.Helper()
	return &Handshake{Eph: append([]byte(nil), eph.PublicKey().Bytes()...), Stat: id.PublicKey()}
}

// deriveBoth computes the session key from both roles' perspectives and asserts
// they agree, returning both sessions.
func deriveBoth(t *testing.T, initID, respID *Identity, initEph, respEph *ecdh.PrivateKey) (*Session, *Session) {
	t.Helper()
	initHS := handshakeMsg(t, initID, initEph)
	respHS := handshakeMsg(t, respID, respEph)

	initSess, err := NewSession(initID, initEph, respHS.Eph, respHS.Stat)
	if err != nil {
		t.Fatalf("initiator NewSession: %v", err)
	}
	respSess, err := NewSession(respID, respEph, initHS.Eph, initHS.Stat)
	if err != nil {
		t.Fatalf("responder NewSession: %v", err)
	}
	if !bytes.Equal(initSess.key[:], respSess.key[:]) {
		t.Fatal("session keys disagree between roles")
	}
	return initSess, respSess
}

func TestHandshakeRoundTrip(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	eph := newKeypair(t)
	h := handshakeMsg(t, id, eph)
	raw := h.Marshal()
	if len(raw) != 64 {
		t.Fatalf("handshake len = %d, want 64", len(raw))
	}
	var got Handshake
	if err := got.Unmarshal(raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(got.Eph, h.Eph) || !bytes.Equal(got.Stat, h.Stat) {
		t.Fatal("handshake round-trip mismatch")
	}
	if err := got.Unmarshal(raw[:10]); err == nil {
		t.Fatal("short handshake accepted")
	}
}

func TestSessionBothRolesAgree(t *testing.T) {
	initID, _ := GenerateIdentity()
	respID, _ := GenerateIdentity()
	initEph := newKeypair(t)
	respEph := newKeypair(t)
	deriveBoth(t, initID, respID, initEph, respEph)
}

func TestSealOpenRoundTrip(t *testing.T) {
	initID, _ := GenerateIdentity()
	respID, _ := GenerateIdentity()
	initSess, respSess := deriveBoth(t, initID, respID, newKeypair(t), newKeypair(t))

	pt := []byte("hello tunnel")
	aad := []byte("aad")
	ct := initSess.Seal(aad, pt)
	if bytes.Contains(ct, pt) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := respSess.Open(aad, ct)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatal("plaintext mismatch")
	}
}

func TestOpenOutOfOrder(t *testing.T) {
	initID, _ := GenerateIdentity()
	respID, _ := GenerateIdentity()
	initSess, respSess := deriveBoth(t, initID, respID, newKeypair(t), newKeypair(t))

	// Send counters 0,1,2 but deliver 2,0,1.
	packets := [][]byte{
		initSess.Seal(nil, []byte("p0")),
		initSess.Seal(nil, []byte("p1")),
		initSess.Seal(nil, []byte("p2")),
	}
	want := []string{"p2", "p0", "p1"}
	for i, idx := range []int{2, 0, 1} {
		got, err := respSess.Open(nil, packets[idx])
		if err != nil {
			t.Fatalf("out-of-order open %d: %v", i, err)
		}
		if string(got) != want[i] {
			t.Fatalf("out-of-order open %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestReplayRejected(t *testing.T) {
	initID, _ := GenerateIdentity()
	respID, _ := GenerateIdentity()
	initSess, respSess := deriveBoth(t, initID, respID, newKeypair(t), newKeypair(t))

	ct := initSess.Seal(nil, []byte("once"))
	if _, err := respSess.Open(nil, ct); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := respSess.Open(nil, ct); err == nil {
		t.Fatal("replayed packet accepted")
	}
}

func TestWindowShiftDiscards(t *testing.T) {
	initID, _ := GenerateIdentity()
	respID, _ := GenerateIdentity()
	initSess, respSess := deriveBoth(t, initID, respID, newKeypair(t), newKeypair(t))

	// Deliver counter 0, then jump 200 counters ahead (out of window), then
	// deliver 0 again — must be rejected as too old.
	p0 := initSess.Seal(nil, []byte("p0"))
	if _, err := respSess.Open(nil, p0); err != nil {
		t.Fatalf("open p0: %v", err)
	}
	for i := 0; i < 200; i++ {
		initSess.Seal(nil, []byte("filler"))
	}
	if _, err := respSess.Open(nil, p0); err == nil {
		t.Fatal("packet older than the replay window accepted")
	}
}

func TestTamperedRejected(t *testing.T) {
	initID, _ := GenerateIdentity()
	respID, _ := GenerateIdentity()
	initSess, respSess := deriveBoth(t, initID, respID, newKeypair(t), newKeypair(t))

	ct := initSess.Seal(nil, []byte("keep this secret"))
	ct[len(ct)-1] ^= 0x01 // flip a ciphertext bit
	if _, err := respSess.Open(nil, ct); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestWrongAADRejected(t *testing.T) {
	initID, _ := GenerateIdentity()
	respID, _ := GenerateIdentity()
	initSess, respSess := deriveBoth(t, initID, respID, newKeypair(t), newKeypair(t))

	ct := initSess.Seal([]byte("peer-a"), []byte("data"))
	if _, err := respSess.Open([]byte("peer-b"), ct); err == nil {
		t.Fatal("packet with wrong AAD accepted")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	// Impersonation: an attacker who claims the responder's public key R_pub but
	// does not hold R_priv cannot derive the session. The initiator uses R_pub
	// (the claimed identity); the attacker computes the responder-side DH with a
	// different private key, so the sessions must disagree and ciphertext sealed
	// by one side must not open under the other.
	victim, _ := GenerateIdentity()    // real initiator
	responder, _ := GenerateIdentity() // real responder, R_priv held here
	attacker, _ := GenerateIdentity()  // claims to be the responder

	victimEph := newKeypair(t)
	attEph := newKeypair(t)

	// The initiator is told the responder's real public static key.
	claim := &Handshake{Eph: append([]byte(nil), attEph.PublicKey().Bytes()...), Stat: responder.PublicKey()}
	victimSess, err := NewSession(victim, victimEph, claim.Eph, claim.Stat)
	if err != nil {
		t.Fatalf("victim NewSession: %v", err)
	}
	// The attacker (responder side) uses its OWN private key, not R_priv.
	attackerSess, err := NewSession(attacker, attEph, victimEph.PublicKey().Bytes(), victim.PublicKey())
	if err != nil {
		t.Fatalf("attacker NewSession: %v", err)
	}
	if victimSess.key == attackerSess.key {
		t.Fatal("impersonator derived the same session key — static authentication failed")
	}
	ct := victimSess.Seal(nil, []byte("spoof"))
	if _, err := attackerSess.Open(nil, ct); err == nil {
		t.Fatal("impersonator opened initiator ciphertext")
	}
}

func TestIdentityPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id.key")
	id1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("load-or-create: %v", err)
	}
	fp1 := id1.Fingerprint()

	id2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if fp1 != id2.Fingerprint() {
		t.Fatal("identity not stable across loads")
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The 0600 check is Unix-only: Windows has no owner/group/other permission
	// bits and reports 0666 regardless of the mode passed to os.WriteFile.
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("identity file permissions too loose: %v", st.Mode())
	}
}

func TestFingerprintRoundTrip(t *testing.T) {
	id, _ := GenerateIdentity()
	raw, err := ParseFingerprint(id.Fingerprint())
	if err != nil {
		t.Fatalf("parse own fingerprint: %v", err)
	}
	if !bytes.Equal(raw, id.PublicKey()) {
		t.Fatal("fingerprint round-trip mismatch")
	}
	if _, err := ParseFingerprint(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("short fingerprint accepted")
	}
	if _, err := ParseFingerprint("not base64!"); err == nil {
		t.Fatal("garbage fingerprint accepted")
	}
}

func TestBadPeerKeysRejected(t *testing.T) {
	initID, _ := GenerateIdentity()
	respID, _ := GenerateIdentity()
	initEph := newKeypair(t)

	if _, err := NewSession(initID, initEph, make([]byte, 32), respID.PublicKey()); err == nil {
		t.Fatal("all-zero ephemeral peer key accepted")
	}
	if _, err := NewSession(initID, initEph, initEph.PublicKey().Bytes(), make([]byte, 31)); err == nil {
		t.Fatal("short static peer key accepted")
	}
}
