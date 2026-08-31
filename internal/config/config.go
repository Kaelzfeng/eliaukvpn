// Package config persists the Eliauk GUI's user settings — display name,
// coordination-server address, and the friends allowlist — to a JSON file
// under %AppData%\Eliauk\config.json. A missing file means "fresh install":
// Load returns an empty config (not an error) so the GUI can show its first-run
// setup. The long-term identity key stays in its own file (see
// agent.DefaultKeyfile); only user-facing settings live here.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Friend is one allowlisted peer. A legacy friend is identified by the base64
// fingerprint (their X25519 public key) pasted into the GUI; an M7 account
// friend is identified by username and the server resolves it to a fingerprint.
type Friend struct {
	Name string `json:"name,omitempty"`
	User string `json:"user,omitempty"` // M7 account username (optional)
	Code string `json:"code"`           // base64 fingerprint (crypto.Fingerprint format)
}

// Config is the whole GUI settings file.
type Config struct {
	Name    string   `json:"name"`
	Server  string   `json:"server,omitempty"` // ws://host:port/ws
	Friends []Friend `json:"friends,omitempty"`

	// M7 account login. Once the server has issued a session token it is cached
	// here so the next start needs no password. The plaintext password is never
	// persisted.
	Account string `json:"account,omitempty"`
	Token   string `json:"token,omitempty"`

	// M7c game panel: remembered paths so the user sets them once. Empty means
	// "auto-detect next start".
	Java     string `json:"java,omitempty"`
	ServerJar string `json:"server_jar,omitempty"`
}

// DefaultPath returns the per-user config path. On Windows this is
// %AppData%\Eliauk\config.json.
func DefaultPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "Eliauk", "config.json")
	}
	return "config.json"
}

// Load reads the config file. A missing file returns an empty Config (the
// GUI treats that as first run). A corrupt file is an error so the user gets a
// message instead of silently losing their settings.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	// Strip a UTF-8 BOM if present — PowerShell's Set-Content -Encoding UTF8
	// and Notepad write one, and encoding/json rejects it as a stray char.
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config file, creating the directory as needed.
func (c *Config) Save(path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

// HasFriend reports whether a fingerprint is already in the allowlist.
func (c *Config) HasFriend(code string) bool {
	code = strings.TrimSpace(code)
	for _, f := range c.Friends {
		if f.Code == code {
			return true
		}
	}
	return false
}

// AddFriend appends a friend (deduplicated by fingerprint). It returns false
// if the code was already present.
func (c *Config) AddFriend(name, code string) bool {
	code = strings.TrimSpace(code)
	if code == "" || c.HasFriend(code) {
		return false
	}
	c.Friends = append(c.Friends, Friend{Name: strings.TrimSpace(name), Code: code})
	return true
}

// RemoveFriend deletes a friend by fingerprint. It returns false if the code
// was not in the list.
func (c *Config) RemoveFriend(code string) bool {
	code = strings.TrimSpace(code)
	for i, f := range c.Friends {
		if f.Code == code {
			c.Friends = append(c.Friends[:i], c.Friends[i+1:]...)
			return true
		}
	}
	return false
}

// HasFriendUser reports whether an account friend is already in the list.
func (c *Config) HasFriendUser(user string) bool {
	user = strings.TrimSpace(user)
	for _, f := range c.Friends {
		if f.User == user {
			return true
		}
	}
	return false
}

// AddFriendUser appends an M7 account friend (deduplicated by username). The
// server is authoritative for the friend graph; the config copy only serves the
// pre-account and offline displays.
func (c *Config) AddFriendUser(user string) bool {
	user = strings.TrimSpace(user)
	if user == "" || c.HasFriendUser(user) {
		return false
	}
	c.Friends = append(c.Friends, Friend{User: user})
	return true
}

// RemoveFriendUser deletes an account friend by username. It returns false if
// the user was not in the list.
func (c *Config) RemoveFriendUser(user string) bool {
	user = strings.TrimSpace(user)
	for i, f := range c.Friends {
		if f.User == user {
			c.Friends = append(c.Friends[:i], c.Friends[i+1:]...)
			return true
		}
	}
	return false
}
