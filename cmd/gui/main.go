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
	"eliaukvpn/internal/mc"
	"eliaukvpn/internal/p2p"
	"eliaukvpn/internal/protocol"
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
		accountFlag   = flag.String("account", "", "M7 account to log in as (automation hook; needs -password)")
		passwordFlag  = flag.String("password", "", "password for -account")
		createFlag    = flag.Bool("create-account", false, "register -account instead of logging in")
		gameStartJar  = flag.String("game-start", "", "automation hook: start the dedicated server with this jar after registering (e2e)")
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
	log.Printf("game: java=%q server-jar=%q", a.mcJava, a.mcJar)

	// Automation hook: log in / register an account on startup (used by the e2e
	// script). The plaintext password lives only in a.pendingPass and is never
	// written to the config; the fresh session token is cached after login.
	if *accountFlag != "" {
		if *passwordFlag != "" || *createFlag {
			// Explicit password login / registration: clear any stale token so
			// the server validates the password.
			a.cfg.Account = *accountFlag
			a.cfg.Token = ""
			a.pendingUser = *accountFlag
			a.pendingPass = *passwordFlag
			a.pendingCreate = *createFlag
		} else {
			// No password: keep the cached session token (if any) and just point
			// the account at the given name.
			a.cfg.Account = *accountFlag
		}
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

	if *gameStartJar != "" {
		go a.autoStartGame(*gameStartJar)
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

	// M7: the login attempt currently in flight. pendingUser non-empty means the
	// agent is trying to authenticate as that account; the password is kept only
	// in memory (never persisted) until the server issues a session token.
	pendingUser   string
	pendingPass   string
	pendingCreate bool

	// M7c game panel: remembered Java / server.jar paths, the running dedicated
	// server process, and a short tail of its output (all under mu).
	mcJava  string
	mcJar   string
	mcSrv   *mc.Server
	mcTail  []string

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

		// Inspect the live agent BEFORE closing it: Status() still holds the
		// last server error, which distinguishes an auth rejection from a
		// network drop.
		st := ag.Status()
		rejected := err != nil && !st.Registered && st.LastErr != ""
		ag.Close()

		if rejected {
			a.mu.Lock()
			pending := a.pendingUser
			a.mu.Unlock()
			if pending != "" {
				// The account/password (or register) was refused. Stop retrying
				// and drop back to the logged-out state so the user can fix it.
				a.setNote("登录失败："+st.LastErr, false)
				a.mu.Lock()
				a.pendingUser, a.pendingPass, a.pendingCreate = "", "", false
				a.cfg.Account, a.cfg.Token = "", ""
				a.mu.Unlock()
			} else {
				// A cached session token no longer works (revoked or rotated).
				a.setNote("登录状态已失效，请重新登录", false)
				a.mu.Lock()
				a.cfg.Account, a.cfg.Token = "", ""
				a.mu.Unlock()
			}
			_ = a.cfg.Save(a.cfgPath)
		} else if err != nil {
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
	pass, create := a.pendingPass, a.pendingCreate
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
		// M7 credentials. A fresh login/registration carries the plaintext
		// password (cfg.Token is cleared so the server validates it); after a
		// session token has been cached, later restarts authenticate with it.
		Account:  cfg.Account,
		Password: pass,
		Token:    cfg.Token,
		Create:   create,
		Info:     log.Printf,
		Logf:     log.Printf,
	}
}

// tickLoop repaints the window and the tray tooltip once a second.
func (a *app) tickLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			a.maybePersistToken()
			a.win.SetView(a.view())
			trTooltip(a)
		case <-a.quitCh:
			return
		}
	}
}

// maybePersistToken caches the session token so the next start can log in
// without a password. The server rotates the token on every successful login,
// so even a token-based login produces a fresh token that must be persisted (the
// old cached one is already invalid server-side). The plaintext password is
// never written to disk: once the server has handed out a token, the pending
// password is forgotten.
func (a *app) maybePersistToken() {
	a.mu.Lock()
	ag := a.ag
	acct := a.cfg.Account
	cached := a.cfg.Token
	a.mu.Unlock()
	if ag == nil || acct == "" {
		return
	}
	if ag.Account() != acct || ag.Token() == "" {
		return
	}
	fresh := ag.Token()
	if fresh == cached {
		return
	}
	a.mu.Lock()
	a.cfg.Token = fresh
	a.pendingUser, a.pendingPass, a.pendingCreate = "", "", false
	a.mu.Unlock()
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.setNote("保存登录状态失败："+err.Error(), false)
	}
}

