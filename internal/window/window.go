//go:build windows

// Package window provides the Eliauk main window: a plain Win32 window with
// native controls (STATIC/EDIT/BUTTON/LISTBOX/group boxes) rendered in the
// system font, plus a notification-area icon attached to the same window.
// Like internal/tray it calls user32/shell32/gdi32 directly through
// syscall.NewLazyDLL — no third-party GUI dependency.
//
// Ownership: Window.Run() owns the UI-thread message pump. The application
// drives it from any goroutine via SetView (marshaled with a PostMessage) and
// reads user actions from Events(). The window itself only renders what the
// application hands it; all logic (agent, config, friends) lives in cmd/gui.
package window

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"eliaukvpn/internal/tray"
)

// ---- window style ----
const (
	// Client-area size in pixels (fixed, non-resizable). The outer window is
	// sized with AdjustWindowRectEx so the client is exactly this big on any
	// theme, regardless of caption/border metrics. Cards run to x=512, y=790.
	winW = 520
	winH = 796
	// WS_OVERLAPPEDWINDOW (0x00CF0000) minus WS_THICKFRAME/WS_MAXIMIZEBOX.
	// WS_VISIBLE is included so the window is shown regardless of the process
	// STARTUPINFO show state: the FIRST ShowWindow call would otherwise be
	// overridden by the launcher, and a hidden/minimized startup state would
	// slip through (the SC_MINIMIZE handler hides the window).
	winStyle = 0x00CA0000 | wsVisible
)

// ---- Win32 constants used below ----
const (
	wmApp = 0x8000

	// wmRefresh is wmApp+3, NOT wmApp+1: internal/tray owns wmApp+1 as its
	// Shell_NotifyIcon callback (CallbackMsg), which also arrives at this
	// window. The host's MsgHook intercepts 0x8001 before wndProc, so the
	// window's own refresh must live at a distinct value or the hook would
	// swallow it.
	wmRefresh = wmApp + 3 // SetView marshals a View to the UI thread
	wmQuitAsk = wmApp + 2 // Stop() quits the pump

	wmCommand        = 0x0111
	wmSysCommand     = 0x0112
	wmCtlColorStatic = 0x0138
	wmSetFont        = 0x0030
	wmClose          = 0x0010
	wmDestroy        = 0x0002

	scMinimize = 0xF020

	wsChild   = 0x40000000
	wsVisible = 0x10000000
	wsTabStop = 0x00010000
	wsBorder  = 0x00800000
	wsVScroll = 0x00200000

	esAutoHScroll = 0x0080
	esReadOnly    = 0x0800
	esPassword    = 0x0020 // EDIT hides the typed characters
	bsGroupBox    = 0x0007
	bsPushButton  = 0x0000
	lbsNotify     = 0x0001

	lbAddString    = 0x0180
	lbResetContent = 0x0184
	lbGetCursel    = 0x0187

	csDblClks      = 0x0008
	imageIcon      = 0x0001
	lrLoadFromFile = 0x0010
	defaultGuiFont = 17
	colorBtnFace   = 15
	swShow         = 5
	swHide         = 0
	cfgUnicodeText = 13
	globalMoveZero = 0x0042
	tokenQuery     = 0x0008
	tokenElevation = 20
	colorGreen     = 0x00008000 // COLORREF 0x00BBGGRR
	colorRed       = 0x000000B0
)

