// Package crypto implements the Eliauk VPN encryption layer (M6): a long-term
// X25519 identity for peer authentication, a Noise-style ephemeral key exchange
// piggybacked on the p2p hello/helloAck handshake, and AES-256-GCM encryption of
// tunneled IP packets with per-packet counters and a small replay window.
//
// Everything uses the standard library (crypto/ecdh, crypto/hkdf, crypto/aes,
// crypto/cipher), so no third-party dependencies are introduced.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// nonceSize is the GCM nonce size (12 bytes); the packet counter occupies the
// last 8 bytes with a 4-byte zero prefix.
const nonceSize = 12

// tagSize is the GCM authentication tag length.
const tagSize = 16

// newAEAD constructs the AEAD from a 32-byte key. It cannot fail for a valid
// key, so the caller treats it as infallible.
func newAEAD(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("crypto: invalid AES key size") // key is always 32 bytes
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic("crypto: new gcm: " + err.Error())
	}
	return aead
}

// Identity is a long-term X25519 keypair used to authenticate this client to
// friends. The public key is the "fingerprint" friends add to their whitelist.
type Identity struct {
	priv *ecdh.PrivateKey
}

// GenerateIdentity creates a fresh identity.
func GenerateIdentity() (*Identity, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	return &Identity{priv: priv}, nil
}

// LoadIdentity reads a previously stored identity from disk.
func LoadIdentity(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("bad identity file %q: want 32 bytes, got %d", path, len(raw))
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("bad identity file %q: %w", path, err)
	}
	return &Identity{priv: priv}, nil
}

// LoadOrCreate loads the identity at path, generating and storing one if the
// file does not exist yet. The key file is mode 0600.
func LoadOrCreate(path string) (*Identity, error) {
	id, err := LoadIdentity(path)
	if err == nil {
		return id, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	id, err = GenerateIdentity()
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create identity dir: %w", err)
		}
	}
	if err := os.WriteFile(path, id.priv.Bytes(), 0o600); err != nil {
		return nil, fmt.Errorf("store identity: %w", err)
	}
	return id, nil
}

// PublicKey returns the identity's X25519 public key (32 bytes).
func (id *Identity) PublicKey() []byte { return id.priv.PublicKey().Bytes() }

// Fingerprint returns the base64 form of the public key — the value shared with
// friends for whitelisting.
func (id *Identity) Fingerprint() string {
	return base64.StdEncoding.EncodeToString(id.PublicKey())
}

// ParseFingerprint decodes a base64 public key.
func ParseFingerprint(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("bad fingerprint: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("bad fingerprint: want 32 bytes, got %d", len(b))
	}
	return b, nil
}

// Handshake is one side's contribution to the key exchange: an ephemeral X25519
// public key (fresh per connection, for forward secrecy) plus the static
// identity key (for authentication). The two handshake messages are carried in
// the hello / helloAck frames.
type Handshake struct {
	Eph  []byte // ephemeral public key, 32 bytes
	Stat []byte // static public key, 32 bytes
}

// Marshal serializes the handshake message (64 bytes: eph || static).
func (h *Handshake) Marshal() []byte {
	out := make([]byte, 0, 64)
	out = append(out, h.Eph...)
	out = append(out, h.Stat...)
	return out
}

// Unmarshal parses a handshake message. It does not validate the public keys;
// that happens when they are used (NewPublicKey in NewSession).
func (h *Handshake) Unmarshal(b []byte) error {
	if len(b) != 64 {
		return fmt.Errorf("bad handshake: want 64 bytes, got %d", len(b))
	}
	h.Eph = append([]byte(nil), b[:32]...)
	h.Stat = append([]byte(nil), b[32:]...)
	return nil
}

// sessionKeyLen is the X25519/AES-256 key size.
const sessionKeyLen = 32

// replayWindow is how many recently-seen packet counters are accepted before a
// packet is treated as a replay. Tolerates mild UDP reordering while rejecting
// duplicates.
const replayWindow = 64

// Session holds the symmetric key for one peer connection, derived from a DH
// chain over the handshake keys, plus send/receive counters and a small replay
// window.
type Session struct {
	key  [sessionKeyLen]byte
	send uint64
	recv uint64
	seen uint64 // bitmap of recently accepted receive counters
}

