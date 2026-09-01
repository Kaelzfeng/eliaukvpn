//go:build windows

// Command gui is the Eliauk VPN Windows application: a foolproof main window
// (WebView2 / Edge Chromium) with a resident tray icon. A non-technical user can
// double-click the exe, fill in a nickname and server address, and play — no
// command-line flags needed. Settings persist to %AppData%\Eliauk\config.json;
// the X25519 identity stays in its own keyfile.
//
// It wraps the same headless agent as cmd/client (internal/agent); this file
// only orchestrates config <-> agent <-> window/tray and the auto-reconnect
// lifecycle.
//
// Build for a windowless release: go build -ldflags "-H windowsgui" ./cmd/gui
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/agent"
	"eliaukvpn/internal/config"
	"eliaukvpn/internal/mc"
	"eliaukvpn/internal/tray"
	"eliaukvpn/internal/update"
	"eliaukvpn/internal/webviewhost"
	"eliaukvpn/internal/winutil"
)

// Tray menu command IDs.
const (
	actOpen = 1 // show the main window
	actQuit = 2 // quit Eliauk
)

// version is the build version shown in the 关于 panel and printed by -version.
// Override at build time with: go build -ldflags "-X main.version=1.2.3".
var version = "1.0.0"

// defaultServer is the coordination-server WebSocket URL pre-filled into the
// settings on a fresh install (when no server has been saved yet), so the app
// can connect out of the box instead of demanding a hand-typed address.
// Override at build time with: go build -ldflags "-X main.defaultServer=wss://…".
var defaultServer = "wss://vpn.kaelzfeng.uk/ws"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		configPath    = flag.String("config", config.DefaultPath(), "path to the settings JSON file")
		name          = flag.String("name", "", "display name (default: from config)")
		serverAddr    = flag.String("server", "", "coordination server WebSocket URL (default: from config)")
		stunPrimary   = flag.String("stun-primary", "stun.l.google.com:19302", "primary STUN server")
		stunSecondary = flag.String("stun-secondary", "stun.cloudflare.com:3478", "secondary STUN server (symmetry detection)")
		forceRelay    = flag.Bool("force-relay", false, "skip direct punching, always relay via server")
		useVnic       = flag.Bool("vnic", true, "create and use the Wintun virtual NIC")
		vnicName      = flag.String("vnic-name", "", "virtual NIC adapter name (default Eliauk-<name>)")
		lanEmu        = flag.Bool("lan", true, "emulate Minecraft LAN discovery (UDP 4445 broadcast fan-out)")
		debugPackets  = flag.Bool("debug-packets", false, "log every packet between the virtual NIC and the tunnel")
		keyfile       = flag.String("keyfile", agent.DefaultKeyfile(), "path to the X25519 identity key")
		noElevate     = flag.Bool("no-elevate", false, "skip the UAC elevation prompt (virtual NIC will be unavailable)")
		exitAfter     = flag.Duration("exit-after", 0, "automation hook: quit after this long (0 = run until quit)")
		gameStartJar  = flag.String("game-start", "", "automation hook: start the dedicated server with this jar after registering (e2e)")
		createRoom    = flag.Bool("create-room", false, "automation hook: create a room after registering and log its code (e2e)")
		joinRoom      = flag.String("join-room", "", "automation hook: join this room code after registering (e2e)")
		updateCheck   = flag.String("update-check", "", "automation hook: run the self-update check against this feed and log the result (e2e)")
		versionFlag   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Remove update artifacts left by an interrupted self-update (the install
	// bat deletes itself and the downloaded exe on success, so anything that
	// remains is orphaned).
	update.CleanupStale()
	// Flags override config (useful for testing); otherwise the config rules.
	if *name != "" {
		cfg.Name = *name
	}
	if *serverAddr != "" {
		cfg.Server = *serverAddr
	}
	// A fresh install has no server saved: fall back to the bundled default so
	// the app can connect immediately (the user only needs to pick a nickname).
	if cfg.Server == "" {
		cfg.Server = defaultServer
	}

	if *useVnic && !winutil.IsElevated() && !*noElevate {
		if err := winutil.RelaunchElevated(); err == nil {
			// The elevated copy has taken over (same command line); exit here.
			return nil
		}
		log.Printf("elevation declined; continuing without the virtual NIC")
	}

	// Cap the server dial so a dead server can't freeze the UI for minutes.
	websocket.DefaultDialer.HandshakeTimeout = 10 * time.Second

	a := &app{
		cfgPath:   *configPath,
		cfg:       cfg,
		restartCh: make(chan struct{}, 1),
		quitCh:    make(chan struct{}),
		opts: options{
			stunPrimary:   *stunPrimary,
			stunSecondary: *stunSecondary,
			forceRelay:    *forceRelay,
			useVnic:       *useVnic,
			vnicName:      *vnicName,
			lanEmu:        *lanEmu,
			debugPackets:  *debugPackets,
			keyfile:       *keyfile,
		},
	}

	// M7c game panel: prefill the Java / server.jar paths from the config,
	// falling back to auto-detection on a fresh install.
	a.mcJava = cfg.Java
	if a.mcJava == "" {
		a.mcJava = mc.FindJava()
	}
	a.mcJar = cfg.ServerJar
	if a.mcJar == "" {
		a.mcJar = mc.FindServerJar()
	}
	a.mcLauncher = cfg.Launcher
	if a.mcLauncher == "" {
		a.mcLauncher = mc.LauncherPath()
	}
	a.mcGameDir = cfg.GameDir
	if a.mcGameDir == "" {
		a.mcGameDir = mc.GameDir()
	}
	log.Printf("game: java=%q server-jar=%q launcher=%q gamedir=%q", a.mcJava, a.mcJar, a.mcLauncher, a.mcGameDir)

	// Create the WebView2 host on a dedicated, LockOSThread'd goroutine; the
	// window is created synchronously inside Run before it pumps messages.
	ui := webviewhost.New()
	a.ui = ui
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- ui.Run() }()
	select {
	case <-ui.Ready():
		// window up and bridge wired
	case err := <-runErrCh:
		return fmt.Errorf("webview: %v", err)
	}

	// Resident tray icon (self-pumping message-only window). If it can't be
	// created the app still runs window-only.
	tr, err := tray.New()
	if err != nil {
		log.Printf("tray: %v", err)
	}
	var selCh chan int
	if tr != nil {
		a.tr = tr
		tr.SetTooltip("Eliauk VPN — 连接中…")
		tr.SetMenu([]tray.Item{
			{Label: "打开窗口", ID: actOpen},
			{Separator: true},
			{Label: "退出 Eliauk VPN", ID: actQuit},
		})
		if err := tr.Add(); err != nil {
			log.Printf("tray: %v", err)
		}
		go func() { _ = tr.Run(nil) }()
		selCh = make(chan int)
		go func() {
			for {
				id, ok := tr.Select()
				if !ok {
					close(selCh)
					return
				}
				selCh <- id
			}
		}()
	}

	if *gameStartJar != "" {
		go a.autoStartGame(*gameStartJar)
	}
	if *createRoom {
		go a.autoCreateRoom()
	}
	if *joinRoom != "" {
		go a.autoJoinRoom(*joinRoom)
	}
	if *updateCheck != "" {
		go func() {
			// Wait for the frontend so state pushes render, then run one check.
			time.Sleep(2 * time.Second)
			a.mu.Lock()
			a.cfg.UpdateURL = *updateCheck
			a.mu.Unlock()
			a.checkUpdate()
		}()
	}
	go a.agentLoop()
	go a.tickLoop()

	var exitTimer <-chan time.Time
	if *exitAfter > 0 {
		log.Printf("automation hook: will quit after %s", *exitAfter)
		exitTimer = time.After(*exitAfter)
	}

	for {
		select {
		case act := <-ui.Actions():
			a.handleAction(act)
		case id, ok := <-selCh:
			if !ok {
				a.quit()
			} else if id == actOpen || id == tray.DblClickID {
				ui.Show()
			} else if id == actQuit {
				a.quit()
			}
		case <-ui.Done():
			// The window terminated without going through quit() (e.g. an
			// external destroy); treat it as a shutdown request.
			a.quit()
		case <-exitTimer:
			a.quit()
		case <-a.quitCh:
			ui.Close()
			select {
			case <-ui.Done():
			case <-time.After(2 * time.Second):
			}
			if tr != nil {
				tr.Stop()
				select {
				case <-tr.Done():
				case <-time.After(2 * time.Second):
				}
				tr.Delete()
			}
			// Apply a staged update last: spawn the detached swap script, then
			// exit immediately so the old exe is free for overwriting.
			a.installPendingUpdate()
			return nil
		}
	}
}