// Control IDs.
const (
	idcStatus       = 100
	idcCode         = 101 // my friend code (read-only)
	idcCopy         = 102
	idcName2        = 103 // (legacy nickname, unused in M7)
	idcCode2        = 104 // add-friend input: username (account) or fingerprint (legacy)
	idcAdd          = 105
	idcList         = 106
	idcDelete       = 107
	idcName         = 108
	idcServer       = 109
	idcSave         = 110
	idcQuit         = 111
	// M7 account + room controls.
	idcAccount   = 112 // username
	idcPass      = 113 // password (masked)
	idcLogin     = 114
	idcRegister  = 115
	idcLogout    = 116
	idcAcctState = 117 // "当前账号：host · 已登录" / "未登录"
	idcAddHint   = 118 // add-friend hint (changes with login state)
	idcRoomCreate = 119
	idcRoomCode   = 120 // room code input
	idcRoomJoin   = 121
	idcRoomLeave  = 122
	idcRoomState  = 123 // "房间：ABC · 2 人" / "未加入房间"
	// M7c game panel controls.
	idcJavaEdit   = 124 // java.exe path
	idcJarEdit    = 125 // server.jar path
	idcGameDetect = 126 // 自动检测
	idcSrvCopy    = 127 // 复制地址 (copy the joinable address)
	idcGameStart  = 128 // 启动服务器
	idcGameStop   = 129 // 停止服务器
	idcMCAdd      = 130 // 添加到启动器 (servers.dat)
	idcLaunch     = 131 // 启动游戏 (official launcher)
	idcGameState  = 132 // game status line
	// UI rework: header status dot (owner-draw static) + app-name title.
	idcStatusDot = 133
	idcTitle     = 134
)

// EvType is a user action emitted by the window.
type EvType int

const (
	// EvAdd: user clicked 添加 — Text is the input: an account username (M7) or a
	// pasted friend fingerprint (legacy). Text2 is unused in M7.
	EvAdd EvType = iota
	// EvCopy: user clicked 复制 (copy my friend code to the clipboard).
	EvCopy
	// EvDelete: user clicked 删除选中好友 — Index is the selected list row.
	EvDelete
	// EvSave: user clicked 保存并连接 — Text is the name, Text2 the server.
	EvSave
	// EvQuit: user clicked 退出.
	EvQuit
	// EvLogin: user clicked 登录 — Text is the username, Text2 the password.
	EvLogin
	// EvRegister: user clicked 注册 — Text is the username, Text2 the password.
	EvRegister
	// EvLogout: user clicked 退出登录.
	EvLogout
	// EvRoomCreate / EvRoomJoin (Text = room code) / EvRoomLeave: room actions.
	EvRoomCreate
	EvRoomJoin
	EvRoomLeave
	// M7c game panel actions.
	// EvGameDetect: user clicked 自动检测 — re-run Java/jar detection.
	// EvSrvStart: user clicked 启动服务器 — Text is the java path, Text2 the jar.
	// EvSrvStop / EvSrvCopy (copy the joinable address) / EvMCAdd (register the
	// server in the launcher's multiplayer list) / EvLaunch (start the launcher).
	EvGameDetect
	EvSrvStart
	EvSrvStop
	EvSrvCopy
	EvMCAdd
	EvLaunch
)

// EvMsg is one user action with its payload.
type EvMsg struct {
	Type  EvType
	Text  string
	Text2 string
	Index int
}

// View is the full renderable state of the window. The application rebuilds
// it from live agent/config state and hands it to SetView.
type View struct {
	Status string // one-line status ("● 已连接 · …" / "连接中…" / error)
	Good   bool   // status shown green (good) or red (warning)
	Code   string // my friend code (base64 fingerprint)
	Rows   []string

	// M7 account / room / hint lines.
	Account     string // logged-in username ("" = not logged in)
	AcctState   string // "当前账号：host · 已登录" / "未登录"
	AddHint     string // add-friend hint (username vs fingerprint)
	RoomState   string // "房间：ABC · 2 人" / "未加入房间"
	RoomIn      bool   // whether the room controls show the "join" or "leave" state
	LoggedIn    bool   // account controls show the "logged in" state

	Name   string // settings: display name
	Server string // settings: server address

	// M7c game panel: remembered/auto-detected paths and the running state.
	Java        string // java.exe path (prefilled, editable)
	Jar         string // server.jar path (prefilled, editable)
	GameState   string // "服务器：运行中 · 可加入 10.8.0.1:25565" / "未运行" ...
	GameRunning bool   // start vs stop button shows depending on this
}

// MsgHook lets the host intercept messages before the window's own handling.
// Return true to consume the message. Used to forward the tray callback.
type MsgHook func(m uint32, wParam, lParam uintptr) bool

