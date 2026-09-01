// Package mc integrates Minecraft into Eliauk: it locates the Java runtime,
// the .minecraft directory, a server jar and the official launcher, manages a
// dedicated server process (eula.txt + server.properties + `java -jar ... nogui`),
// and can register the virtual-LAN server address into the launcher's
// multiplayer list (servers.dat) so joining friends just click it. Stdlib only —
// no third-party dependency, matching the rest of the project.
package mc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---- detection ----

// MinecraftDir returns the official launcher's game directory
// (%AppData%\.minecraft on Windows), or "" when it is not installed.
func MinecraftDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		d := filepath.Join(dir, ".minecraft")
		if isDir(d) {
			return d
		}
	}
	if app := os.Getenv("APPDATA"); app != "" {
		d := filepath.Join(app, ".minecraft")
		if isDir(d) {
			return d
		}
	}
	return ""
}

// LauncherExe returns the official launcher executable under .minecraft, or "".
func LauncherExe() string {
	mc := MinecraftDir()
	if mc == "" {
		return ""
	}
	for _, p := range []string{
		filepath.Join(mc, "launcher", "MinecraftLauncher.exe"),
		filepath.Join(mc, "MinecraftLauncher.exe"),
	} {
		if isFile(p) {
			return p
		}
	}
	return ""
}

// pclExeNames are the executables the Plain Craft Launcher (PCL/PCL2) ships as.
// PCL is a popular third-party launcher that lives outside .minecraft, so it
// needs its own detection.
var pclExeNames = []string{
	"PCL.exe",
	"PCL2.exe",
	"PCL2 CE.exe",
	"Plain Craft Launcher.exe",
	"PlainCraftLauncher.exe",
}

// FindPCL locates a Plain Craft Launcher executable in the places a portable
// launcher usually lives (Desktop / Downloads / Documents, then Program Files).
// The search is shallow (depth-limited) so it stays fast.
func FindPCL() string {
	var dirs []string
	if home := os.Getenv("USERPROFILE"); home != "" {
		dirs = append(dirs,
			filepath.Join(home, "Desktop"),
			filepath.Join(home, "Downloads"),
			filepath.Join(home, "Documents"),
		)
	}
	for _, v := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		if r := os.Getenv(v); r != "" {
			dirs = append(dirs, r)
		}
	}
	for _, d := range dirs {
		if p := findExeNamed(d, pclExeNames); p != "" {
			return p
		}
	}
	return ""
}

