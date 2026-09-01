//go:build windows

package winutil

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Elevation constants.
const (
	tokenQuery     = 0x0008 // TOKEN_QUERY
	tokenElevation = 20     // TokenElevation
	swShow         = 5      // SW_SHOW
)

// IsElevated reports whether the current process runs with an admin token
// (needed to create the Wintun adapter and install routes).
func IsElevated() bool {
	var token uintptr
	cur, _, _ := procGetCurrentProcess.Call()
	r, _, _ := procOpenProcessToken.Call(cur, tokenQuery, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return false
	}
	defer procCloseHandle.Call(token)
	var elev uint32
	var ret uint32
	procGetTokenInformation.Call(token, tokenElevation, uintptr(unsafe.Pointer(&elev)), 4, uintptr(unsafe.Pointer(&ret)))
	return elev != 0
}

// RelaunchElevated re-executes the current process (same command line) with a
// UAC elevation prompt. It returns nil on success — the caller should exit, as
// the elevated copy takes over. It returns an error if the user declined or
// elevation is unavailable; the caller then continues non-elevated.
func RelaunchElevated() error {
	var buf [1024]uint16
	n, _, _ := procGetModuleFileNameW.Call(0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return fmt.Errorf("GetModuleFileNameW")
	}
	exe := syscall.UTF16ToString(buf[:n])

	p, _, _ := procGetCommandLineW.Call()
	if p == 0 {
		return fmt.Errorf("GetCommandLineW")
	}
	// GetCommandLineW returns a stable, Windows-owned buffer pointer in a uintptr.
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	words := unsafe.Slice((*uint16)(ptr), 1<<16) // command lines are ≤ 32767 chars
	l := 0
	for words[l] != 0 {
		l++
	}
	cmdline := syscall.UTF16ToString(words[:l])

	runas, _ := syscall.UTF16PtrFromString("runas")
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cmdPtr, _ := syscall.UTF16PtrFromString(cmdline)
	r, _, e := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(runas)), uintptr(unsafe.Pointer(exePtr)), uintptr(unsafe.Pointer(cmdPtr)), 0, swShow)
	if r <= 32 {
		return fmt.Errorf("ShellExecuteW(runas): %v", e)
	}
	return nil
}