// Window is the main window.
type Window struct {
	hwnd uintptr
	inst uintptr
	cb   uintptr // WndProc callback, kept alive
	icon uintptr // taskbar HICON
	font uintptr // body HFONT (Segoe UI)
	sf   uintptr // status HFONT

	// Theme font set (created in loadFonts).
	fTitle, fSection, fBody, fLabel, fSmall uintptr
	// Theme brushes (created in loadThemeBrushes).
	brBg, brCard, brInput uintptr

	// child controls
	statusTxt, titleTxt, statusDotHwnd                              uintptr
	codeEdit, code2Edit, listBox                                    uintptr
	copyBtn, addBtn, deleteBtn, nameEdit, serverEdit, saveBtn, quitBtn uintptr
	acctEdit, passEdit, loginBtn, registerBtn, logoutBtn, acctStateTxt uintptr
	addHintTxt, roomCreateBtn, roomCodeEdit, roomJoinBtn, roomLeaveBtn uintptr
	roomStateTxt                                                      uintptr
	// M7c game panel controls.
	javaEdit, jarEdit, gameDetectBtn, srvCopyBtn                     uintptr
	gameStartBtn, gameStopBtn, mcAddBtn, launchBtn, gameStateTxt     uintptr

	// Owner-draw plumbing.
	buttons      []uintptr        // created in createControls, subclassed after
	btnCallbacks []uintptr        // kept alive so hover/press procs survive
	statusDot    int8             // 0=connecting/neutral, 1=good, 2=bad
	staticColors map[uintptr]uint32 // per-static text color (WM_CTLCOLORSTATIC)
	staticFonts  map[uintptr]uintptr // per-static font

	mu         sync.Mutex
	pending    View
	statusGood bool
	hook       MsgHook
	evCh       chan EvMsg
	doneCh     chan struct{}
	once       sync.Once
}

// New constructs an unstarted window. Call Run on the UI thread.
func New() (*Window, error) {
	return &Window{
		evCh:         make(chan EvMsg, 16),
		doneCh:       make(chan struct{}),
		staticColors: make(map[uintptr]uint32),
		staticFonts:  make(map[uintptr]uintptr),
	}, nil
}

// Events returns the user-action channel.
func (w *Window) Events() <-chan EvMsg { return w.evCh }

// SetView renders the given view on the UI thread. Safe from any goroutine.
func (w *Window) SetView(v View) {
	w.mu.Lock()
	w.pending = v
	w.mu.Unlock()
	procPostMessageW.Call(w.hwnd, wmRefresh, 0, 0)
}

// SetMsgHook installs a pre-dispatch message hook (see MsgHook).
func (w *Window) SetMsgHook(h MsgHook) {
	w.mu.Lock()
	w.hook = h
	w.mu.Unlock()
}

// Hwnd returns the window handle, valid after Run starts.
func (w *Window) Hwnd() uintptr { return w.hwnd }

// Show makes the window visible (tray "open window" / double-click).
func (w *Window) Show() {
	if w.hwnd != 0 {
		procShowWindow.Call(w.hwnd, swShow)
		procSetForegroundWindow.Call(w.hwnd)
	}
}

// Hide hides the window to the tray.
func (w *Window) Hide() {
	if w.hwnd != 0 {
		procShowWindow.Call(w.hwnd, swHide)
	}
}

// Stop asks the UI thread to quit; Run returns. Safe from any goroutine.
func (w *Window) Stop() {
	if w.hwnd != 0 {
		procPostMessageW.Call(w.hwnd, wmQuitAsk, 0, 0)
	}
}

// Done is closed when Run has exited.
func (w *Window) Done() <-chan struct{} { return w.doneCh }

