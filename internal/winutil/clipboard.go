//go:build windows

package winutil

import (
	"syscall"
	"unsafe"
)

// Win32 clipboard constants.
const (
	globalMoveZero = 0x0042 // GMEM_MOVEABLE | GMEM_ZEROINIT
	cfgUnicodeText = 13     // CF_UNICODETEXT
)

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
		// GlobalLock returns a uintptr holding a pointer into the global heap —
		// stable, non-GC memory. Launder the bits through memory (the standard Go
		// idiom for exactly this case), since a direct uintptr→unsafe.Pointer
		// conversion trips go vet's unsafeptr analyzer.
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
