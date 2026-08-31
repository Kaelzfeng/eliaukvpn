package tray

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

// These tests pin the Win32 struct layouts this package feeds to user32 and
// shell32. If a layout drifts, the APIs silently misread fields — so the
// exact sizes and field offsets are asserted here.

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
	if off := unsafe.Offsetof(wndClass{}.lpszClass); off != 64 {
		t.Fatalf("WNDCLASSW.lpszClassName offset = %d, want 64", off)
	}
}

func TestNotifyIconDataLayout(t *testing.T) {
	n := notifyIconData{}
	if sz := unsafe.Sizeof(n); sz != 976 {
		t.Fatalf("NOTIFYICONDATAW size = %d, want 976", sz)
	}
	checks := map[string]uintptr{
		"hWnd":             unsafe.Offsetof(n.hWnd),
		"uCallbackMessage": unsafe.Offsetof(n.uCallbackMessage),
		"hIcon":            unsafe.Offsetof(n.hIcon),
		"szTip":            unsafe.Offsetof(n.szTip),
		"szInfo":           unsafe.Offsetof(n.szInfo),
		"guidItem":         unsafe.Offsetof(n.guidItem),
		"hBalloonIcon":     unsafe.Offsetof(n.hBalloonIcon),
	}
	want := map[string]uintptr{
		"hWnd":             8,
		"uCallbackMessage": 24,
		"hIcon":            32,
		"szTip":            40,
		"szInfo":           304,
		"guidItem":         952,
		"hBalloonIcon":     968,
	}
	for name, off := range checks {
		if off != want[name] {
			t.Errorf("NOTIFYICONDATAW.%s offset = %d, want %d", name, off, want[name])
		}
	}
}

func TestMakeIconICO(t *testing.T) {
	ico := makeIconICO()
	// ICONDIR: reserved(2)=0, type(2)=1, count(2)=1
	if binary.LittleEndian.Uint16(ico[0:2]) != 0 || binary.LittleEndian.Uint16(ico[2:4]) != 1 || binary.LittleEndian.Uint16(ico[4:6]) != 1 {
		t.Fatalf("bad ICONDIR: % x", ico[:6])
	}
	// ICONDIRENTRY at offset 6: width=32 height=32, 32bpp, size>0, offset=22
	if ico[6] != 32 || ico[7] != 32 {
		t.Fatalf("bad entry dims: %d x %d", ico[6], ico[7])
	}
	if binary.LittleEndian.Uint16(ico[12:14]) != 32 {
		t.Fatalf("bad bpp: %d", binary.LittleEndian.Uint16(ico[12:14]))
	}
	imgSize := binary.LittleEndian.Uint32(ico[14:18])
	imgOff := binary.LittleEndian.Uint32(ico[18:22])
	if imgOff != 22 {
		t.Fatalf("image offset = %d, want 22", imgOff)
	}
	// BITMAPINFOHEADER at 22: biSize=40, biWidth=32, biHeight=64
	if binary.LittleEndian.Uint32(ico[22:26]) != 40 {
		t.Fatalf("bad biSize: %d", binary.LittleEndian.Uint32(ico[22:26]))
	}
	if binary.LittleEndian.Uint32(ico[26:30]) != 32 || binary.LittleEndian.Uint32(ico[30:34]) != 64 {
		t.Fatalf("bad DIB dims: %d x %d", binary.LittleEndian.Uint32(ico[26:30]), binary.LittleEndian.Uint32(ico[30:34]))
	}
	wantSize := 40 + 32*32*4 + (32*32)/8
	if int(imgSize) != wantSize {
		t.Fatalf("image size = %d, want %d", imgSize, wantSize)
	}
	if len(ico) != 22+wantSize {
		t.Fatalf("total ico length = %d, want %d", len(ico), 22+wantSize)
	}
}

func TestBuildMenuAssignsIDs(t *testing.T) {
	tr, err := New()
	if err != nil {
		t.Fatal(err)
	}
	menu, err := tr.buildMenu([]Item{
		{Label: "status", Disabled: true},
		{Separator: true},
		{Label: "sub", Submenu: []Item{{Label: "leaf", ID: 7}}},
		{Label: "quit", ID: 42},
	})
	if err != nil {
		t.Fatalf("buildMenu: %v", err)
	}
	if menu == 0 {
		t.Fatal("CreatePopupMenu returned 0")
	}
	procDestroyMenu.Call(menu)
}