// Run creates the window and pumps messages until Stop is called. It must run
// on the goroutine that owns the UI thread; it blocks.
func (w *Window) Run() error {
	// System-DPI-aware so the fixed 520x786 layout renders crisp (no blurry
	// bitmap scaling) on scaled displays. Must precede any window creation.
	procSetProcessDPIAware.Call()

	w.cb = syscall.NewCallback(w.wndProc)
	w.loadFonts()
	w.loadThemeBrushes()
	if err := w.loadWindowIcon(); err != nil {
		_ = err // non-fatal: window works without a custom taskbar icon
	}

	className, err := syscall.UTF16PtrFromString("EliaukMainClass")
	if err != nil {
		return err
	}
	w.inst, _, _ = procGetModuleHandleW.Call(0)

	wc := &wndClass{
		style:       csDblClks,
		lpfnWndProc: w.cb,
		hInstance:   w.inst,
		hIcon:       w.icon,
		hbrBgnd:     w.brBg, // dark class background so every child erases dark
		lpszClass:   uintptr(unsafe.Pointer(className)),
	}
	if r, _, e := procRegisterClassW.Call(uintptr(unsafe.Pointer(wc))); r == 0 {
		if e != syscall.Errno(1410) { // ERROR_CLASS_ALREADY_EXISTS: fine
			return fmt.Errorf("RegisterClassW: %v", e)
		}
	}

	// Size the outer window so the client area is exactly winW×winH on this
	// theme's caption/border metrics (fixed dialog-style window, no resize).
	var wr rect
	wr.right, wr.bottom = winW, winH
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&wr)), uintptr(winStyle), 0, 0)
	outerW, outerH := wr.right-wr.left, wr.bottom-wr.top

	title, _ := syscall.UTF16PtrFromString("Eliauk VPN")
	hwnd, _, e2 := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		uintptr(winStyle), 0x80000000, 0x80000000, uintptr(outerW), uintptr(outerH),
		0, 0, w.inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW: %v", e2)
	}
	w.hwnd = hwnd
	applyImmersiveDark(hwnd) // dark caption bar
	w.createControls()
	w.applyFonts()
	w.mu.Lock()
	v := w.pending
	w.mu.Unlock()
	w.applyView(v)

	procShowWindow.Call(hwnd, swShow)
	procUpdateWindow.Call(hwnd)

	for {
		var m msg
		got, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if got == 0 { // WM_QUIT
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	if w.icon != 0 {
		procDestroyIcon.Call(w.icon)
	}
	procDestroyWindow.Call(hwnd)
	w.once.Do(func() { close(w.doneCh) })
	return nil
}

// wndProc dispatches messages for the main window.
func (w *Window) wndProc(hwnd uintptr, m uint32, wParam, lParam uintptr) uintptr {
	w.mu.Lock()
	hook := w.hook
	w.mu.Unlock()
	if hook != nil && hook(m, wParam, lParam) {
		return 0
	}
	switch m {
	case wmRefresh:
		w.mu.Lock()
		v := w.pending
		w.mu.Unlock()
		w.applyView(v)
		return 0
	case wmQuitAsk:
		procPostQuitMessage.Call(0)
		return 0
	case wmCommand:
		return w.onCommand(wParam)
	case wmEraseBkgnd, wmPaint, wmDrawItem, wmCtlColorStatic, wmCtlColorEdit, wmCtlColorListbox:
		r, _ := w.themeMessage(m, wParam, lParam)
		return r
	case wmSysCommand:
		if wParam&0xFFF0 == scMinimize {
			procShowWindow.Call(hwnd, swHide) // minimize hides to the tray
			return 0
		}
	case wmClose:
		procShowWindow.Call(hwnd, swHide) // close hides to the tray
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(m), wParam, lParam)
	return r
}

// onCommand handles button clicks and listbox notifications.
func (w *Window) onCommand(wParam uintptr) uintptr {
	switch id := uint32(wParam) & 0xFFFF; id {
	case idcCopy:
		w.emit(EvMsg{Type: EvCopy})
	case idcAdd:
		w.emit(EvMsg{Type: EvAdd, Text: w.getText(w.code2Edit)})
	case idcDelete:
		idx, _, _ := procSendMessageW.Call(w.listBox, lbGetCursel, 0, 0)
		if int32(idx) >= 0 {
			w.emit(EvMsg{Type: EvDelete, Index: int(idx)})
		}
	case idcSave:
		w.emit(EvMsg{Type: EvSave, Text: w.getText(w.nameEdit), Text2: w.getText(w.serverEdit)})
	case idcQuit:
		w.emit(EvMsg{Type: EvQuit})
	case idcLogin:
		w.emit(EvMsg{Type: EvLogin, Text: w.getText(w.acctEdit), Text2: w.getText(w.passEdit)})
		setText(w.passEdit, "") // don't leave the password sitting in the control
	case idcRegister:
		w.emit(EvMsg{Type: EvRegister, Text: w.getText(w.acctEdit), Text2: w.getText(w.passEdit)})
		setText(w.passEdit, "")
	case idcLogout:
		w.emit(EvMsg{Type: EvLogout})
	case idcRoomCreate:
		w.emit(EvMsg{Type: EvRoomCreate})
	case idcRoomJoin:
		w.emit(EvMsg{Type: EvRoomJoin, Text: w.getText(w.roomCodeEdit)})
		setText(w.roomCodeEdit, "")
	case idcRoomLeave:
		w.emit(EvMsg{Type: EvRoomLeave})
	case idcGameDetect:
		w.emit(EvMsg{Type: EvGameDetect})
	case idcGameStart:
		w.emit(EvMsg{Type: EvSrvStart, Text: w.getText(w.javaEdit), Text2: w.getText(w.jarEdit)})
	case idcGameStop:
		w.emit(EvMsg{Type: EvSrvStop})
	case idcSrvCopy:
		w.emit(EvMsg{Type: EvSrvCopy})
	case idcMCAdd:
		w.emit(EvMsg{Type: EvMCAdd})
	case idcLaunch:
		w.emit(EvMsg{Type: EvLaunch})
	}
	return 0
}

func (w *Window) emit(ev EvMsg) {
	select {
	case w.evCh <- ev:
	default:
	}
}

// applyView writes a View to the controls. UI thread only.
func (w *Window) applyView(v View) {
	setText(w.statusTxt, v.Status)
	setText(w.codeEdit, v.Code)
	setText(w.nameEdit, v.Name)
	setText(w.serverEdit, v.Server)

	// M7 account/room state lines and hints.
	setText(w.acctStateTxt, v.AcctState)
	setText(w.addHintTxt, v.AddHint)
	setText(w.roomStateTxt, v.RoomState)

	// The account/password edits are only meaningful before login; after that
	// they collapse into a read-only "已登录" line plus the logout button.
	setVisible(w.acctEdit, !v.LoggedIn)
	setVisible(w.passEdit, !v.LoggedIn)
	setVisible(w.loginBtn, !v.LoggedIn)
	setVisible(w.registerBtn, !v.LoggedIn)
	setVisible(w.logoutBtn, v.LoggedIn)

	// Room controls: "创建/加入" before joining, "离开" after.
	setVisible(w.roomCreateBtn, !v.RoomIn)
	setVisible(w.roomCodeEdit, !v.RoomIn)
	setVisible(w.roomJoinBtn, !v.RoomIn)
	setVisible(w.roomLeaveBtn, v.RoomIn)

	// M7c game panel: prefilled paths and the running state.
	setText(w.javaEdit, v.Java)
	setText(w.jarEdit, v.Jar)
	setText(w.gameStateTxt, v.GameState)
	setVisible(w.gameStartBtn, !v.GameRunning)
	setVisible(w.gameStopBtn, v.GameRunning)

	procSendMessageW.Call(w.listBox, lbResetContent, 0, 0)
	for _, row := range v.Rows {
		p, _ := syscall.UTF16PtrFromString(row)
		procSendMessageW.Call(w.listBox, lbAddString, 0, uintptr(unsafe.Pointer(p)))
	}
	w.mu.Lock()
	changed := w.statusGood != v.Good
	w.statusGood = v.Good
	w.mu.Unlock()

	// Status dot: good=green, an in-flight "连接" attempt=neutral, else red.
	dot := int8(2)
	if v.Good {
		dot = 1
	} else if strings.Contains(v.Status, "连接") {
		dot = 0
	}
	w.setStatusDot(dot)
	if changed {
		procInvalidateRect.Call(w.statusTxt, 0, 1)
	}
}

func setVisible(h uintptr, visible bool) {
	if h == 0 {
		return
	}
	cmd := uintptr(swShow)
	if !visible {
		cmd = uintptr(swHide)
	}
	procShowWindow.Call(h, cmd)
}

func setText(h uintptr, s string) {
	p, _ := syscall.UTF16PtrFromString(s)
	procSetWindowTextW.Call(h, uintptr(unsafe.Pointer(p)))
}

func (w *Window) getText(h uintptr) string {
	if h == 0 {
		return ""
	}
	var buf [1024]uint16
	n, _, _ := procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// ---- controls ----

// create makes a child control with the standard child style and returns its
// HWND. cls is the built-in class ("STATIC"/"EDIT"/"BUTTON"/"LISTBOX").
func (w *Window) create(cls, text string, style uintptr, x, y, wd, ht int32, id uintptr) uintptr {
	cp, _ := syscall.UTF16PtrFromString(cls)
	tp, _ := syscall.UTF16PtrFromString(text)
	h, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(cp)), uintptr(unsafe.Pointer(tp)),
		uintptr(wsChild|wsVisible|style),
		uintptr(x), uintptr(y), uintptr(wd), uintptr(ht),
		w.hwnd, id, w.inst, 0)
	return h
}

