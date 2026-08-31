//go:build windows

package window

import "syscall"

// Win32 bindings and struct mirrors for the main window. The layouts of
// wndClass and msg are pinned by window_test.go so a drift fails loudly
// instead of silently misreading fields (see internal/tray for the same
// approach on the notification-icon structs).

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")
	uxtheme  = syscall.NewLazyDLL("uxtheme.dll")

	procRegisterClassW      = user32.NewProc("RegisterClassW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procSendMessageW        = user32.NewProc("SendMessageW")
	procSetWindowTextW      = user32.NewProc("SetWindowTextW")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procUpdateWindow        = user32.NewProc("UpdateWindow")
	procInvalidateRect      = user32.NewProc("InvalidateRect")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procLoadImageW          = user32.NewProc("LoadImageW")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procGetSysColorBrush    = user32.NewProc("GetSysColorBrush")
	procOpenClipboard       = user32.NewProc("OpenClipboard")
	procEmptyClipboard      = user32.NewProc("EmptyClipboard")
	procSetClipboardData    = user32.NewProc("SetClipboardData")
	procCloseClipboard      = user32.NewProc("CloseClipboard")
	// UI rework (theme.go): drawing, owner-draw, button hover tracking.
	procCallWindowProcW     = user32.NewProc("CallWindowProcW")
	procGetWindowLongPtrW   = user32.NewProc("GetWindowLongPtrW")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")
	procAdjustWindowRectEx  = user32.NewProc("AdjustWindowRectEx")
	procSetWindowLongPtrW   = user32.NewProc("SetWindowLongPtrW")
	procBeginPaint          = user32.NewProc("BeginPaint")
	procEndPaint            = user32.NewProc("EndPaint")
	procGetClientRect       = user32.NewProc("GetClientRect")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procTrackMouseEvent     = user32.NewProc("TrackMouseEvent")
	procDrawTextW           = user32.NewProc("DrawTextW")
	procSetCapture          = user32.NewProc("SetCapture")
	procReleaseCapture      = user32.NewProc("ReleaseCapture")

	procGetStockObject = gdi32.NewProc("GetStockObject")
	procCreateFontW    = gdi32.NewProc("CreateFontW")
	procSetTextColor   = gdi32.NewProc("SetTextColor")
	procSetBkColor     = gdi32.NewProc("SetBkColor")
	procSetBkMode      = gdi32.NewProc("SetBkMode")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procCreatePen        = gdi32.NewProc("CreatePen")
	procSelectObject     = gdi32.NewProc("SelectObject")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	procRoundRect        = gdi32.NewProc("RoundRect")
	procEllipse          = gdi32.NewProc("Ellipse")

	// FillRect is a user32 export (legacy split: it takes GDI handles but
	// lives in user32.dll alongside DrawTextW).
	procFillRect = user32.NewProc("FillRect")

	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
	procGetModuleFileNameW = kernel32.NewProc("GetModuleFileNameW")
	procGetCommandLineW    = kernel32.NewProc("GetCommandLineW")
	procGetCurrentProcess  = kernel32.NewProc("GetCurrentProcess")
	procCloseHandle        = kernel32.NewProc("CloseHandle")
	procGlobalAlloc        = kernel32.NewProc("GlobalAlloc")
	procGlobalLock         = kernel32.NewProc("GlobalLock")
	procGlobalUnlock       = kernel32.NewProc("GlobalUnlock")

	procOpenProcessToken    = advapi32.NewProc("OpenProcessToken")
	procGetTokenInformation = advapi32.NewProc("GetTokenInformation")

	procShellExecuteW = shell32.NewProc("ShellExecuteW")

	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	procSetWindowTheme        = uxtheme.NewProc("SetWindowTheme")
)

// wndClass mirrors WNDCLASSW (see internal/tray for the same layout, pinned by
// tests to 72 bytes on x64).
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

// msg mirrors MSG (48 bytes on x64).
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

// ---- UI-rework mirrors (pinned by window_test.go) ----

// rect mirrors RECT (16 bytes on x64).
type rect struct {
	left, top, right, bottom int32
}

// drawItemStruct mirrors DRAWITEMSTRUCT. The five leading UINT fields are
// contiguous (no padding), so hwndItem lands at offset 24 — 64 bytes on x64.
type drawItemStruct struct {
	ctlType    uint32
	ctlID      uint32
	itemID     uint32
	itemAction uint32
	itemState  uint32
	hwndItem   uintptr
	hDC        uintptr
	rcItem     rect
	itemData   uintptr
}

// paintStruct mirrors PAINTSTRUCT (72 bytes on x64): hdc(8) fErase(4)
// rcPaint(16, aligned) fRestore(4) fIncUpdate(4) rgbReserved[32].
type paintStruct struct {
	hdc         uintptr
	fErase      uint32
	rcPaint     rect
	fRestore    uint32
	fIncUpdate  uint32
	rgbReserved [32]byte
}

// trackMouseEvent mirrors TRACKMOUSEEVENT (20 bytes on x64).
type trackMouseEvent struct {
	cbSize      uint32
	dwFlags     uint32
	hwndTrack   uintptr
	dwHoverTime uint32
}
