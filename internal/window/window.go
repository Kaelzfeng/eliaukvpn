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
	"sync"
	"syscall"
	"unsafe"

	"eliaukvpn/internal/tray"
)

// ---- window style ----
const (
	// Client-area size in pixels (fixed, non-resizable).
	winW = 520
	winH = 700
	// WS_OVERLAPPEDWINDOW (0x00CF0000) minus WS_THICKFRAME/WS_MAXIMIZEBOX.
	winStyle = 0x00CA0000
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
	font uintptr // HFONT (stock)
	sf   uintptr // bold status font

	// child controls
	statusTxt, codeEdit, code2Edit, listBox                                   uintptr
	copyBtn, addBtn, deleteBtn, nameEdit, serverEdit, saveBtn, quitBtn         uintptr
	acctEdit, passEdit, loginBtn, registerBtn, logoutBtn, acctStateTxt         uintptr
	addHintTxt, roomCreateBtn, roomCodeEdit, roomJoinBtn, roomLeaveBtn         uintptr
	roomStateTxt                                                               uintptr

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
		evCh:   make(chan EvMsg, 16),
		doneCh: make(chan struct{}),
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
	w.cb = syscall.NewCallback(w.wndProc)
	w.loadFonts()
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
		lpszClass:   uintptr(unsafe.Pointer(className)),
	}
	if r, _, e := procRegisterClassW.Call(uintptr(unsafe.Pointer(wc))); r == 0 {
		if e != syscall.Errno(1410) { // ERROR_CLASS_ALREADY_EXISTS: fine
			return fmt.Errorf("RegisterClassW: %v", e)
		}
	}

	title, _ := syscall.UTF16PtrFromString("Eliauk VPN")
	hwnd, _, e2 := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		uintptr(winStyle), 0x80000000, 0x80000000, winW, winH,
		0, 0, w.inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW: %v", e2)
	}
	w.hwnd = hwnd

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
	case wmCtlColorStatic:
		if uintptr(wParam) == w.statusTxt {
			w.mu.Lock()
			good := w.statusGood
			w.mu.Unlock()
			color := uintptr(colorGreen)
			if !good {
				color = uintptr(colorRed)
			}
			procSetTextColor.Call(lParam, color)
			procSetBkMode.Call(lParam, 1) // TRANSPARENT
			brush, _, _ := procGetSysColorBrush.Call(colorBtnFace)
			return brush
		}
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

	procSendMessageW.Call(w.listBox, lbResetContent, 0, 0)
	for _, row := range v.Rows {
		p, _ := syscall.UTF16PtrFromString(row)
		procSendMessageW.Call(w.listBox, lbAddString, 0, uintptr(unsafe.Pointer(p)))
	}
	w.mu.Lock()
	changed := w.statusGood != v.Good
	w.statusGood = v.Good
	w.mu.Unlock()
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