// label creates a themed STATIC: colored text with a per-static font, both
// served by WM_CTLCOLORSTATIC. Returns the HWND.
func (w *Window) label(text string, x, y, wd, ht int32, color uint32, font uintptr) uintptr {
	h := w.create("STATIC", text, 0, x, y, wd, ht, 0)
	w.staticColors[h] = color
	w.staticFonts[h] = font
	return h
}

// button creates an owner-draw push button. Buttons are subclassed for
// hover/press feedback at the end of createControls.
func (w *Window) button(text string, x, y, wd, ht int32, id uintptr) uintptr {
	h := w.create("BUTTON", text, bsPushButton|bsOwnerDraw|wsTabStop, x, y, wd, ht, id)
	w.buttons = append(w.buttons, h)
	return h
}

func (w *Window) createControls() {
	// Header strip (on the window background): app name, status dot, status line.
	w.titleTxt = w.label("Eliauk VPN", 16, 7, 150, 26, colText, w.fTitle)
	w.statusDotHwnd = w.create("STATIC", "", ssOwnerDraw, 176, 15, 14, 14, idcStatusDot)
	w.statusTxt = w.create("STATIC", "", 0, 198, 10, 306, 22, idcStatus)

	// 账号 (card 44..136; title band 58..76, content 76..134)
	w.label("账号", 24, 82, 40, 18, colMuted, w.fLabel)
	w.acctEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 70, 78, 130, 24, idcAccount)
	w.label("密码", 208, 82, 40, 18, colMuted, w.fLabel)
	w.passEdit = w.create("EDIT", "", wsBorder|esPassword|wsTabStop, 252, 78, 120, 24, idcPass)
	w.loginBtn = w.button("登录", 380, 76, 54, 28, idcLogin)
	w.registerBtn = w.button("注册", 440, 76, 60, 28, idcRegister)
	w.acctStateTxt = w.label("未登录", 24, 108, 340, 18, colMuted, w.fSmall)
	w.logoutBtn = w.button("退出登录", 380, 106, 120, 28, idcLogout)

	// 好友 (card 142..258; title band 156..174, content 172..256)
	w.label("我的好友码", 24, 178, 76, 18, colMuted, w.fLabel)
	w.codeEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|esReadOnly|wsTabStop, 104, 174, 290, 24, idcCode)
	w.copyBtn = w.button("复制", 400, 172, 96, 28, idcCopy)
	w.label("添加好友", 24, 208, 76, 18, colMuted, w.fLabel)
	w.code2Edit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 104, 204, 290, 24, idcCode2)
	w.addBtn = w.button("添加", 400, 202, 96, 28, idcAdd)
	w.addHintTxt = w.label("输入对方的用户名（账号）即可添加", 24, 238, 472, 18, colMuted, w.fSmall)

	// 房间 (card 264..352; title band 278..296, content 296..348)
	w.roomCreateBtn = w.button("创建房间", 24, 296, 92, 28, idcRoomCreate)
	w.label("房间码", 124, 302, 46, 18, colMuted, w.fLabel)
	w.roomCodeEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 172, 296, 100, 24, idcRoomCode)
	w.roomJoinBtn = w.button("加入房间", 280, 296, 92, 28, idcRoomJoin)
	w.roomLeaveBtn = w.button("离开房间", 280, 296, 92, 28, idcRoomLeave)
	w.roomStateTxt = w.label("未加入房间", 24, 330, 472, 18, colMuted, w.fSmall)

	// 游戏 (card 358..506; title band 372..390, content 388..500)
	w.label("Java 路径", 24, 394, 66, 18, colMuted, w.fLabel)
	w.javaEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 94, 390, 300, 24, idcJavaEdit)
	w.gameDetectBtn = w.button("自动检测", 400, 388, 96, 28, idcGameDetect)
	w.label("服务器 jar", 24, 422, 66, 18, colMuted, w.fLabel)
	w.jarEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 94, 418, 300, 24, idcJarEdit)
	w.srvCopyBtn = w.button("复制地址", 400, 416, 96, 28, idcSrvCopy)
	w.gameStartBtn = w.button("启动服务器", 24, 446, 104, 28, idcGameStart)
	w.gameStopBtn = w.button("停止服务器", 24, 446, 104, 28, idcGameStop)
	w.mcAddBtn = w.button("添加服务器", 136, 446, 104, 28, idcMCAdd)
	w.launchBtn = w.button("启动游戏", 248, 446, 104, 28, idcLaunch)
	w.gameStateTxt = w.label("未运行", 24, 482, 472, 18, colMuted, w.fSmall)

	// 连接状态 (card 512..668; title band 526..544, content 546..666)
	w.listBox = w.create("LISTBOX", "", wsBorder|wsVScroll|lbsNotify, 24, 546, 472, 88, idcList)
	w.deleteBtn = w.button("删除选中好友", 24, 638, 120, 28, idcDelete)

	// 设置 (card 674..790; title band 688..706, content 706..782)
	w.label("昵称", 24, 710, 36, 18, colMuted, w.fLabel)
	w.nameEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 64, 706, 130, 24, idcName)
	w.label("服务器", 204, 710, 44, 18, colMuted, w.fLabel)
	w.serverEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 252, 706, 244, 24, idcServer)
	w.label("首次使用：填写昵称与服务器地址后点保存。服务器地址形如 ws://主机:9090/ws", 24, 736, 472, 18, colMuted, w.fSmall)
	w.saveBtn = w.button("保存并连接", 24, 754, 120, 28, idcSave)
	w.quitBtn = w.button("退出", 392, 754, 88, 28, idcQuit)

	// Native controls: dark internal theme (borders, listbox selection).
	applyDarkThemeControl(w.acctEdit)
	applyDarkThemeControl(w.passEdit)
	applyDarkThemeControl(w.codeEdit)
	applyDarkThemeControl(w.code2Edit)
	applyDarkThemeControl(w.roomCodeEdit)
	applyDarkThemeControl(w.javaEdit)
	applyDarkThemeControl(w.jarEdit)
	applyDarkThemeControl(w.nameEdit)
	applyDarkThemeControl(w.serverEdit)
	applyDarkThemeControl(w.listBox)

	// Owner-draw buttons: subclass to track hover/press.
	for _, b := range w.buttons {
		w.subclassButton(b)
	}
}

