//go:build windows

package winutil

// Window show/hide and subclassing helpers, used by the WebView2 host to
// implement close-to-tray and restore-from-tray.

const (
	SWShow     = 5          // SW_SHOW
	SWHide     = 0          // SW_HIDE
	gwlWndProc = ^uintptr(3) // GWLP_WNDPROC (-4, two's complement)
)

// ShowWindow sets the window's show state (SW_SHOW / SW_HIDE / ...).
func ShowWindow(hwnd uintptr, show int) {
	procShowWindow.Call(hwnd, uintptr(show))
}

// SetForegroundWindow brings the window to the foreground.
func SetForegroundWindow(hwnd uintptr) {
	procSetForegroundWindow.Call(hwnd)
}

// SetWindowProc subclasses the window's procedure, returning the previous
// procedure to chain to via CallWindowProc. cb must be a syscall.NewCallback
// value the caller keeps alive.
func SetWindowProc(hwnd uintptr, cb uintptr) uintptr {
	prev, _, _ := procSetWindowLongPtrW.Call(hwnd, uintptr(gwlWndProc), cb)
	return prev
}

// CallWindowProc invokes a window procedure directly (used to chain from a
// subclass to the original procedure).
func CallWindowProc(proc, hwnd, msg, w, l uintptr) uintptr {
	r, _, _ := procCallWindowProcW.Call(proc, hwnd, msg, w, l)
	return r
}