// view snapshots the current agent/config state into the window's renderable
// model. When logged in to an M7 account the listbox mirrors the server-side
// friend directory (row i ↔ directory[i], sorted by username); in legacy mode
// it mirrors cfg.Friends (row i ↔ friend i). The delete button's listbox index
// maps straight back to whichever list view() rendered.
func (a *app) view() window.View {
	a.mu.Lock()
	cfg := *a.cfg
	ag := a.ag
	note, noteGood, noteAt := a.note, a.noteGood, a.noteAt
	a.mu.Unlock()

	v := window.View{Code: a.myCode, Name: cfg.Name, Server: cfg.Server}

	var st agent.Status
	acct := ""
	if ag != nil {
		st = ag.Status()
		// Registered gates "logged in": myAccount is set at construction, so a
		// rejected login would otherwise look logged-in before the server reply.
		if st.Registered && st.Account != "" {
			acct = st.Account
		}
	}

	switch {
	case note != "" && time.Since(noteAt) < 4*time.Second:
		v.Status, v.Good = note, noteGood
	case cfg.Name == "" || cfg.Server == "":
		v.Status = "请先填写昵称和服务器地址，然后点“保存并连接”"
	case ag == nil:
		v.Status = "正在连接服务器…"
	default:
		switch {
		case st.VnicMsg != "":
			v.Status = "虚拟网卡不可用：" + st.VnicMsg
		case st.Registered:
			name := st.Name
			if name == "" && acct != "" {
				name = acct
			}
			v.Status = fmt.Sprintf("已连接 · %s · 虚拟IP %s", name, st.VirtualIP)
			v.Good = true
		default:
			v.Status = "连接中…"
		}
	}

	// M7 account / room / hint lines.
	v.Account = acct
	v.LoggedIn = acct != ""
	if acct != "" {
		v.AcctState = "当前账号：" + acct + " · 已登录"
		v.AddHint = "输入对方的用户名（账号）即可添加"
	} else {
		v.AcctState = "未登录"
		v.AddHint = "未登录：可粘贴好友码连接；登录后可按用户名加好友"
	}
	if ag != nil {
		if room := ag.RoomState(); room != nil {
			v.RoomState = fmt.Sprintf("房间：%s · %d 人", room.Code, len(room.Members))
			v.RoomIn = true
		}
	}

	// M7c game panel: prefilled paths plus the running/joinable state. When a
	// dedicated server runs locally its address is our own virtual IP:25565;
	// otherwise the room host's address is the one to join.
	a.mu.Lock()
	v.Java, v.Jar = a.mcJava, a.mcJar
	gameRunning := a.mcSrv != nil && a.mcSrv.Running()
	a.mu.Unlock()
	v.GameRunning = gameRunning
	if addr := a.joinableAddr(); addr != "" {
		if gameRunning {
			v.GameState = "服务器运行中 · 可加入 " + addr
		} else {
			v.GameState = "未运行 · 可加入 " + addr
		}
	} else if gameRunning {
		v.GameState = "服务器运行中"
	} else {
		v.GameState = "未运行"
	}

	online, conn := peerState(ag)
	if acct != "" {
		for _, f := range ag.FriendDirectory() {
			v.Rows = append(v.Rows, accountFriendRow(f, conn))
		}
	} else {
		for _, f := range cfg.Friends {
			v.Rows = append(v.Rows, friendRow(f, online, conn))
		}
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

// accountFriendRow renders one M7 account-directory friend. Presence comes from
// the server (f.Online); the tunnel state overlays a stronger claim when a P2P
// connection is actually up or in progress.
func accountFriendRow(f protocol.Friend, conn map[string]p2p.State) string {
	switch conn[strings.ToLower(f.Username)] {
	case p2p.StateConnected:
		return f.Username + " — 已连接"
	case p2p.StateConnecting:
		return f.Username + " — 连接中"
	}
	if f.Online {
		return f.Username + " — 在线"
	}
	return f.Username + " — 离线"
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
			name := st.Name
			if name == "" && st.Account != "" {
				name = st.Account
			}
			a.tr.SetTooltip(fmt.Sprintf("Eliauk VPN — %s (%s)", name, st.VirtualIP))
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
		a.addFriend(ev.Text)
	case window.EvDelete:
		a.deleteFriend(ev.Index)
	case window.EvSave:
		a.saveSettings(ev.Text, ev.Text2)
	case window.EvQuit:
		a.quit()
	case window.EvLogin:
		a.doAuth(ev.Text, ev.Text2, false)
	case window.EvRegister:
		a.doAuth(ev.Text, ev.Text2, true)
	case window.EvLogout:
		a.logout()
	case window.EvRoomCreate:
		a.roomAction(func(ag *agent.Agent) error { return ag.CreateRoom() }, "已创建房间", true)
	case window.EvRoomJoin:
		a.roomAction(func(ag *agent.Agent) error { return ag.JoinRoom(ev.Text) }, "已加入房间", true)
	case window.EvRoomLeave:
		a.roomAction(func(ag *agent.Agent) error { return ag.LeaveRoom() }, "已离开房间", true)
	case window.EvGameDetect:
		a.detectGame()
	case window.EvSrvStart:
		a.startGame(ev.Text, ev.Text2)
	case window.EvSrvStop:
		a.stopGame()
	case window.EvSrvCopy:
		a.copyGameAddr()
	case window.EvMCAdd:
		a.addToLauncher()
	case window.EvLaunch:
		a.launchGame()
	}
}

// addFriend adds a peer: by username when logged in to an M7 account (the
// server resolves it and makes the friendship symmetric), or by pasted friend
// fingerprint in legacy mode. Either way the live agent's whitelist updates.
func (a *app) addFriend(input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		a.setNote("请输入对方的用户名", false)
		return
	}
	a.mu.Lock()
	ag := a.ag
	acct := loggedInAccount(ag)
	a.mu.Unlock()
	if acct != "" {
		if err := ag.AddFriendByName(input); err != nil {
			a.setNote(err.Error(), false)
			return
		}
		a.setNote("已添加好友："+input, true)
		return
	}
	// Legacy fingerprint mode (not logged in).
	if _, err := crypto.ParseFingerprint(input); err != nil {
		a.setNote("未登录且输入的不是有效好友码："+err.Error(), false)
		return
	}
	a.mu.Lock()
	added := a.cfg.AddFriend("", input)
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
		_ = ag.AddFriend(input)
	}
	a.setNote("已添加好友", true)
}