// loadFonts builds the theme font set in Segoe UI. w.font stays the body face
// for any legacy callers.
func (w *Window) loadFonts() {
	w.fTitle = makeFont(20, 700)
	w.fSection = makeFont(15, 600)
	w.fBody = makeFont(14, 400)
	w.fLabel = makeFont(13, 400)
	w.fSmall = makeFont(12, 400)
	w.sf = makeFont(14, 600) // status line
	w.font = w.fBody
}

func (w *Window) applyFonts() {
	for h, f := range w.staticFonts {
		procSendMessageW.Call(h, wmSetFont, f, 1)
	}
	procSendMessageW.Call(w.statusTxt, wmSetFont, w.sf, 1)
	for _, h := range []uintptr{
		w.codeEdit, w.code2Edit, w.nameEdit, w.serverEdit,
		w.acctEdit, w.passEdit, w.roomCodeEdit, w.javaEdit, w.jarEdit,
	} {
		procSendMessageW.Call(h, wmSetFont, w.fBody, 1)
	}
	for _, b := range w.buttons {
		procSendMessageW.Call(b, wmSetFont, w.fBody, 1)
	}
	procSendMessageW.Call(w.listBox, wmSetFont, w.fBody, 1)
}

// loadWindowIcon loads the runtime-drawn ICO as the taskbar icon and assigns
// it to the window class.
func (w *Window) loadWindowIcon() error {
	ico := tray.IconICO()
	f, err := os.CreateTemp("", "eliauk-win-*.ico")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(ico); err != nil {
		f.Close()
		return err
	}
	f.Close()

	path, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(path)), imageIcon, 32, 32, lrLoadFromFile)
	if h == 0 {
		return fmt.Errorf("LoadImageW failed for %s", name)
	}
	w.icon = h
	return nil
}

