// M7 accounts: a server-side user directory with password auth and a symmetric
// friend graph. The X25519 fingerprint (Devices) is what the P2P whitelist keys
// on — friends reference accounts by username, and the server resolves
// usernames to fingerprints so clients can whitelist each other without ever
// pasting a base64 code.
//
// Password hashing is PBKDF2-HMAC-SHA256 (stdlib only, no new dependency).
// Persistence is a JSON file with an atomic temp+rename save.
package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const pbkdf2Iter = 100_000

// Account is one registered user.
type Account struct {
	Username string   `json:"username"`
	PassHash string   `json:"pass_hash"` // pbkdf2$<salt hex>$<hash hex>
	Devices  []string `json:"devices"`   // base64 X25519 fingerprints this account logs in with
	Friends  []string `json:"friends"`   // symmetric friend graph, sorted
	Token    string   `json:"token"`     // current session token (rotated on each login)
}

// AccountStore loads and persists accounts, owns the friend graph and session
// tokens.
type AccountStore struct {
	mu       sync.Mutex
	path     string
	accounts map[string]*Account
}

// NewAccountStore loads the account file (creating it empty if missing).
func NewAccountStore(path string) (*AccountStore, error) {
	s := &AccountStore{path: path, accounts: make(map[string]*Account)}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read accounts %s: %w", path, err)
	}
	if len(raw) == 0 {
		return s, nil
	}
	var all []*Account
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("parse accounts %s: %w", path, err)
	}
	for _, a := range all {
		s.accounts[a.Username] = a
	}
	return s, nil
}

// Count returns the number of registered accounts.
func (s *AccountStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts)
}

// Get returns an account by username.
func (s *AccountStore) Get(username string) (*Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[username]
	return a, ok
}

// Create registers a new account with the given password and device
// fingerprint. It fails if the username already exists.
func (s *AccountStore) Create(username, password, fp string) (*Account, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("username is required")
	}
	if password == "" {
		return nil, fmt.Errorf("password is required")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.accounts[username]; exists {
		return nil, fmt.Errorf("账号已存在: %s", username)
	}
	a := &Account{Username: username, PassHash: hash, Token: newToken()}
	if fp != "" {
		a.Devices = append(a.Devices, fp)
	}
	s.accounts[username] = a
	return a, s.saveLocked()
}

// Authenticate validates a cached session token or, failing that, a password.
// It returns (account, true, nil) on success. The caller rotates the token via
// RotateToken after a successful login.
func (s *AccountStore) Authenticate(username, password, token string) (*Account, bool, error) {
	username = strings.TrimSpace(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[username]
	if !ok {
		return nil, false, nil
	}
	if token != "" && token == a.Token {
		return a, true, nil
	}
	if password != "" && verifyPassword(password, a.PassHash) {
		return a, true, nil
	}
	return nil, false, nil
}

// RotateToken issues a fresh session token (so a cached token dies when the
// user logs in again elsewhere) and persists.
func (s *AccountStore) RotateToken(a *Account) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.accounts[a.Username]
	if !ok {
		return "", fmt.Errorf("account %s no longer exists", a.Username)
	}
	cur.Token = newToken()
	return cur.Token, s.saveLocked()
}

// RegisterDevice adds a fingerprint to the account's known devices (used when a
// friend account appears under a new identity key). Persists only if changed.
func (s *AccountStore) RegisterDevice(a *Account, fp string) error {
	if fp == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.accounts[a.Username]
	if !ok {
		return fmt.Errorf("account %s no longer exists", a.Username)
	}
	for _, d := range cur.Devices {
		if d == fp {
			return nil
		}
	}
	cur.Devices = append(cur.Devices, fp)
	return s.saveLocked()
}

// AddFriend makes two accounts friends (symmetric: adding you to them too).
func (s *AccountStore) AddFriend(user, friend string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[user]
	if !ok {
		return fmt.Errorf("account not found: %s", user)
	}
	b, ok := s.accounts[friend]
	if !ok {
		return fmt.Errorf("account not found: %s", friend)
	}
	if user == friend {
		return fmt.Errorf("不能添加自己为好友")
	}
	a.Friends = addSorted(a.Friends, friend)
	b.Friends = addSorted(b.Friends, user)
	return s.saveLocked()
}

// RemoveFriend drops the friendship both ways.
func (s *AccountStore) RemoveFriend(user, friend string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[user]
	if !ok {
		return nil
	}
	a.Friends = removeSorted(a.Friends, friend)
	if b, ok := s.accounts[friend]; ok {
		b.Friends = removeSorted(b.Friends, user)
	}
	return s.saveLocked()
}

// IsFriend reports whether user lists friend.
func (s *AccountStore) IsFriend(user, friend string) bool {
	if user == "" || friend == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[user]
	if !ok {
		return false
	}
	i := sort.SearchStrings(a.Friends, friend)
	return i < len(a.Friends) && a.Friends[i] == friend
}

// FriendList returns the friend graph of user. Names are never exposed.
func (s *AccountStore) FriendList(user string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[user]
	if !ok {
		return nil
	}
	out := make([]string, len(a.Friends))
	copy(out, a.Friends)
	return out
}

// saveLocked writes the account file atomically. Callers hold s.mu.
func (s *AccountStore) saveLocked() error {
	all := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Username < all[j].Username })
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails to produce bytes
	}
	return hex.EncodeToString(b)
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := pbkdf2SHA256(password, salt, pbkdf2Iter, 32)
	return "pbkdf2$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(h), nil
}

func verifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != "pbkdf2" {
		return false
	}
	salt, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256(password, salt, pbkdf2Iter, len(want))
	return hmac.Equal(got, want)
}

// pbkdf2SHA256 is PBKDF2-HMAC-SHA256 (RFC 2898). Standard library only.
func pbkdf2SHA256(password string, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, []byte(password))
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var dk []byte
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		prf.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func addSorted(list []string, v string) []string {
	i := sort.SearchStrings(list, v)
	if i < len(list) && list[i] == v {
		return list
	}
	list = append(list, "")
	copy(list[i+1:], list[i:])
	list[i] = v
	return list
}

func removeSorted(list []string, v string) []string {
	i := sort.SearchStrings(list, v)
	if i < len(list) && list[i] == v {
		return append(list[:i], list[i+1:]...)
	}
	return list
}
