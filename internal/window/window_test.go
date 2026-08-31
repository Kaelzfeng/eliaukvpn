package window

import (
	"testing"
	"unsafe"
)

// These pin the Win32 struct layouts the window package feeds to user32. If a
// layout drifts, the APIs silently misread fields (same rationale as the
// internal/tray layout tests).

func TestMsgLayout(t *testing.T) {
	if sz := unsafe.Sizeof(msg{}); sz != 48 {
		t.Fatalf("MSG size = %d, want 48", sz)
	}
	if off := unsafe.Offsetof(msg{}.hwnd); off != 0 {
		t.Fatalf("MSG.hwnd offset = %d, want 0", off)
	}
	if off := unsafe.Offsetof(msg{}.wParam); off != 16 {
		t.Fatalf("MSG.wParam offset = %d, want 16", off)
	}
	if off := unsafe.Offsetof(msg{}.ptX); off != 40 {
		t.Fatalf("MSG.pt.x offset = %d, want 40", off)
	}
}

func TestWndClassLayout(t *testing.T) {
	if sz := unsafe.Sizeof(wndClass{}); sz != 72 {
		t.Fatalf("WNDCLASSW size = %d, want 72", sz)
	}
	if off := unsafe.Offsetof(wndClass{}.lpfnWndProc); off != 8 {
		t.Fatalf("WNDCLASSW.lpfnWndProc offset = %d, want 8", off)
	}
}