// options are the agent settings that are not user-editable in the window.
type options struct {
	stunPrimary, stunSecondary  string
	forceRelay, useVnic, lanEmu bool
	debugPackets                bool
	vnicName, keyfile           string
}

// app owns the shared state between the event loop, the agent lifecycle, and
// the 1s status ticker.
type app struct {
	cfgPath string
	cfg     *config.Config
	opts    options

	ui        *webviewhost.Host
	tr        *tray.Tray
	uiReady   bool
	restartCh chan struct{} // buffered 1: "settings changed, rebuild the agent"
	quitCh    chan struct{}

	mu       sync.Mutex
	ag       *agent.Agent
	cancel   context.CancelFunc // cancels the running agent's context
	note     string             // transient status note (success/error toast)
	noteGood bool
	noteAt   time.Time

	// M7c game panel: remembered Java / server.jar paths, the running dedicated
	// server process, and a short tail of its output (all under mu).
	mcJava     string
	mcJar      string
	mcLauncher string
	mcGameDir  string
	mcSrv      *mc.Server
	mcTail     []string

	// Self-update state (under mu): a downloaded new exe awaiting install on
	// the next quit, plus the About-panel status/version/notes.
	updStatus  string
	updVersion string
	updNotes   string
	updReady   bool
	updExe     string // temp path of the downloaded new exe; "" = none pending

	quitOnce sync.Once
}