func (w *Window) createControls() {
	w.statusTxt = w.create("STATIC", "Eliauk VPN", 0, 12, 10, 496, 24, idcStatus)

	w.create("BUTTON", "账号", bsGroupBox, 8, 36, 504, 82, 0)
	w.create("STATIC", "账号", 0, 16, 52, 40, 20, 0)
	w.acctEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 58, 50, 140, 22, idcAccount)
	w.create("STATIC", "密码", 0, 206, 52, 40, 20, 0)
	w.passEdit = w.create("EDIT", "", wsBorder|esPassword|wsTabStop, 248, 50, 128, 22, idcPass)
	w.loginBtn = w.create("BUTTON", "登录", bsPushButton|wsTabStop, 384, 48, 56, 26, idcLogin)
	w.registerBtn = w.create("BUTTON", "注册", bsPushButton|wsTabStop, 444, 48, 60, 26, idcRegister)
	w.acctStateTxt = w.create("STATIC", "未登录", 0, 16, 86, 330, 18, idcAcctState)
	w.logoutBtn = w.create("BUTTON", "退出登录", bsPushButton|wsTabStop, 384, 84, 120, 24, idcLogout)

	w.create("BUTTON", "好友", bsGroupBox, 8, 122, 504, 100, 0)
	w.create("STATIC", "我的好友码", 0, 16, 140, 70, 18, 0)
	w.codeEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|esReadOnly|wsTabStop, 88, 138, 320, 22, idcCode)
	w.copyBtn = w.create("BUTTON", "复制", bsPushButton|wsTabStop, 416, 136, 88, 26, idcCopy)
	w.create("STATIC", "添加好友", 0, 16, 174, 70, 18, 0)
	w.code2Edit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 88, 172, 320, 22, idcCode2)
	w.addBtn = w.create("BUTTON", "添加", bsPushButton|wsTabStop, 416, 170, 88, 26, idcAdd)
	w.addHintTxt = w.create("STATIC", "输入对方的用户名（账号）即可添加", 0, 16, 200, 488, 18, idcAddHint)

	w.create("BUTTON", "房间", bsGroupBox, 8, 226, 504, 78, 0)
	w.roomCreateBtn = w.create("BUTTON", "创建房间", bsPushButton|wsTabStop, 16, 246, 92, 26, idcRoomCreate)
	w.create("STATIC", "房间码", 0, 116, 250, 46, 18, 0)
	w.roomCodeEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 164, 244, 110, 22, idcRoomCode)
	w.roomJoinBtn = w.create("BUTTON", "加入房间", bsPushButton|wsTabStop, 282, 244, 92, 26, idcRoomJoin)
	w.roomLeaveBtn = w.create("BUTTON", "离开房间", bsPushButton|wsTabStop, 282, 244, 92, 26, idcRoomLeave)
	w.roomStateTxt = w.create("STATIC", "未加入房间", 0, 16, 278, 488, 18, idcRoomState)

	w.create("BUTTON", "连接状态", bsGroupBox, 8, 308, 504, 252, 0)
	w.listBox = w.create("LISTBOX", "", wsBorder|wsVScroll|lbsNotify, 16, 326, 488, 200, idcList)
	w.deleteBtn = w.create("BUTTON", "删除选中好友", bsPushButton|wsTabStop, 16, 532, 120, 26, idcDelete)

	w.create("BUTTON", "设置", bsGroupBox, 8, 564, 504, 128, 0)
	w.create("STATIC", "昵称", 0, 16, 584, 40, 20, 0)
	w.nameEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 58, 582, 150, 22, idcName)
	w.create("STATIC", "服务器", 0, 218, 584, 50, 20, 0)
	w.serverEdit = w.create("EDIT", "", wsBorder|esAutoHScroll|wsTabStop, 270, 582, 232, 22, idcServer)
	w.create("STATIC", "首次使用：填写昵称与服务器地址后点保存。服务器地址形如 ws://主机:9090/ws", 0, 16, 612, 488, 18, 0)
	w.saveBtn = w.create("BUTTON", "保存并连接", bsPushButton|wsTabStop, 16, 640, 120, 28, idcSave)
	w.quitBtn = w.create("BUTTON", "退出", bsPushButton|wsTabStop, 392, 640, 80, 28, idcQuit)
}

// loadFonts picks the stock GUI font for all controls and a bold face for the
// status line.
func (w *Window) loadFonts() {
	w.font, _, _ = procGetStockObject.Call(defaultGuiFont)
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	h, _, _ := procCreateFontW.Call(
		^uintptr(18), 0, 0, 0, 700, 0, 0, 0, 1, // height=-19, weight(700=Bold), DEFAULT_CHARSET
		0, 0, 5, 0, uintptr(unsafe.Pointer(face)))
	if h != 0 {
		w.sf = h
	} else {
		w.sf = w.font
	}
}

func (w *Window) applyFonts() {
	for _, h := range []uintptr{
		w.statusTxt, w.codeEdit, w.code2Edit, w.listBox,
		w.copyBtn, w.addBtn, w.deleteBtn, w.nameEdit, w.serverEdit,
		w.saveBtn, w.quitBtn,
		w.acctEdit, w.passEdit, w.loginBtn, w.registerBtn, w.logoutBtn,
		w.acctStateTxt, w.addHintTxt, w.roomCreateBtn, w.roomCodeEdit,
		w.roomJoinBtn, w.roomLeaveBtn, w.roomStateTxt,
	} {
		procSendMessageW.Call(h, wmSetFont, w.font, 1)
	}
	procSendMessageW.Call(w.statusTxt, wmSetFont, w.sf, 1)
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
