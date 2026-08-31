//go:build windows

// Command gui is the Eliauk VPN Windows application: a foolproof main window
// with a resident tray icon. A non-technical user can double-click the exe,
// fill in a nickname and server address, paste friends' codes, and play — no
// command-line flags needed. Settings and friends persist to
// %AppData%\Eliauk\config.json; the X25519 identity stays in its own keyfile.
//
// It wraps the same headless agent as cmd/client (internal/agent); this file
// only orchestrates config <-> agent <-> window/tray and the auto-reconnect
// lifecycle.
//
// Build for a windowless release: go build -ldflags "-H windowsgui" ./cmd/gui
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"eliaukvpn/internal/agent"
	"eliaukvpn/internal/config"
	"eliaukvpn/internal/crypto"
	"eliaukvpn/internal/p2p"
	"eliaukvpn/internal/tray"
	"eliaukvpn/internal/window"
)

// Tray menu command IDs.
const (
	actOpen = 1 // show the main window
	actQuit = 2 // quit Eliauk
)

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
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Flags override config (useful for testing); otherwise the config rules.
	if *name != "" {
		cfg.Name = *name
	}
	if *serverAddr != "" {
		cfg.Server = *serverAddr
	}

	// Load the identity up front so the friend code shows even before the
	// first connection; the agent reuses the same keyfile.
	identity, err := crypto.LoadOrCreate(*keyfile)
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}

	if *useVnic && !window.IsElevated() && !*noElevate {
		if err := window.RelaunchElevated(); err == nil {
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
		myCode:    identity.Fingerprint(),
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

	win, err := window.New()
	if err != nil {
		return err
	}
	a.win = win

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- win.Run() }()
	for win.Hwnd() == 0 {
		select {
		case err := <-runErrCh:
			return fmt.Errorf("window: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	tr := tray.NewOnWindow(win.Hwnd())
	a.tr = tr
	tr.SetTooltip("Eliauk VPN — 连接中…")
	tr.SetMenu([]tray.Item{
		{Label: "打开窗口", ID: actOpen},
		{Separator: true},
		{Label: "退出 Eliauk VPN", ID: actQuit},
	})
	// Intercept the tray's callback message before wndProc. Left-click handling
	// belongs to the tray; double-click restores the window.
	win.SetMsgHook(func(m uint32, wParam, lParam uintptr) bool {
		if m == tray.CallbackMsg {
			if lParam == tray.LDblClk {
				win.Show()
			} else {
				tr.HandleTrayMsg(lParam)
			}
			return true
		}
		return false
	})
	if err := tr.Add(); err != nil {
		log.Printf("tray: %v", err)
	}

	// Forward tray menu selections to a channel so the main loop can select on
	// them alongside the window events.
	selCh := make(chan int)
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

	go a.agentLoop()
	go a.tickLoop()

	var exitTimer <-chan time.Time
	if *exitAfter > 0 {
		log.Printf("automation hook: will quit after %s", *exitAfter)
		exitTimer = time.After(*exitAfter)
	}

	for {
		select {
		case ev := <-win.Events():
			a.handleEvent(ev)
		case id, ok := <-selCh:
			if !ok {
				a.quit()
			} else if id == actOpen {
				win.Show()
			} else if id == actQuit {
				a.quit()
			}
		case <-exitTimer:
			a.quit()
		case <-a.quitCh:
			win.Stop()
			tr.Delete()
			select {
			case <-win.Done():
			case <-time.After(2 * time.Second):
			}
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
	myCode  string

	win       *window.Window
	tr        *tray.Tray
	restartCh chan struct{} // buffered 1: "settings changed, rebuild the agent"
	quitCh    chan struct{}

	mu       sync.Mutex
	ag       *agent.Agent
	cancel   context.CancelFunc // cancels the running agent's context
	note     string             // transient status note (success/error toast)
	noteGood bool
	noteAt   time.Time

	quitOnce sync.Once
}

// quit stops the agent and asks the window/tray to shut down. Idempotent.
func (a *app) quit() {
	a.quitOnce.Do(func() {
		a.mu.Lock()
		if a.cancel != nil {
			a.cancel()
		}
		a.mu.Unlock()
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
		hasCfg := a.cfg.Name != "" && a.cfg.Server != ""
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

	var keys [][]byte
	for _, f := range cfg.Friends {
		if raw, err := crypto.ParseFingerprint(f.Code); err == nil {
			keys = append(keys, raw)
		}
	}
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
		Friends:       keys,
		Info:          log.Printf,
		Logf:          log.Printf,
	}
}

// tickLoop repaints the window and the tray tooltip once a second.
func (a *app) tickLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			a.win.SetView(a.view())
			trTooltip(a)
		case <-a.quitCh:
			return
		}
	}
}

// view snapshots the current agent/config state into the window's renderable
// model. The listbox mirrors cfg.Friends one row each (row i ↔ friend i), so
// the delete button's listbox index maps straight back to a friend.
func (a *app) view() window.View {
	a.mu.Lock()
	cfg := *a.cfg
	ag := a.ag
	note, noteGood, noteAt := a.note, a.noteGood, a.noteAt
	a.mu.Unlock()

	v := window.View{Code: a.myCode, Name: cfg.Name, Server: cfg.Server}

	switch {
	case note != "" && time.Since(noteAt) < 4*time.Second:
		v.Status, v.Good = note, noteGood
	case cfg.Name == "" || cfg.Server == "":
		v.Status = "请先填写昵称和服务器地址，然后点“保存并连接”"
	case ag == nil:
		v.Status = "正在连接服务器…"
	default:
		st := ag.Status()
		switch {
		case st.VnicMsg != "":
			v.Status = "虚拟网卡不可用：" + st.VnicMsg
		case st.Registered:
			v.Status = fmt.Sprintf("已连接 · %s · 虚拟IP %s", st.Name, st.VirtualIP)
			v.Good = true
		default:
			v.Status = "连接中…"
		}
	}

	online, conn := peerState(ag)
	for _, f := range cfg.Friends {
		v.Rows = append(v.Rows, friendRow(f, online, conn))
	}
	return v
}

// peerState maps (lowercased) peer names to their online status and tunnel
// connection state.
func peerState(ag *agent.Agent) (online map[string]bool, conn map[string]p2p.State) {
	online = make(map[string]bool)
	conn = make(map[string]p2p.State)
	if ag == nil {
		return online, conn
	}
	for _, p := range ag.Peers() {
		online[strings.ToLower(p.Name)] = true
	}
	for _, s := range ag.Snapshot() {
		k := strings.ToLower(s.Name)
		online[k] = true
		conn[k] = s.State
	}
	return online, conn
}

// friendRow renders one friend as a listbox line with its live state.
func friendRow(f config.Friend, online map[string]bool, conn map[string]p2p.State) string {
	name := strings.TrimSpace(f.Name)
	if name == "" {
		name = "(未命名)"
	}
	key := strings.ToLower(strings.TrimSpace(f.Name))
	switch conn[key] {
	case p2p.StateConnected:
		return name + " — 已连接"
	case p2p.StateConnecting:
		return name + " — 连接中"
	}
	if online[key] {
		return name + " — 在线"
	}
	return name + " — 离线"
}

// trTooltip updates the tray tooltip from the live agent state (notes are not
// shown in the tooltip).
func trTooltip(a *app) {
	a.mu.Lock()
	ag := a.ag
	cfg := *a.cfg
	a.mu.Unlock()
	switch {
	case cfg.Name == "" || cfg.Server == "":
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

// handleEvent applies one window action to config and the live agent.
func (a *app) handleEvent(ev window.EvMsg) {
	switch ev.Type {
	case window.EvCopy:
		window.CopyToClipboard(a.win.Hwnd(), a.myCode)
		a.setNote("好友码已复制到剪贴板", true)
	case window.EvAdd:
		a.addFriend(ev.Text2, ev.Text)
	case window.EvDelete:
		a.deleteFriend(ev.Index)
	case window.EvSave:
		a.saveSettings(ev.Text, ev.Text2)
	case window.EvQuit:
		a.quit()
	}
}

// addFriend validates and stores a friend code, then pushes it to the live
// agent so the whitelist updates immediately.
func (a *app) addFriend(name, code string) {
	code = strings.TrimSpace(code)
	name = strings.TrimSpace(name)
	if code == "" {
		a.setNote("请先粘贴好友的好友码", false)
		return
	}
	if _, err := crypto.ParseFingerprint(code); err != nil {
		a.setNote("好友码格式不对："+err.Error(), false)
		return
	}
	a.mu.Lock()
	added := a.cfg.AddFriend(name, code)
	ag := a.ag
	a.mu.Unlock()
	if !added {
		a.setNote("这个好友码已经在列表里了", false)
		return
	}
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.setNote("保存配置失败："+err.Error(), false)
		return
	}
	if ag != nil {
		_ = ag.AddFriend(code)
	}
	a.setNote("已添加好友", true)
}

// deleteFriend removes the friend at listbox row idx from config and the live
// agent.
func (a *app) deleteFriend(idx int) {
	a.mu.Lock()
	if idx < 0 || idx >= len(a.cfg.Friends) {
		a.mu.Unlock()
		return
	}
	f := a.cfg.Friends[idx]
	a.cfg.RemoveFriend(f.Code)
	ag := a.ag
	a.mu.Unlock()
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.setNote("保存配置失败："+err.Error(), false)
		return
	}
	if ag != nil {
		_ = ag.RemoveFriend(f.Code)
	}
	a.setNote("已删除好友", true)
}

// saveSettings persists the nickname/server and restarts the agent with them.
func (a *app) saveSettings(name, server string) {
	name = strings.TrimSpace(name)
	server = strings.TrimSpace(server)
	if name == "" || server == "" {
		a.setNote("昵称和服务器地址都不能为空", false)
		return
	}
	a.mu.Lock()
	a.cfg.Name = name
	a.cfg.Server = server
	a.mu.Unlock()
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.setNote("保存配置失败："+err.Error(), false)
		return
	}
	a.signalRestart()
	a.setNote("已保存，正在连接…", true)
}