// quit stops the agent and the dedicated Minecraft server (if any) and asks the
// window/tray to shut down. Idempotent.
func (a *app) quit() {
	a.quitOnce.Do(func() {
		a.mu.Lock()
		if a.cancel != nil {
			a.cancel()
		}
		srv := a.mcSrv
		a.mcSrv = nil
		a.mu.Unlock()
		if srv != nil {
			srv.Stop()
			log.Printf("game: server stopped at exit")
		}
		close(a.quitCh)
	})
}

// setNote shows a transient status message on the next tick (4s lifetime).
func (a *app) setNote(s string, good bool) {
	a.mu.Lock()
	a.note, a.noteGood, a.noteAt = s, good, time.Now()
	a.mu.Unlock()
}

// signalRestart cancels the running agent and wakes agentLoop to rebuild it
// with the current settings.
func (a *app) signalRestart() {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	select {
	case a.restartCh <- struct{}{}:
	default:
	}
}

// agentLoop owns the agent's lifecycle: it stays idle until settings exist,
// creates an agent, runs it until it errors or is cancelled, then
// auto-reconnects after 3s or rebuilds immediately when settings change.
func (a *app) agentLoop() {
	for {
		a.mu.Lock()
		// The agent starts when a server is set and there's a display name to
		// identify as (the name is how peers and room members see you).
		hasCfg := a.cfg.Server != "" && a.cfg.Name != ""
		a.mu.Unlock()

		if !hasCfg {
			// Idle: wait for the user to save settings.
			if _, ok := <-a.restartCh; !ok {
				return
			}
			continue
		}

		ag, err := agent.New(a.agentOptions())
		if err != nil {
			a.setNote("连接服务器失败："+err.Error(), false)
			select {
			case <-a.restartCh: // settings changed: retry immediately
			case <-time.After(3 * time.Second):
			}
			continue
		}

		// Settings changed while we were dialing? Discard this stale agent.
		select {
		case <-a.restartCh:
			ag.Close()
			continue
		default:
		}

		ctx, cancel := context.WithCancel(context.Background())
		a.mu.Lock()
		a.ag = ag
		a.cancel = cancel
		a.mu.Unlock()

		err = ag.Run(ctx)

		a.mu.Lock()
		a.ag = nil
		a.cancel = nil
		a.mu.Unlock()
		cancel()

		ag.Close()

		if err != nil {
			a.setNote("连接中断，正在重连…", false)
		}
		select {
		case <-a.restartCh: // settings changed: rebuild with the new values
		case <-time.After(3 * time.Second): // server gone: auto-reconnect
		}
	}
}