// ---- clipboard ----

// CopyToClipboard puts text on the clipboard as UTF-16. Ownership of the
// allocated handle transfers to the clipboard (do not free it).
func CopyToClipboard(hwnd uintptr, text string) {
	if text == "" {
		return
	}
	u, err := syscall.UTF16FromString(text)
	if err != nil {
		return
	}
	h, _, _ := procGlobalAlloc.Call(globalMoveZero, uintptr(len(u)*2))
	if h == 0 {
		return
	}
	p, _, _ := procGlobalLock.Call(h)
	if p != 0 {
		// GlobalLock returns a uintptr that holds a pointer into the global
		// heap — stable, non-GC memory. vet's unsafeptr analyzer rejects a
		// direct uintptr→unsafe.Pointer conversion, so launder the bits
		// through memory (the standard Go idiom for exactly this case).
		ptr := *(*unsafe.Pointer)(unsafe.Pointer(&p))
		dst := unsafe.Slice((*byte)(ptr), len(u)*2)
		for i, v := range u {
			dst[i*2] = byte(v)
			dst[i*2+1] = byte(v >> 8)
		}
		procGlobalUnlock.Call(h)
	}
	procOpenClipboard.Call(hwnd)
	procEmptyClipboard.Call()
	procSetClipboardData.Call(cfgUnicodeText, h)
	procCloseClipboard.Call()
}

