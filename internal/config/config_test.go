package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileIsFresh(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if c.Name != "" || c.Server != "" {
		t.Fatalf("expected empty config, got %+v", c)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c := &Config{
		Name:      "Alice",
		Server:    "ws://vps.example.com:9090/ws",
		Java:      `C:\jdk\bin\java.exe`,
		ServerJar: `C:\mc\server.jar`,
		Launcher:  `C:\PCL\Plain Craft Launcher 2.exe`,
		GameDir:   `C:\mc\.minecraft`,
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
	if got.Java != c.Java || got.ServerJar != c.ServerJar ||
		got.Launcher != c.Launcher || got.GameDir != c.GameDir {
		t.Fatalf("round-trip game paths mismatch: %+v", got)
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

func TestLoadStripsUTF8BOM(t *testing.T) {
	// PowerShell's Set-Content -Encoding UTF8 (and Notepad) prepend a BOM;
	// encoding/json rejects it, so Load must strip it first.
	path := filepath.Join(t.TempDir(), "bom.json")
	raw := append([]byte("\xef\xbb\xbf"), []byte(`{"name":"bom-host"}`)...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("BOM'd config should load: %v", err)
	}
	if c.Name != "bom-host" {
		t.Fatalf("Name = %q, want %q", c.Name, "bom-host")
	}
}