// agentOptions builds the agent configuration from the current settings.
func (a *app) agentOptions() agent.Options {
	a.mu.Lock()
	cfg := *a.cfg
	a.mu.Unlock()

	return agent.Options{
		Name:          cfg.Name,
		Server:        cfg.Server,
		StunPrimary:   a.opts.stunPrimary,
		StunSecondary: a.opts.stunSecondary,
		ForceRelay:    a.opts.forceRelay,
		UseVnic:       a.opts.useVnic,
		VnicName:      a.opts.vnicName,
		LanEmu:        a.opts.lanEmu,
		DebugPackets:  a.opts.debugPackets,
		Keyfile:       a.opts.keyfile,
		Info:          log.Printf,
		Logf:          log.Printf,
	}
}

// tickLoop repaints the window once a second (once the frontend has signalled
// ready).
func (a *app) tickLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			trTooltip(a)
			a.mu.Lock()
			ready := a.uiReady
			a.mu.Unlock()
			if ready {
				a.ui.Push(a.state())
			}
		case <-a.quitCh:
			return
		}
	}
}

// state snapshots the current agent/config state into the frontend's renderable
// model. Room membership is the single connectivity gate: same-room peers become
// visible and reachable automatically.
func (a *app) state() webviewhost.State {
	a.mu.Lock()
	cfg := *a.cfg
	ag := a.ag
	note, noteGood, noteAt := a.note, a.noteGood, a.noteAt
	a.mu.Unlock()

	s := webviewhost.State{
		Fullscreen: a.ui.Fullscreen(),
		Version:    version,
		Settings:   webviewhost.Settings{Name: cfg.Name, Server: cfg.Server, UpdateURL: cfg.UpdateURL},
	}

	var st agent.Status
	if ag != nil {
		st = ag.Status()
	}

	switch {
	case note != "" && time.Since(noteAt) < 4*time.Second:
		s.Status = webviewhost.StatusInfo{Text: note, Good: noteGood}
	case cfg.Name == "":
		s.Status = webviewhost.StatusInfo{Text: "请先填写昵称，然后点“保存并连接”"}
	case cfg.Server == "":
		s.Status = webviewhost.StatusInfo{Text: "请先填写服务器地址，然后点“保存并连接”"}
	case ag == nil:
		s.Status = webviewhost.StatusInfo{Text: "正在连接服务器…"}
	default:
		switch {
		case st.VnicMsg != "":
			s.Status = webviewhost.StatusInfo{Text: "虚拟网卡不可用：" + st.VnicMsg}
		case st.Registered:
			s.Status = webviewhost.StatusInfo{Text: fmt.Sprintf("已连接 · %s · 虚拟IP %s", st.Name, st.VirtualIP), Good: true}
		default:
			s.Status = webviewhost.StatusInfo{Text: "连接中…"}
		}
	}
	s.Status.Dot = statusDot(s.Status)

	if ag != nil {
		if room := ag.RoomState(); room != nil {
			s.Room.In = true
			s.Room.Code = room.Code
			for _, m := range room.Members {
				s.Room.Members = append(s.Room.Members, webviewhost.RoomMember{
					Username:  m.Username,
					VirtualIP: m.VirtualIP,
					Host:      m.Host,
				})
			}
		}
	}

	// M7c game panel: prefilled paths plus the running/joinable state. When a
	// dedicated server runs locally its address is our own virtual IP:25565;
	// otherwise the room host's address is the one to join.
	a.mu.Lock()
	s.Game.Java, s.Game.Jar = a.mcJava, a.mcJar
	s.Game.Launcher, s.Game.GameDir = a.mcLauncher, a.mcGameDir
	gameRunning := a.mcSrv != nil && a.mcSrv.Running()
	a.mu.Unlock()
	s.Game.Running = gameRunning

	// Self-update status for the About panel.
	a.mu.Lock()
	s.Update = webviewhost.UpdateInfo{
		Status:  a.updStatus,
		Ready:   a.updReady,
		Version: a.updVersion,
		Notes:   a.updNotes,
	}
	a.mu.Unlock()

	if addr := a.joinableAddr(); addr != "" {
		s.Game.Addr = addr
		if gameRunning {
			s.Game.State = "服务器运行中 · 可加入 " + addr
		} else {
			s.Game.State = "未运行 · 可加入 " + addr
		}
	} else if gameRunning {
		s.Game.State = "服务器运行中"
	} else {
		s.Game.State = "未运行"
	}

	return s
}