// deleteFriend removes the friend at listbox row idx. The row source matches
// view(): the account friend directory when logged in, config friends otherwise.
func (a *app) deleteFriend(idx int) {
	a.mu.Lock()
	ag := a.ag
	acct := loggedInAccount(ag)
	a.mu.Unlock()
	if acct != "" {
		if ag == nil {
			return
		}
		dir := ag.FriendDirectory()
		if idx < 0 || idx >= len(dir) {
			return
		}
		if err := ag.RemoveFriendByName(dir[idx].Username); err != nil {
			a.setNote(err.Error(), false)
			return
		}
		a.setNote("已删除好友", true)
		return
	}
	a.mu.Lock()
	if idx < 0 || idx >= len(a.cfg.Friends) {
		a.mu.Unlock()
		return
	}
	f := a.cfg.Friends[idx]
	a.cfg.RemoveFriend(f.Code)
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

// loggedInAccount returns the authenticated username of ag, or "" when the
// agent is not connected or the account is still pending.
func loggedInAccount(ag *agent.Agent) string {
	if ag == nil {
		return ""
	}
	st := ag.Status()
	if st.Registered && st.Account != "" {
		return st.Account
	}
	return ""
}

// doAuth logs in (create=false) or registers (create=true) an account. It sets
// the pending credentials, clears any stale session token, and rebuilds the
// agent so it re-registers with the server.
func (a *app) doAuth(user, pass string, create bool) {
	user = strings.TrimSpace(user)
	if user == "" || pass == "" {
		a.setNote("账号和密码都不能为空", false)
		return
	}
	a.mu.Lock()
	a.pendingUser, a.pendingPass, a.pendingCreate = user, pass, create
	a.cfg.Account = user
	a.cfg.Token = "" // force a password-based (re)authentication
	a.mu.Unlock()
	a.signalRestart()
	if create {
		a.setNote("正在注册…", true)
	} else {
		a.setNote("正在登录…", true)
	}
}

// logout clears the cached account/token and reconnects anonymously.
func (a *app) logout() {
	a.mu.Lock()
	a.pendingUser, a.pendingPass, a.pendingCreate = "", "", false
	a.cfg.Account, a.cfg.Token = "", ""
	a.mu.Unlock()
	if err := a.cfg.Save(a.cfgPath); err != nil {
		a.setNote("保存配置失败："+err.Error(), false)
		return
	}
	a.signalRestart()
	a.setNote("已退出登录", true)
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
// detected (or configured) Java. Bound so a failed login can't hang the e2e.
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

// detectGame re-runs Java / server.jar detection and fills the edit boxes.
func (a *app) detectGame() {
	java := mc.FindJava()
	jar := mc.FindServerJar()
	a.persistGamePaths(java, jar)
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

// copyGameAddr copies the current joinable address to the clipboard.
func (a *app) copyGameAddr() {
	addr := a.joinableAddr()
	if addr == "" {
		a.setNote("暂无可复制的地址：请先启动服务器，或加入有房主的房间", false)
		return
	}
	window.CopyToClipboard(a.win.Hwnd(), addr)
	a.setNote("已复制 "+addr, true)
}

// addToLauncher registers the current joinable address in the official
// launcher's multiplayer list (servers.dat), so joining is one click.
func (a *app) addToLauncher() {
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
	if _, err := mc.AddServerToLauncher(name, addr); err != nil {
		a.setNote(err.Error(), false)
		return
	}
	a.setNote("已添加服务器 "+name+" → "+addr+"，打开启动器即可加入", true)
}

// launchGame starts the official Minecraft launcher.
func (a *app) launchGame() {
	if err := mc.LaunchLauncher(); err != nil {
		a.setNote(err.Error(), false)
		return
	}
	a.setNote("已启动 Minecraft 启动器", true)
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
