//go:build windows

// Package winutil holds the small Win32 helpers the GUI needs that are not part
// of any UI toolkit: clipboard, elevation, and (later) window show/hide. It is a
// leaf package — syscall only, no third-party dependency.
package winutil

import "syscall"

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procCloseClipboard   = user32.NewProc("CloseClipboard")

	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procSetWindowLongPtrW   = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW     = user32.NewProc("CallWindowProcW")

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