// statusDot maps a status line to its dot color: green when good, amber for an
// in-flight "连接…" message, red otherwise.
func statusDot(st webviewhost.StatusInfo) string {
	if st.Good {
		return "ok"
	}
	if strings.Contains(st.Text, "连接") {
		return "busy"
	}
	return "warn"
}

// trTooltip updates the tray tooltip from the live agent state (notes are not
// shown in the tooltip).
func trTooltip(a *app) {
	if a.tr == nil {
		return
	}
	a.mu.Lock()
	ag := a.ag
	cfg := *a.cfg
	a.mu.Unlock()
	switch {
	case cfg.Server == "" || cfg.Name == "":
		a.tr.SetTooltip("Eliauk VPN — 未连接")
	case ag == nil:
		a.tr.SetTooltip("Eliauk VPN — 连接中…")
	default:
		st := ag.Status()
		if st.Registered {
			a.tr.SetTooltip(fmt.Sprintf("Eliauk VPN — %s (%s)", st.Name, st.VirtualIP))
		} else {
			a.tr.SetTooltip("Eliauk VPN — 连接中…")
		}
	}
}

// handleAction applies one frontend action to config and the live agent.
func (a *app) handleAction(act webviewhost.Action) {
	switch act.Type {
	case webviewhost.ActReady:
		log.Printf("webview: frontend ready")
		a.mu.Lock()
		a.uiReady = true
		a.mu.Unlock()
		a.ui.Push(a.state())
	case webviewhost.ActCopy:
		winutil.CopyToClipboard(a.ui.Window(), act.Text)
		a.setNote("已复制到剪贴板", true)
	case webviewhost.ActSave:
		a.saveSettings(act.Name, act.Server, act.UpdateURL)
	case webviewhost.ActQuit:
		a.quit()
	case webviewhost.ActCheckUpdate:
		go a.checkUpdate()
	case webviewhost.ActApplyUpdate:
		a.quit()
	case webviewhost.ActRoomCreate:
		a.roomAction(func(ag *agent.Agent) error { return ag.CreateRoom() }, "已创建房间", true)
	case webviewhost.ActRoomJoin:
		a.roomAction(func(ag *agent.Agent) error { return ag.JoinRoom(act.Code) }, "已加入房间", true)
	case webviewhost.ActRoomLeave:
		a.roomAction(func(ag *agent.Agent) error { return ag.LeaveRoom() }, "已离开房间", true)
	case webviewhost.ActGameDetect:
		a.detectGame()
	case webviewhost.ActSrvStart:
		a.startGame(act.Java, act.Jar)
	case webviewhost.ActSrvStop:
		a.stopGame()
	case webviewhost.ActMCAdd:
		a.addToLauncher(act.GameDir)
	case webviewhost.ActLaunch:
		a.launchGame(act.Launcher)
	case webviewhost.ActFullscreen:
		a.ui.ToggleFullscreen()
		a.ui.Push(a.state())
	}
}

