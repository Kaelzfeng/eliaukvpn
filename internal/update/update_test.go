package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Manifest{Version: "1.1.0", URL: "http://x/new.exe", SHA256: "abc"})
	}))
	defer srv.Close()

	m, err := Check(srv.URL, nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if m.Version != "1.1.0" {
		t.Fatalf("version: %q", m.Version)
	}

	// Empty feed is an error (feature off).
	if _, err := Check("", nil); err == nil {
		t.Fatal("empty feed should error")
	}

	// Missing fields must be rejected.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"version": "1.0"})
	}))
	defer srv2.Close()
	if _, err := Check(srv2.URL, nil); err == nil {
		t.Fatal("manifest missing url/sha256 should error")
	}
}

func TestVerifyAndDownload(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("fake new exe bytes")
	sum := sha256.Sum256(payload)

	m := &Manifest{
		Version: "2.0.0",
		URL:     "http://x/new.exe",
		SHA256:  hex.EncodeToString(sum[:]),
	}
	// No key baked in -> signature skipped.
	if err := Verify(m, nil); err != nil {
		t.Fatalf("Verify nil key: %v", err)
	}
	// Key baked in but manifest unsigned -> error.
	if err := Verify(m, pub); err == nil {
		t.Fatal("Verify with pub and no signature should error")
	}
	// Signed correctly.
	m.Signature = hex.EncodeToString(ed25519.Sign(priv, []byte(m.Version+"|"+m.URL+"|"+m.SHA256)))
	if err := Verify(m, pub); err != nil {
		t.Fatalf("Verify signed: %v", err)
	}
	// Tampered manifest -> signature fails.
	m.Version = "2.0.1"
	if err := Verify(m, pub); err == nil {
		t.Fatal("Verify tampered should fail")
	}
	m.Version = "2.0.0"

	// Download + digest check against an httptest server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	m.URL = srv.URL
	path, err := Download(m, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer os.Remove(path)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("downloaded bytes mismatch: %q err=%v", got, err)
	}

	// Wrong digest must be rejected and the temp file removed.
	bad := *m
	bad.SHA256 = strings.Repeat("0", 64)
	p2, err := Download(&bad, nil)
	if err == nil {
		t.Fatal("Download with wrong digest should fail")
	}
	if p2 != "" {
		os.Remove(p2)
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		next, cur string
		want      bool
	}{
		{"1.10.0", "1.9.0", true},
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.1.0", false},
		{"1.0.0", "1.0.1", false},
		{"2.0", "1.9.9", true},
		{"1.0.0-alpha", "1.0.0", false}, // pre-release treated as equal base
	}
	for _, c := range cases {
		if got := Newer(c.next, c.cur); got != c.want {
			t.Errorf("Newer(%q,%q) = %v, want %v", c.next, c.cur, got, c.want)
		}
	}
}

// TestInstallSwap exercises the real .bat artifact (relaunch disabled) end to
// end: current file gets replaced by the new bytes and the temp new file is
// removed afterwards.
func TestInstallSwap(t *testing.T) {
	dir := t.TempDir()
	currentExe := filepath.Join(dir, "app.exe")
	newExe := filepath.Join(dir, "new.exe")
	os.WriteFile(currentExe, []byte("old"), 0o644)
	os.WriteFile(newExe, []byte("new-version"), 0o644)

	batPath := filepath.Join(dir, "install.bat")
	os.WriteFile(batPath, []byte(installScript(newExe, currentExe, false)), 0o644)

	cmd := exec.Command("cmd", "/d", "/c", "call", batPath)
	cmd.SysProcAttr = hideWindowAttr()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bat failed: %v (%s)", err, out)
	}

	// Wait for the bat's timeout(1) to finish the copy.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := os.ReadFile(currentExe)
		if err == nil && string(got) == "new-version" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("currentExe was not replaced: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(newExe); !os.IsNotExist(err) {
		t.Fatalf("temp newExe should be deleted, stat err=%v", err)
	}
}

func hideWindowAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