// ---- elevation ----

// IsElevated reports whether the current process runs with an admin token
// (needed to create the Wintun adapter and install routes).
func IsElevated() bool {
	var token uintptr
	cur, _, _ := procGetCurrentProcess.Call()
	r, _, _ := procOpenProcessToken.Call(cur, tokenQuery, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return false
	}
	defer procCloseHandle.Call(token)
	var elev uint32
	var ret uint32
	procGetTokenInformation.Call(token, tokenElevation, uintptr(unsafe.Pointer(&elev)), 4, uintptr(unsafe.Pointer(&ret)))
	return elev != 0
}

// RelaunchElevated re-executes the current process (same command line) with a
// UAC elevation prompt. It returns nil on success — the caller should exit, as
// the elevated copy takes over. It returns an error if the user declined or
// elevation is unavailable; the caller then continues non-elevated.
func RelaunchElevated() error {
	var buf [1024]uint16
	n, _, _ := procGetModuleFileNameW.Call(0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return fmt.Errorf("GetModuleFileNameW")
	}
	exe := syscall.UTF16ToString(buf[:n])

	p, _, _ := procGetCommandLineW.Call()
	if p == 0 {
		return fmt.Errorf("GetCommandLineW")
	}
	// Same laundering as CopyToClipboard: GetCommandLineW returns a stable,
	// Windows-owned buffer pointer carried in a uintptr.
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	words := unsafe.Slice((*uint16)(ptr), 1<<16) // command lines are ≤ 32767 chars
	l := 0
	for words[l] != 0 {
		l++
	}
	cmdline := syscall.UTF16ToString(words[:l])

	runas, _ := syscall.UTF16PtrFromString("runas")
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cmdPtr, _ := syscall.UTF16PtrFromString(cmdline)
	r, _, e := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(runas)), uintptr(unsafe.Pointer(exePtr)), uintptr(unsafe.Pointer(cmdPtr)), 0, swShow)
	if r <= 32 {
		return fmt.Errorf("ShellExecuteW(runas): %v", e)
	}
	return nil
}