// findExeNamed walks root (depth-limited) looking for a file whose name matches
// any of names (case-insensitive). It returns "" when nothing matches.
func findExeNamed(root string, names []string) string {
	if !isDir(root) {
		return ""
	}
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if found != "" {
			return filepath.SkipAll
		}
		if rel, e := filepath.Rel(root, path); e == nil && rel != "." && strings.Count(rel, string(os.PathSeparator)) > 3 {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() {
			for _, n := range names {
				if strings.EqualFold(info.Name(), n) {
					found = path
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return found
}

// LauncherPath returns the best launcher to start: PCL first (the launcher
// offline-account players usually use), then the official launcher.
func LauncherPath() string {
	if p := FindPCL(); p != "" {
		return p
	}
	return LauncherExe()
}

// GameDir returns the .minecraft directory to write servers.dat into: PCL's
// (next to its exe) when present, otherwise the official one.
func GameDir() string {
	if p := FindPCL(); p != "" {
		if d := filepath.Join(filepath.Dir(p), ".minecraft"); isDir(d) {
			return d
		}
	}
	return MinecraftDir()
}

// FindJava locates a java runtime: $JAVA_HOME first, then the well-known
// Windows install trees (Adoptium/Java/Microsoft/Corretto), then the Java
// runtimes the Minecraft launcher bundles under .minecraft\runtime, then PATH.
func FindJava() string {
	if h := os.Getenv("JAVA_HOME"); h != "" {
		for _, p := range []string{
			filepath.Join(h, "bin", "java.exe"),
			filepath.Join(h, "bin", "java"),
		} {
			if isFile(p) {
				return p
			}
		}
	}

	var roots []string
	for _, v := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		if r := os.Getenv(v); r != "" {
			roots = append(roots, r)
		}
	}
	for _, root := range roots {
		// Shallow globs only — no full-tree scans of Program Files.
		for _, pat := range []string{
			filepath.Join(root, "Java", "*", "bin", "java.exe"),
			filepath.Join(root, "Eclipse Adoptium", "*", "bin", "java.exe"),
			filepath.Join(root, "Amazon Corretto", "*", "bin", "java.exe"),
			filepath.Join(root, "Microsoft", "jdk-*", "bin", "java.exe"),
			filepath.Join(root, "Microsoft", "jdk*", "bin", "java.exe"),
		} {
			if hits, _ := filepath.Glob(pat); len(hits) > 0 {
				return hits[0]
			}
		}
	}
	if mc := MinecraftDir(); mc != "" {
		if p := findJavaUnder(filepath.Join(mc, "runtime")); p != "" {
			return p
		}
	}
	if p, err := exec.LookPath("java"); err == nil {
		return p
	}
	return ""
}

// findJavaUnder walks root looking for java.exe, stopping descent beyond a few
// levels so a pathological directory tree can never trigger a full-disk scan.
func findJavaUnder(root string) string {
	if !isDir(root) {
		return ""
	}
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel != "." && strings.Count(rel, string(os.PathSeparator)) > 6 {
			return filepath.SkipDir
		}
		if info.Name() == "java.exe" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// FindServerJar looks for a dedicated-server jar in a few obvious places.
func FindServerJar() string {
	for _, d := range []string{".", MinecraftDir()} {
		p := filepath.Join(d, "server.jar")
		if isFile(p) {
			return p
		}
	}
	return ""
}

// ---- dedicated server ----

// Server is one running `java -jar server.jar nogui` process.
type Server struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	in   io.WriteCloser
	done chan struct{} // closed when the server process exits
}

// PrepareServerDir writes eula.txt (accept) and merges server.properties so a
// server started from dir binds the expected port. Existing settings the user
// already configured are preserved; only missing keys get defaults.
func PrepareServerDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s: %w", dir, err)
	}
	if !isFile(filepath.Join(dir, "eula.txt")) {
		// Accepting the EULA is required before a vanilla server starts.
		if err := os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("eula=true\n"), 0o644); err != nil {
			return fmt.Errorf("写 eula.txt: %w", err)
		}
	}
	defaults := map[string]string{
		"server-port": "25565",
		"online-mode": "false", // LAN-style play between friends; keep if already set
		"motd":        "Eliauk VPN",
	}
	propsPath := filepath.Join(dir, "server.properties")
	lines := []string{}
	if raw, err := os.ReadFile(propsPath); err == nil {
		lines = strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	seen := map[string]bool{}
	for i, l := range lines {
		key := strings.TrimSpace(strings.SplitN(l, "=", 2)[0])
		if v, ok := defaults[key]; ok {
			lines[i] = key + "=" + v
			seen[key] = true
		}
	}
	for k, v := range defaults {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	if err := os.WriteFile(propsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("写 server.properties: %w", err)
	}
	return nil
}

// StartServer launches the dedicated server with java. logf receives each
// stdout/stderr line (called from a goroutine). Returns nil, err on failure.
func StartServer(java, jar string, logf func(string)) (*Server, error) {
	if java == "" {
		return nil, errors.New("未找到 Java，请在“游戏”面板手动填写路径")
	}
	if jar == "" {
		return nil, errors.New("未找到服务器 jar，请在“游戏”面板手动填写路径")
	}
	dir := filepath.Dir(jar)
	if err := PrepareServerDir(dir); err != nil {
		return nil, err
	}
	cmd := exec.Command(java, "-Xmx1G", "-Xms1G", "-jar", jar, "nogui")
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr: %w", err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动服务器失败: %w", err)
	}
	s := &Server{cmd: cmd, in: in, done: make(chan struct{})}
	// Reap the process so Running() can observe it exiting (and so we don't leak
	// the handle); the scan goroutines above drain its output to EOF.
	go func() { _ = cmd.Wait(); close(s.done) }()
	go scanLines(stdout, logf)
	go scanLines(stderr, logf)
	return s, nil
}

// Running reports whether the server process is alive.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Stop asks the server to shut down gracefully ("stop" on stdin so the world
// saves), then force-kills it if it hasn't exited within a grace period.
func (s *Server) Stop() {
	s.mu.Lock()
	cmd := s.cmd
	in := s.in
	done := s.done
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if in != nil {
		_, _ = io.WriteString(in, "stop\n")
	}
	select {
	case <-done:
		return // exited gracefully after saving the world
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// LaunchLauncher starts the user's Minecraft launcher: PCL when detected,
// otherwise the official launcher. exe, when non-empty, overrides detection.
func LaunchLauncher(exe string) error {
	if exe == "" {
		exe = LauncherPath()
	}
	if exe == "" {
		return errors.New("未找到 Minecraft 启动器（PCL 或 .minecraft\\launcher\\MinecraftLauncher.exe）")
	}
	if err := exec.Command(exe).Start(); err != nil {
		return fmt.Errorf("启动游戏失败: %w", err)
	}
	return nil
}

func scanLines(r io.Reader, logf func(string)) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if logf != nil {
			logf(sc.Text())
		}
	}
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
