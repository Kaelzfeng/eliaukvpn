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

	procGetStockObject = gdi32.NewProc("GetStockObject")
	procCreateFontW    = gdi32.NewProc("CreateFontW")
	procSetTextColor   = gdi32.NewProc("SetTextColor")
	procSetBkMode      = gdi32.NewProc("SetBkMode")

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