// roomAction runs one room operation on the live agent and reports the result.
func (a *app) roomAction(fn func(*agent.Agent) error, okNote string, good bool) {
	a.mu.Lock()
	ag := a.ag
	a.mu.Unlock()
	if ag == nil {
		a.setNote("尚未连接服务器", false)
		return
	}
	if err := fn(ag); err != nil {
		a.setNote(err.Error(), false)
		return
	}
	a.setNote(okNote, good)
}

// ---- M7c game panel ----

// joinableAddr is the Minecraft server address to copy/show: our own virtual
// IP when a dedicated server is running locally, otherwise the room host's
// virtual IP (the one running the game server guests should join).
func (a *app) joinableAddr() string {
	a.mu.Lock()
	ag := a.ag
	srv := a.mcSrv
	a.mu.Unlock()
	if ag == nil {
		return ""
	}
	if srv != nil && srv.Running() {
		if vip := ag.Status().VirtualIP; vip != "" {
			return vip + ":25565"
		}
	}
	if room := ag.RoomState(); room != nil {
		for _, m := range room.Members {
			if m.Host && m.VirtualIP != "" {
				return m.VirtualIP + ":25565"
			}
		}
	}
	return ""
}

// gameLogf returns the log callback the dedicated server feeds; it keeps a
// bounded tail of the server's output under the app lock.
func (a *app) gameLogf() func(string) {
	return func(line string) {
		a.mu.Lock()
		a.mcTail = append(a.mcTail, line)
		if len(a.mcTail) > 20 {
			a.mcTail = a.mcTail[len(a.mcTail)-20:]
		}
		a.mu.Unlock()
	}
}

