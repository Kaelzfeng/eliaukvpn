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

func TestRectLayout(t *testing.T) {
	if sz := unsafe.Sizeof(rect{}); sz != 16 {
		t.Fatalf("RECT size = %d, want 16", sz)
	}
}

func TestDrawItemLayout(t *testing.T) {
	if sz := unsafe.Sizeof(drawItemStruct{}); sz != 64 {
		t.Fatalf("DRAWITEMSTRUCT size = %d, want 64", sz)
	}
	// Five contiguous UINTs: hwndItem must land at offset 24 (after padding).
	if off := unsafe.Offsetof(drawItemStruct{}.ctlID); off != 4 {
		t.Fatalf("DRAWITEMSTRUCT.ctlID offset = %d, want 4", off)
	}
	if off := unsafe.Offsetof(drawItemStruct{}.hwndItem); off != 24 {
		t.Fatalf("DRAWITEMSTRUCT.hwndItem offset = %d, want 24", off)
	}
	if off := unsafe.Offsetof(drawItemStruct{}.hDC); off != 32 {
		t.Fatalf("DRAWITEMSTRUCT.hDC offset = %d, want 32", off)
	}
}

func TestPaintLayout(t *testing.T) {
	if sz := unsafe.Sizeof(paintStruct{}); sz != 72 {
		t.Fatalf("PAINTSTRUCT size = %d, want 72", sz)
	}
	// hdc(8) + fErase(4) then RECT: rcPaint is at 12, not 16 (RECT is
	// 4-byte-aligned int32s).
	if off := unsafe.Offsetof(paintStruct{}.rcPaint); off != 12 {
		t.Fatalf("PAINTSTRUCT.rcPaint offset = %d, want 12", off)
	}
}

func TestTrackMouseLayout(t *testing.T) {
	// Go pads the struct to 24 bytes for uintptr alignment, but the cbSize we
	// pass to TrackMouseEvent is 20, so Windows reads only 20 bytes. The field
	// offsets are what matter, not Go's sizeof.
	if off := unsafe.Offsetof(trackMouseEvent{}.dwFlags); off != 4 {
		t.Fatalf("TRACKMOUSEEVENT.dwFlags offset = %d, want 4", off)
	}
	if off := unsafe.Offsetof(trackMouseEvent{}.hwndTrack); off != 8 {
		t.Fatalf("TRACKMOUSEEVENT.hwndTrack offset = %d, want 8", off)
	}
	if off := unsafe.Offsetof(trackMouseEvent{}.dwHoverTime); off != 16 {
		t.Fatalf("TRACKMOUSEEVENT.dwHoverTime offset = %d, want 16", off)
	}
}
