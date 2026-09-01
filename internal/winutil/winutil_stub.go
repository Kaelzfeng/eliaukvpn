//go:build !windows

package winutil

// Non-Windows stubs so the module still compiles cross-platform. cmd/gui is
// Windows-only, so these are never exercised there.

func CopyToClipboard(hwnd uintptr, text string) {}

func IsElevated() bool { return false }

func RelaunchElevated() error { return nil }
