//go:build windows

// Package tray provides a minimal Windows notification-area (tray) icon with
// a right-click context menu, implemented directly on the Win32 API through
// the stdlib syscall package (syscall.NewLazyDLL) — no third-party GUI
// dependency.
//
// Lifecycle: New() constructs a Tray; Run() must be called on the goroutine
// that will own the UI thread — it creates the hidden message window and
// pumps messages until Stop() is called. SetMenu/SetTooltip/SetIcon may be
// called from any goroutine while the window exists; the menu model is
// snapshotted on each right-click, so the caller simply replaces the model
// to refresh the menu.
package tray

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// Item is one context-menu entry. Separator items have only Separator set.
// A leaf item with a nonzero ID becomes a clickable command; the value is
// delivered back through the Run callback. Items with a Submenu are nested
// menus and are not clickable themselves.
type Item struct {
	Label     string
	Separator bool
	Disabled  bool // grayed out, not selectable
	Submenu   []Item
	ID        int
}

const (
	wmApp = 0x8000

	// CallbackMsg is the Shell_NotifyIcon callback message (WM_APP+1). A host
	// window (main window / message-only window) must forward it to
	// Tray.HandleTrayMsg so right-clicks and double-clicks work.
	CallbackMsg = wmApp + 1
	wmTray      = wmApp + 1 // alias for the internal callback constant
	wmQuitAsk   = wmApp + 2 // Stop() marshal: quit from the UI thread

	wmDestroy   = 0x0002
	wmRButtonUp = 0x0205
	wmLDblClk   = 0x0203

	// Mouse-message lParam values delivered with CallbackMsg to a host window.
	RButtonUp = wmRButtonUp
	LDblClk   = wmLDblClk

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString    = 0x00000000
	mfPopup     = 0x00000010
	mfGrayed    = 0x00000001
	mfSeparator = 0x00000800

	tpmLeftAlign   = 0x00000000
	tpmRightButton = 0x00000002
	tpmReturnCmd   = 0x00000100

	csDblClks = 0x0008

	imageIcon      = 0x00000001
	lrLoadFromFile = 0x00000010

	// hwndMessage is HWND_MESSAGE ((HWND)-3): a message-only window that never
	// shows. (HWND)-1 is HWND_TOPMOST, not HWND_MESSAGE.
	hwndMessage = ^uintptr(2)
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassW      = user32.NewProc("RegisterClassW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procLoadImageW          = user32.NewProc("LoadImageW")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
)

// wndClass mirrors WNDCLASSW. Layout is pinned by Go struct alignment (all
// fields naturally aligned), which matches the Win32 definition on x64.
type wndClass struct {
	style       uint32
	pad1        uint32
	lpfnWndProc uintptr
	cbClsExtra  int32
	cbWndExtra  int32
	hInstance   uintptr
	hIcon       uintptr
	hCursor     uintptr
	hbrBgnd     uintptr
	lpszMenu    uintptr
	lpszClass   uintptr
}

// msg mirrors MSG. Padding is explicit so the layout is exactly 48 bytes on
// x64 regardless of compiler alignment decisions.
type msg struct {
	hwnd    uintptr
	message uint32
	pad1    uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pad2    uint32
	ptX     int32
	ptY     int32
}

// notifyIconData mirrors NOTIFYICONDATAW (Vista+, with guidItem and
// hBalloonIcon). Go field alignment produces the correct 976-byte layout on
// x64; cbSize is set from unsafe.Sizeof at runtime.
type notifyIconData struct {
	cbSize            uint32
	pad1              uint32
	hWnd              uintptr
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	pad2              uint32
	hIcon             uintptr
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uTimeoutOrVersion uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          [16]byte // GUID: 952..968
	hBalloonIcon      uintptr  // 968, already 8-aligned
}

// Tray is a tray icon with a context menu.
type Tray struct {
	mu sync.Mutex

	items   []Item
	tooltip string
	icon    uintptr // HICON, lazily loaded

	hwnd   uintptr
	cb     uintptr // WndProc callback kept alive
	iconID uint32  // uID used for Shell_NotifyIcon
	added  bool

	selCh  chan int
	doneCh chan struct{}
	once   sync.Once
}

// New constructs an unstarted tray icon that owns its own hidden message
// window. Call Run to start it (it pumps the window's messages itself).
func New() (*Tray, error) {
	t := &Tray{
		iconID: 1,
		selCh:  make(chan int, 4),
		doneCh: make(chan struct{}),
	}
	t.cb = syscall.NewCallback(t.wndProc)
	return t, nil
}

// NewOnWindow attaches a tray icon to an existing window owned by the caller.
// The host window must forward tray.CallbackMsg to HandleTrayMsg and pump its
// own messages; this Tray never starts a message loop or registers a class.
// Menu selections still arrive on Select(); double-click is reported by the
// host (see RButtonUp / LDblClk constants).
func NewOnWindow(hwnd uintptr) *Tray {
	return &Tray{
		iconID: 1,
		hwnd:   hwnd,
		selCh:  make(chan int, 4),
		doneCh: make(chan struct{}),
	}
}

// Add publishes the tray icon (NIM_ADD). Call it after SetMenu/SetTooltip.
func (t *Tray) Add() error {
	if err := t.loadIcon(); err != nil {
		return err
	}
	t.update(true)
	return nil
}

// Delete removes the tray icon and frees resources. Safe from any goroutine.
func (t *Tray) Delete() {
	t.deleteIcon()
	t.once.Do(func() { close(t.doneCh) })
}

// HandleTrayMsg dispatches a Shell_NotifyIcon callback message (CallbackMsg)
// forwarded from the host window's wndProc; lParam is the mouse message.
// Right-click shows the context menu. Must be called on the UI thread.
func (t *Tray) HandleTrayMsg(l uintptr) {
	if uint32(l) == wmRButtonUp {
		t.showMenu()
	}
}

// wndProc is the message-only window's procedure. All UI-thread work
// (menu showing, quit marshaling) happens here on the pump goroutine.
func (t *Tray) wndProc(hwnd uintptr, m uint32, w, l uintptr) uintptr {
	switch m {
	case wmTray:
		switch uint32(l) {
		case wmRButtonUp: // right-click: show the context menu
			t.showMenu()
		}
	case wmQuitAsk:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(m), w, l)
	return r
}

// SetMenu replaces the context-menu model. It is applied on the next
// right-click; safe to call from any goroutine.
func (t *Tray) SetMenu(items []Item) {
	t.mu.Lock()
	t.items = items
	t.mu.Unlock()
}

// SetTooltip replaces the tray tooltip text and re-publishes the icon.
func (t *Tray) SetTooltip(s string) {
	t.mu.Lock()
	t.tooltip = s
	t.mu.Unlock()
	t.update(false)
}

// Run starts the UI thread: registers the window class, creates the hidden
// message window, adds the tray icon, then pumps messages until Stop is
// called. onSelect receives the ID of the menu command the user chose (a
// leaf item with a nonzero ID). Run must be called on the goroutine that
// owns the window; it blocks until Stop.
func (t *Tray) Run(onSelect func(id int)) error {
	className, err := syscall.UTF16PtrFromString("EliaukTrayClass")
	if err != nil {
		return err
	}
	inst, _, _ := procGetModuleHandleW.Call(0)

	wc := &wndClass{
		style:       csDblClks,
		lpfnWndProc: t.cb,
		hInstance:   inst,
		lpszClass:   uintptr(unsafe.Pointer(className)),
	}
	if r, _, e := procRegisterClassW.Call(uintptr(unsafe.Pointer(wc))); r == 0 {
		if e == syscall.Errno(1410) { // ERROR_CLASS_ALREADY_EXISTS: fine
			_ = e
		} else {
			return fmt.Errorf("RegisterClassW failed: %v", e)
		}
	}

	hwnd, _, e2 := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), 0, 0,
		uintptr(0x80000000), uintptr(0x80000000), 0, 0, // CW_USEDEFAULT x2, w=0 h=0
		hwndMessage, 0, inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW failed: %v", e2)
	}
	t.mu.Lock()
	t.hwnd = hwnd
	t.mu.Unlock()

	if err := t.loadIcon(); err != nil {
		// Not fatal for the tray's function — continue without an icon.
		_ = err
	}
	t.update(true) // NIM_ADD with the initial tooltip/icon

	for {
		var m msg
		got, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if got == 0 { // WM_QUIT
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	t.deleteIcon()
	procDestroyWindow.Call(hwnd)
	t.once.Do(func() { close(t.doneCh) })
	return nil
}

// Stop asks the UI thread to quit (WM_QUIT), which makes Run return. Safe
// from any goroutine.
func (t *Tray) Stop() {
	t.mu.Lock()
	hwnd := t.hwnd
	t.mu.Unlock()
	if hwnd == 0 {
		t.once.Do(func() { close(t.doneCh) })
		return
	}
	procPostMessageW.Call(hwnd, wmQuitAsk, 0, 0)
}

// Done is closed when Run has exited.
func (t *Tray) Done() <-chan struct{} { return t.doneCh }

// Select returns the next menu command ID chosen by the user, or false if
// the tray has stopped.
func (t *Tray) Select() (int, bool) {
	select {
	case id := <-t.selCh:
		return id, true
	case <-t.doneCh:
		return 0, false
	}
}

// showMenu builds and displays the context menu at the cursor on the UI
// thread. Called only from wndProc.
func (t *Tray) showMenu() {
	t.mu.Lock()
	items := t.items
	hwnd := t.hwnd
	t.mu.Unlock()

	menu, err := t.buildMenu(items)
	if err != nil {
		return
	}
	defer procDestroyMenu.Call(menu)

	var pt struct{ x, y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(hwnd)
	r, _, _ := procTrackPopupMenu.Call(menu,
		tpmLeftAlign|tpmRightButton|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y), 0, hwnd, 0)
	if r != 0 {
		select {
		case t.selCh <- int(r):
		default:
		}
	}
}

// buildMenu recursively converts the Item model into a Win32 popup menu.
// AppendMenuW copies the strings, so they only need to outlive this call.
func (t *Tray) buildMenu(items []Item) (uintptr, error) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return 0, fmt.Errorf("CreatePopupMenu failed")
	}
	for _, it := range items {
		switch {
		case it.Separator:
			procAppendMenuW.Call(menu, mfSeparator, 0, 0)
			continue
		case len(it.Submenu) > 0:
			sub, err := t.buildMenu(it.Submenu)
			if err != nil {
				procDestroyMenu.Call(menu)
				return 0, err
			}
			label, _ := syscall.UTF16PtrFromString(it.Label)
			procAppendMenuW.Call(menu, mfPopup|mfString, sub, uintptr(unsafe.Pointer(label)))
			continue
		}
		flags := uintptr(mfString)
		if it.Disabled {
			flags |= mfGrayed
		}
		label, _ := syscall.UTF16PtrFromString(it.Label)
		procAppendMenuW.Call(menu, flags, uintptr(it.ID), uintptr(unsafe.Pointer(label)))
	}
	return menu, nil
}