// autoStartGame is the -game-start automation hook: once the agent is
// registered, start the dedicated server with the given jar and the auto-
// detected (or configured) Java. Bound so a failed registration can't hang the
// e2e.
func (a *app) autoStartGame(jar string) {
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		ag := a.ag
		a.mu.Unlock()
		if ag != nil && ag.Status().Registered {
			a.startGame("", jar)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	log.Printf("game: auto-start aborted: agent not registered within 40s")
}

// autoCreateRoom is the -create-room automation hook: once the agent is
// registered, create a room and log its code so the e2e can hand it to a second
// instance to join.
func (a *app) autoCreateRoom() {
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		ag := a.ag
		a.mu.Unlock()
		if ag != nil && ag.Status().Registered {
			if err := ag.CreateRoom(); err != nil {
				log.Printf("room: create failed: %v", err)
				return
			}
			for time.Now().Before(deadline) {
				if r := ag.RoomState(); r != nil && r.Code != "" {
					log.Printf("room: created code=%s", r.Code)
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
			log.Printf("room: created but no code within deadline")
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	log.Printf("room: auto-create aborted: agent not registered within 40s")
}

// autoJoinRoom is the -join-room automation hook: once the agent is registered,
// join the given room code so the e2e can pair two instances.
func (a *app) autoJoinRoom(code string) {
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		ag := a.ag
		a.mu.Unlock()
		if ag != nil && ag.Status().Registered {
			if err := ag.JoinRoom(code); err != nil {
				log.Printf("room: join failed: %v", err)
				return
			}
			log.Printf("room: join requested code=%s", code)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	log.Printf("room: auto-join aborted: agent not registered within 40s")
}

// persistGamePaths remembers the last-used Java / server.jar so the next start
// prefills them.
func (a *app) persistGamePaths(java, jar string) {
	a.mu.Lock()
	a.mcJava, a.mcJar = java, jar
	a.cfg.Java, a.cfg.ServerJar = java, jar
	a.mu.Unlock()
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.setNote("保存配置失败："+err.Error(), false)
	}
}

// persistLauncher remembers the detected launcher / game directory so they are
// not re-detected (and possibly mis-detected) on every start.
func (a *app) persistLauncher(launcher, gameDir string) {
	a.mu.Lock()
	a.mcLauncher, a.mcGameDir = launcher, gameDir
	a.cfg.Launcher, a.cfg.GameDir = launcher, gameDir
	a.mu.Unlock()
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.setNote("保存配置失败："+err.Error(), false)
	}
}

// detectGame re-runs Java / server.jar detection and fills the edit boxes.
func (a *app) detectGame() {
	java := mc.FindJava()
	jar := mc.FindServerJar()
	launcher := mc.LauncherPath()
	gameDir := mc.GameDir()
	a.persistGamePaths(java, jar)
	a.persistLauncher(launcher, gameDir)
	switch {
	case java == "" && jar == "":
		a.setNote("未找到 Java 和服务器 jar，请手动填写路径", false)
	case java == "":
		a.setNote("已找到服务器 jar，未找到 Java，请手动填写", false)
	case jar == "":
		a.setNote("已找到 Java，未找到服务器 jar，请手动填写", false)
	default:
		a.setNote("已自动检测到 Java 和服务器 jar", true)
	}
}

// startGame launches the dedicated server with the given java/jar (the current
// edit-box contents), falling back to detection when the boxes are empty.
func (a *app) startGame(java, jar string) {
	java = strings.TrimSpace(java)
	jar = strings.TrimSpace(jar)
	a.mu.Lock()
	already := a.mcSrv != nil && a.mcSrv.Running()
	a.mu.Unlock()
	if already {
		a.setNote("服务器已经在运行了", false)
		return
	}
	if java == "" {
		java = mc.FindJava()
	}
	if jar == "" {
		jar = mc.FindServerJar()
	}
	if java == "" || jar == "" {
		a.setNote("请先点“自动检测”或手动填写 Java 与服务器 jar 路径", false)
		return
	}
	srv, err := mc.StartServer(java, jar, a.gameLogf())
	if err != nil {
		a.setNote(err.Error(), false)
		return
	}
	a.mu.Lock()
	a.mcSrv = srv
	a.mu.Unlock()
	a.persistGamePaths(java, jar)
	addr := a.joinableAddr()
	log.Printf("game: server started (addr %s)", addr)
	if addr != "" {
		a.setNote("服务器已启动，可加入地址 "+addr, true)
	} else {
		a.setNote("服务器已启动", true)
	}
}

// stopGame stops the dedicated server, if one is running.
func (a *app) stopGame() {
	a.mu.Lock()
	srv := a.mcSrv
	a.mcSrv = nil
	a.mu.Unlock()
	if srv == nil {
		a.setNote("服务器未在运行", false)
		return
	}
	srv.Stop()
	a.setNote("服务器已停止", true)
}

// addToLauncher registers the current joinable address in the official
// launcher's multiplayer list (servers.dat), so joining is one click.
func (a *app) addToLauncher(gameDir string) {
	addr := a.joinableAddr()
	if addr == "" {
		a.setNote("暂无可添加的地址：请先启动服务器，或加入有房主的房间", false)
		return
	}
	name := "Eliauk VPN"
	a.mu.Lock()
	ag := a.ag
	a.mu.Unlock()
	if ag != nil {
		if room := ag.RoomState(); room != nil && room.Code != "" {
			name = "Eliauk [" + room.Code + "]"
		}
	}
	if gameDir == "" {
		a.mu.Lock()
		gameDir = a.mcGameDir
		a.mu.Unlock()
	}
	if _, err := mc.AddServerToLauncher(name, addr, gameDir); err != nil {
		a.setNote(err.Error(), false)
		return
	}
	a.setNote("已添加服务器 "+name+" → "+addr+"，打开启动器即可加入", true)
}

// launchGame starts the user's Minecraft launcher (PCL or official).
func (a *app) launchGame(launcher string) {
	if launcher == "" {
		a.mu.Lock()
		launcher = a.mcLauncher
		a.mu.Unlock()
	}
	if err := mc.LaunchLauncher(launcher); err != nil {
		a.setNote(err.Error(), false)
		return
	}
	a.setNote("已启动 Minecraft 启动器", true)
}

// saveSettings persists the nickname/server and restarts the agent with them.
func (a *app) saveSettings(name, server, updateURL string) {
	name = strings.TrimSpace(name)
	server = strings.TrimSpace(server)
	updateURL = strings.TrimSpace(updateURL)
	if name == "" || server == "" {
		a.setNote("昵称和服务器地址都不能为空", false)
		return
	}
	a.mu.Lock()
	// Only the connectivity settings justify tearing down the agent; changing
	// the update feed alone just persists.
	restart := name != a.cfg.Name || server != a.cfg.Server
	a.cfg.Name = name
	a.cfg.Server = server
	a.cfg.UpdateURL = updateURL
	a.mu.Unlock()
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.setNote("保存配置失败："+err.Error(), false)
		return
	}
	if restart {
		a.signalRestart()
		a.setNote("已保存，正在连接…", true)
	} else {
		a.setNote("已保存", true)
	}
}

// checkUpdate runs the self-update flow off the UI thread: fetch the manifest,
// verify its signature, download and SHA-256-verify the new exe, then stage it
// for install at the next quit. Every stage pushes state so the About panel
// reflects progress.
func (a *app) checkUpdate() {
	a.setUpdate("", "", "正在检查更新…", false, "")
	a.pushState()

	a.mu.Lock()
	feed := a.cfg.UpdateURL
	a.mu.Unlock()
	if feed == "" {
		a.setUpdate("", "", "未配置更新源（在设置里填写更新源地址）", false, "")
		a.pushState()
		return
	}

	m, err := update.Check(feed, nil)
	if err != nil {
		log.Printf("update: check failed: %v", err)
		a.setUpdate("", "", "检查更新失败："+err.Error(), false, "")
		a.pushState()
		return
	}
	if err := update.Verify(m, updatePublicKey()); err != nil {
		log.Printf("update: verify failed: %v", err)
		a.setUpdate("", "", "检查更新失败："+err.Error(), false, "")
		a.pushState()
		return
	}
	if !update.Newer(m.Version, version) {
		a.setUpdate("", "", "已是最新版本 v"+version, false, "")
		a.pushState()
		return
	}

	a.setUpdate("", m.Version, "正在下载 v"+m.Version+" …", false, m.Notes)
	a.pushState()
	exe, err := update.Download(m, nil)
	if err != nil {
		log.Printf("update: download failed: %v", err)
		a.setUpdate("", m.Version, "下载失败："+err.Error(), false, "")
		a.pushState()
		return
	}
	a.setUpdate(exe, m.Version, "已下载 v"+m.Version+"，退出时自动安装", true, m.Notes)
	a.pushState()
	log.Printf("update: staged v%s (%s)", m.Version, exe)
}

// installPendingUpdate arms the delayed swap for a staged download. Called at
// the very end of run(), after teardown, so the old exe is free for the bat to
// overwrite the instant this process exits.
func (a *app) installPendingUpdate() {
	a.mu.Lock()
	exe := a.updExe
	a.mu.Unlock()
	if exe == "" {
		return
	}
	cur, err := os.Executable()
	if err != nil {
		log.Printf("update: resolve current exe: %v", err)
		return
	}
	log.Printf("update: installing %s over %s at exit", exe, cur)
	if err := update.Install(exe, cur); err != nil {
		log.Printf("update: install failed: %v", err)
	}
}

func (a *app) setUpdate(exe, ver, status string, ready bool, notes string) {
	a.mu.Lock()
	a.updExe, a.updVersion, a.updStatus, a.updReady, a.updNotes = exe, ver, status, ready, notes
	a.mu.Unlock()
}

func (a *app) pushState() {
	a.ui.Push(a.state())
}

// updatePublicKey decodes the baked-in Ed25519 release key. A blank or invalid
// key returns nil, which makes update.Verify fall back to SHA-256 only.
func updatePublicKey() ed25519.PublicKey {
	raw, err := hex.DecodeString(update.UpdatePubKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}
