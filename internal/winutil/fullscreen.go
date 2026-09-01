//go:build windows

package winutil

import "unsafe"

// Fullscreen toggles a window between its normal state and a borderless
// fullscreen covering its monitor. It remembers the pre-fullscreen style and
// placement so Set can restore the window exactly.
type Fullscreen struct {
	active bool
	style  uintptr // GWL_STYLE before entering fullscreen
	place  windowPlacement
}

// Windows structs mirrored by field order (all naturally aligned on x64).
type point struct{ x, y int32 }

type rect struct{ left, top, right, bottom int32 }

// windowPlacement mirrors WINDOWPLACEMENT.
type windowPlacement struct {
	length   uint32
	flags    uint32
	showCmd  uint32
	ptMin    point
	ptMax    point
	rcNormal rect
}

// monitorInfo mirrors MONITORINFO.
type monitorInfo struct {
	cbSize    uint32
	rcMonitor rect
	rcWork    rect
	flags     uint32
}

// Fullscreen style/position constants.
const (
	gwlStyle     = ^uintptr(15) // GWL_STYLE (-16)
	wsCaption    = 0x00C00000   // WS_CAPTION
	wsThickFrame = 0x00040000   // WS_THICKFRAME
	wsMinimize   = 0x00020000   // WS_MINIMIZEBOX
	wsMaximize   = 0x00010000   // WS_MAXIMIZEBOX
	monDefNear   = 2            // MONITOR_DEFAULTTONEAREST
	swpFrame     = 0x0020       // SWP_FRAMECHANGED
	hwndTop      = 0            // HWND_TOP
)

// Set puts the window into (or out of) borderless fullscreen. It is idempotent:
// calling Set(hwnd, true) while already fullscreen is a no-op. Must run on the
// UI thread (it reads the current style/placement).
func (f *Fullscreen) Set(hwnd uintptr, fullscreen bool) {
	if fullscreen {
		if f.active {
			return
		}
		style, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle))
		f.style = style
		f.place.length = uint32(unsafe.Sizeof(f.place))
		procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&f.place)))

		mon, _, _ := procMonitorFromWindow.Call(hwnd, monDefNear)
		var mi monitorInfo
		mi.cbSize = uint32(unsafe.Sizeof(mi))
		procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))

		// Strip the caption + sizing border + min/max buttons, then cover the
		// whole monitor (not just the work area — true fullscreen).
		full := style &^ (wsCaption | wsThickFrame | wsMinimize | wsMaximize)
		procSetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle), full)
		w := uintptr(mi.rcMonitor.right - mi.rcMonitor.left)
		h := uintptr(mi.rcMonitor.bottom - mi.rcMonitor.top)
		procSetWindowPos.Call(hwnd, hwndTop,
			uintptr(int32(mi.rcMonitor.left)), uintptr(int32(mi.rcMonitor.top)), w, h, swpFrame)
		f.active = true
		return
	}

	if !f.active {
		return
	}
	procSetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle), f.style)
	procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&f.place)))
	f.active = false
}