// NewSession derives the per-connection session key from our static + ephemeral
// private keys and the peer's public keys. It is role-independent: both sides of
// a simultaneous hole punch compute the same key without agreeing on who
// "started", because every input is canonicalized.
//
// The construction computes the three symmetric DH combinations — static x
// peer_eph, our_eph x peer_static, our_eph x peer_eph — sorts the three
// 32-byte shared secrets to get a canonical IKM (the values are identical on
// both sides, only their order would differ), and HKDFs it with a transcript of
// the four public keys (also sorted) as the salt. The two static DHs
// authenticate both peers (an impostor who claims a static key without its
// private half cannot compute them) and the ephemeral DH provides forward
// secrecy.
func NewSession(myStatic *Identity, myEph *ecdh.PrivateKey, peerEph, peerStatic []byte) (*Session, error) {
	peerEphPub, err := ecdh.X25519().NewPublicKey(peerEph)
	if err != nil {
		return nil, fmt.Errorf("bad peer ephemeral key: %w", err)
	}
	peerStaticPub, err := ecdh.X25519().NewPublicKey(peerStatic)
	if err != nil {
		return nil, fmt.Errorf("bad peer static key: %w", err)
	}

	// Each DH is symmetric (X25519(a, B) == X25519(b, A)), so both sides compute
	// the same three values; only the association of value->slot differs.
	var dhStaticEph, dhEphStatic, dhEphEph []byte
	if dhStaticEph, err = myStatic.priv.ECDH(peerEphPub); err != nil {
		return nil, fmt.Errorf("dh static x peer eph: %w", err)
	}
	if dhEphStatic, err = myEph.ECDH(peerStaticPub); err != nil {
		return nil, fmt.Errorf("dh our eph x peer static: %w", err)
	}
	if dhEphEph, err = myEph.ECDH(peerEphPub); err != nil {
		return nil, fmt.Errorf("dh our eph x peer eph: %w", err)
	}

	// Canonical IKM: sort the three shared secrets so both sides concatenate
	// them in the same order regardless of role.
	dh := [][]byte{dhStaticEph, dhEphStatic, dhEphEph}
	slices.SortFunc(dh, func(a, b []byte) int { return bytes.Compare(a, b) })
	ikm := make([]byte, 0, 96)
	for _, d := range dh {
		ikm = append(ikm, d...)
	}

	// Canonical transcript: sort the four public keys.
	transcript := [][]byte{
		myEph.PublicKey().Bytes(),
		myStatic.PublicKey(),
		peerEph,
		peerStatic,
	}
	slices.SortFunc(transcript, func(a, b []byte) int { return bytes.Compare(a, b) })
	var tb []byte
	for _, k := range transcript {
		tb = append(tb, k...)
	}
	salt := sha256.Sum256(tb)

	keyBytes, err := hkdf.Key(sha256.New, ikm, salt[:], "eliaukvpn/session", sessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("derive session key: %w", err)
	}
	var key [sessionKeyLen]byte
	copy(key[:], keyBytes)
	return &Session{key: key}, nil
}

// Seal encrypts plaintext under the session key, prepending the packet's
// counter so the receiver can decrypt out-of-order packets. The nonce is the
// counter in the last 8 bytes with a 4-byte zero prefix (WireGuard-style); aad
// is authenticated but not encrypted and must match on Open.
func (s *Session) Seal(aad, plaintext []byte) []byte {
	ctr := s.send
	s.send++
	aead := newAEAD(s.key[:])
	var nonce [nonceSize]byte
	binary.BigEndian.PutUint64(nonce[4:], ctr)
	ct := aead.Seal(nil, nonce[:], plaintext, aad)
	out := make([]byte, 8+len(ct))
	binary.BigEndian.PutUint64(out[:8], ctr)
	copy(out[8:], ct)
	return out
}

// Open decrypts a packet produced by Seal, enforcing a replay window: counters
// within [recv-replayWindow, recv] that were already seen are rejected, as are
// anything older. Counter state is only advanced after a successful
// authentication, so replayed ciphertext cannot corrupt the window.
func (s *Session) Open(aad, pkt []byte) ([]byte, error) {
	if len(pkt) < 8+tagSize {
		return nil, errors.New("crypto: packet too short")
	}
	ctr := binary.BigEndian.Uint64(pkt[:8])
	aead := newAEAD(s.key[:])
	var nonce [nonceSize]byte
	binary.BigEndian.PutUint64(nonce[4:], ctr)
	pt, err := aead.Open(nil, nonce[:], pkt[8:], aad)
	if err != nil {
		return nil, err
	}

	switch {
	case ctr+replayWindow < s.recv:
		return nil, errors.New("crypto: packet too old (replay?)")
	case ctr > s.recv:
		d := ctr - s.recv
		if d >= replayWindow {
			s.seen = 0
		} else {
			s.seen <<= d
		}
		s.recv = ctr
		s.seen |= 1
	default:
		bit := uint64(1) << (s.recv - ctr)
		if s.seen&bit != 0 {
			return nil, errors.New("crypto: duplicate packet (replay)")
		}
		s.seen |= bit
	}
	return pt, nil
}
