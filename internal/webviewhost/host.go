//go:build windows

package webviewhost

import (
	"encoding/json"
	"errors"
	"log"
	"runtime"
	"sync"
	"syscall"

	webview2 "github.com/jchv/go-webview2"

	"eliaukvpn/internal/winutil"
)

var errRuntime = errors.New("需要安装 WebView2 运行时（Microsoft Edge WebView2 Runtime）")

// Window messages the host subclass intercepts for close/minimize-to-tray.
const (
	wmClose      = 0x0010
	wmSysCommand = 0x0112
	scMinimize   = 0xF020
)

// Host wraps a WebView2 window plus the Go<->JS bridge.
type Host struct {
	w       webview2.WebView
	hwnd    uintptr
	actions chan Action
	ready   chan struct{}
	done    chan struct{}

	prevWndProc uintptr // original wndProc, chained to by hostWndProc
	subCB       uintptr // keep-alive for the subclass callback

	mu         sync.Mutex
	fullscreen bool
	fs         winutil.Fullscreen

	readyOnce sync.Once
	doneOnce  sync.Once
}

// New returns an unstarted Host. Call Run on a dedicated goroutine.
func New() *Host {
	return &Host{
		actions: make(chan Action, 16),
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Ready is closed once the WebView2 window exists and the bridge is wired.
func (h *Host) Ready() <-chan struct{} { return h.ready }

// Done is closed once Run returns (window terminated).
func (h *Host) Done() <-chan struct{} { return h.done }

// Actions returns the channel of user actions decoded from the frontend.
func (h *Host) Actions() <-chan Action { return h.actions }

// Window returns the host window HWND (valid after Ready).
func (h *Host) Window() uintptr { return h.hwnd }

// Run creates the WebView2 window, wires the bridge, and pumps messages until
// Close is called or the window is destroyed. It must run on the goroutine that
// owns the UI thread — LockOSThread is taken here because webview2 captures the
// creating thread's ID for Dispatch. Blocks.
func (h *Host) Run() error {
	runtime.LockOSThread()

	w := webview2.New(false)
	if w == nil {
		return errRuntime
	}
	h.w = w
	w.SetTitle("Eliauk VPN")
	w.SetSize(1080, 720, webview2.HintNone)
	if err := w.Bind("eliaukAction", h.handleAction); err != nil {
		return err
	}
	w.SetHtml(PageHTML())
	h.hwnd = uintptr(w.Window())
	h.subclassWndProc()
	h.readyOnce.Do(func() { close(h.ready) })

	w.Run()

	h.doneOnce.Do(func() { close(h.done) })
	return nil
}

// Close terminates the message loop. Safe from any goroutine.
func (h *Host) Close() {
	if h.w != nil {
		h.w.Terminate()
	}
}

// Show restores and foregrounds the host window (from the tray). Safe from any
// goroutine; marshaled onto the UI thread.
func (h *Host) Show() {
	if h.w == nil {
		return
	}
	h.w.Dispatch(func() {
		winutil.ShowWindow(h.hwnd, winutil.SWShow)
		winutil.SetForegroundWindow(h.hwnd)
	})
}

// Hide hides the host window without terminating it (to the tray). Safe from
// any goroutine.
func (h *Host) Hide() {
	if h.w == nil {
		return
	}
	h.w.Dispatch(func() { winutil.ShowWindow(h.hwnd, winutil.SWHide) })
}

// ToggleFullscreen flips the window between normal and borderless fullscreen.
// The flag flips synchronously so a Push immediately after reflects it; the
// Win32 work is marshaled onto the UI thread.
func (h *Host) ToggleFullscreen() {
	if h.w == nil {
		return
	}
	h.mu.Lock()
	h.fullscreen = !h.fullscreen
	enter := h.fullscreen
	h.mu.Unlock()
	h.w.Dispatch(func() { h.fs.Set(h.hwnd, enter) })
}

// Fullscreen reports whether the window is currently fullscreen.
func (h *Host) Fullscreen() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fullscreen
}

// subclassWndProc installs hostWndProc as the window procedure so closing or
// minimizing the window hides it to the tray instead of terminating the app.
// Must run on the UI thread, after hwnd is known.
func (h *Host) subclassWndProc() {
	h.subCB = syscall.NewCallback(h.hostWndProc)
	h.prevWndProc = winutil.SetWindowProc(h.hwnd, h.subCB)
}

// hostWndProc is the subclass: it turns close/minimize into hide-to-tray and
// chains every other message to the original (go-webview2) procedure.
func (h *Host) hostWndProc(hwnd uintptr, m uint32, w, l uintptr) uintptr {
	if m == wmClose || (m == wmSysCommand && (w&0xFFF0) == scMinimize) {
		h.Hide()
		return 0
	}
	return winutil.CallWindowProc(h.prevWndProc, hwnd, uintptr(m), w, l)
}

// Push renders a full state snapshot on the UI thread. Safe from any goroutine.
func (h *Host) Push(s State) {
	if h.w == nil {
		return
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return
	}
	// json.Marshal of a string is a valid JS string literal; encoding/json already
	// escapes <, >, & and U+2028/U+2029, so it is safe to splice into Eval.
	arg, _ := json.Marshal(string(payload))
	js := "window.eliauk.render(" + string(arg) + ")"
	h.w.Dispatch(func() { h.w.Eval(js) })
}

// handleAction is the single Bind entry point: JS calls window.eliaukAction with
// a JSON string. It runs on the UI thread, so it only decodes and hands the
// action off without blocking.
func (h *Host) handleAction(actionJSON string) {
	a, err := ParseAction(actionJSON)
	if err != nil {
		log.Printf("webviewhost: bad action %q: %v", actionJSON, err)
		return
	}
	select {
	case h.actions <- a:
	default:
	}
}
