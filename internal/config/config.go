// Package config persists the Eliauk GUI's user settings — display name,
// coordination-server address, and remembered game-panel paths — to a JSON file
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
)

// Config is the whole GUI settings file.
type Config struct {
	Name   string `json:"name"`
	Server string `json:"server,omitempty"` // ws://host:port/ws

	// M7c game panel: remembered paths so the user sets them once. Empty means
	// "auto-detect next start".
	Java      string `json:"java,omitempty"`
	ServerJar string `json:"server_jar,omitempty"`
	Launcher  string `json:"launcher,omitempty"`
	GameDir   string `json:"game_dir,omitempty"`

	// UpdateURL is the self-update feed (a JSON manifest). Empty disables the
	// "检查更新" button until the user sets an update source.
	UpdateURL string `json:"update_url,omitempty"`
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
