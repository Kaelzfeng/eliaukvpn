// Package update implements the minimal self-update loop: fetch a version
// manifest, verify the downloaded binary (SHA-256 of the exe, plus an optional
// Ed25519 signature over the manifest when a public key is compiled in), and
// hand the new exe to the caller to install on the next quit — a running exe
// cannot be overwritten in place, so install happens via a tiny .bat that waits
// for us to exit, swaps the file, and relaunches.
package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Manifest is the update feed document served at the configured update URL.
type Manifest struct {
	Version   string `json:"version"`              // new version, e.g. "1.1.0"
	URL       string `json:"url"`                  // absolute URL of the new exe
	SHA256    string `json:"sha256"`               // hex digest of the exe
	Signature string `json:"signature,omitempty"`  // hex Ed25519 sig over version|url|sha256
	Notes     string `json:"notes,omitempty"`      // release notes shown in the About panel
}

// Check fetches and parses the manifest at feedURL. An empty feedURL is an
// error (the feature is off until the user sets an update source).
func Check(feedURL string, client *http.Client) (*Manifest, error) {
	if strings.TrimSpace(feedURL) == "" {
		return nil, fmt.Errorf("未配置更新源")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequest("GET", feedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("更新源地址无效: %w", err)
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接更新源失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("更新源返回 %s", resp.Status)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("解析更新清单失败: %w", err)
	}
	if m.Version == "" || m.URL == "" || m.SHA256 == "" {
		return nil, fmt.Errorf("更新清单缺少字段 (version/url/sha256)")
	}
	return &m, nil
}

// Verify checks the manifest's Ed25519 signature against pub. When pub is
// empty (no public key compiled in) the signature is skipped and the caller
// relies on the SHA-256 digest alone.
func Verify(m *Manifest, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return nil
	}
	if m.Signature == "" {
		return fmt.Errorf("更新清单缺少签名")
	}
	sig, err := hex.DecodeString(m.Signature)
	if err != nil {
		return fmt.Errorf("更新清单签名格式错误: %v", err)
	}
	if !ed25519.Verify(pub, signingMessage(m), sig) {
		return fmt.Errorf("更新清单签名校验失败")
	}
	return nil
}

// signingMessage canonicalizes the signed fields; cmd/updatesign signs exactly
// this byte sequence, so the two sides always agree.
func signingMessage(m *Manifest) []byte {
	return []byte(m.Version + "|" + m.URL + "|" + m.SHA256)
}

// Download fetches the new exe to a temp file, verifies its SHA-256 against the
// manifest, and returns the temp path. The file persists so the install step
// (which runs after this process exits) can read it.
func Download(m *Manifest, client *http.Client) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Get(m.URL)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败: %s", resp.Status)
	}
	tmp, err := os.CreateTemp("", "eliauk-update-*.exe")
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败: %w", err)
	}
	name := tmp.Name()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", fmt.Errorf("下载中断: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, m.SHA256) {
		os.Remove(name)
		return "", fmt.Errorf("下载校验失败 (SHA-256 不匹配)")
	}
	return name, nil
}

// Newer reports whether the next version is newer than the current one, using a
// tolerant numeric comparison (1.10 > 1.9, trailing zeroes ignored).
func Newer(next, current string) bool {
	a, b := splitVersion(next), splitVersion(current)
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		if i < len(b) {
			y, _ = strconv.Atoi(b[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func splitVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' })
}

// Install arms a delayed swap: it writes a tiny .bat that waits for the current
// process to exit, copies the freshly downloaded newExe over currentExe, and
// relaunches it, then deletes the temp files. The .bat is started hidden and
// detached, so it survives this process exiting. currentExe is os.Executable().
func Install(newExe, currentExe string) error {
	bat, err := os.CreateTemp("", "eliauk-update-*.bat")
	if err != nil {
		return fmt.Errorf("创建安装脚本失败: %w", err)
	}
	batPath := bat.Name()
	body := installScript(newExe, currentExe, true)
	if _, err := io.WriteString(bat, body); err != nil {
		bat.Close()
		os.Remove(batPath)
		return fmt.Errorf("写安装脚本失败: %w", err)
	}
	if err := bat.Close(); err != nil {
		os.Remove(batPath)
		return err
	}
	// Hide the console and don't wait: cmd.exe must outlive this process so the
	// swap happens after we've fully exited. `call` explicitly marks the arg as
	// a batch file, which survives temp-dir paths that contain spaces.
	cmd := exec.Command("cmd", "/d", "/c", "call", batPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := cmd.Start(); err != nil {
		os.Remove(batPath)
		return fmt.Errorf("启动安装脚本失败: %w", err)
	}
	return nil
}

// installScript builds the .bat body that performs the swap. relaunch controls
// whether it starts the new exe afterwards (true in production; tests disable
// it so they never execute the target as a program).
func installScript(newExe, currentExe string, relaunch bool) string {
	var sb strings.Builder
	sb.WriteString("@echo off\r\n")
	// ping 127.0.0.1 is the console-independent ~1s sleep: unlike `timeout` it
	// does not require an interactive console, so it works when the app exits
	// from a service or a piped session.
	sb.WriteString("ping 127.0.0.1 -n 2 >nul\r\n")
	sb.WriteString(`copy /y "` + newExe + `" "` + currentExe + `" >nul` + "\r\n")
	if relaunch {
		sb.WriteString(`start "" "` + currentExe + `"` + "\r\n")
	}
	sb.WriteString(`del "` + newExe + `"` + "\r\n")
	// Note: the bat deliberately does NOT delete itself (del "%~f0"). cmd
	// reads the batch incrementally and fails with "The batch file cannot be
	// found" the moment the file it is executing is removed. CleanupStale()
	// removes orphaned install scripts on the next app start instead.
	return sb.String()
}

// CleanupStale removes leftover update artifacts in the temp directory — an
// install script and/or downloaded exe from an update that was interrupted
// before the swap ran. Call once at startup, before any new download.
func CleanupStale() {
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if (strings.HasPrefix(name, "eliauk-update-") && strings.HasSuffix(name, ".bat")) ||
			(strings.HasPrefix(name, "eliauk-update-") && strings.HasSuffix(name, ".exe")) {
			os.Remove(filepath.Join(dir, name))
		}
	}
}