// loadIcon writes the generated ICO to a temp file and loads it as a HICON.
func (t *Tray) loadIcon() error {
	ico := IconICO()
	f, err := os.CreateTemp("", "eliauk-*.ico")
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
	h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(path)), imageIcon, 16, 16, lrLoadFromFile)
	if h == 0 {
		return fmt.Errorf("LoadImageW failed for %s", name)
	}
	t.mu.Lock()
	t.icon = h
	t.mu.Unlock()
	return nil
}

// update re-publishes the tray icon (NIM_ADD on first call, then NIM_MODIFY).
func (t *Tray) update(add bool) {
	t.mu.Lock()
	hwnd := t.hwnd
	icon := t.icon
	tip := t.tooltip
	id := t.iconID
	t.mu.Unlock()
	if hwnd == 0 {
		return
	}

	var tipBuf [128]uint16
	copy(tipBuf[:], syscall.StringToUTF16(tip))

	nid := &notifyIconData{
		hWnd:             hwnd,
		uID:              id,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTray,
		hIcon:            icon,
	}
	copy(nid.szTip[:], tipBuf[:])
	nid.cbSize = uint32(unsafe.Sizeof(*nid))

	msg := uint32(nimModify)
	if add {
		msg = nimAdd
	}
	procShellNotifyIconW.Call(uintptr(msg), uintptr(unsafe.Pointer(nid)))
}

// deleteIcon removes the tray icon and frees the HICON.
func (t *Tray) deleteIcon() {
	t.mu.Lock()
	hwnd := t.hwnd
	icon := t.icon
	id := t.iconID
	t.icon = 0
	t.mu.Unlock()
	if hwnd != 0 {
		nid := &notifyIconData{
			cbSize: uint32(unsafe.Sizeof(notifyIconData{})),
			hWnd:   hwnd,
			uID:    id,
		}
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(nid)))
	}
	if icon != 0 {
		procDestroyIcon.Call(icon)
	}
}
