//go:build !windows

package window

import "errors"

// EvType / EvMsg / View / MsgHook are needed so cmd/gui (windows-only) is the
// only real consumer; this stub keeps the package building on other platforms
// with the same surface.
type EvType int

const (
	EvAdd EvType = iota
	EvCopy
	EvDelete
	EvSave
	EvQuit
)

type EvMsg struct {
	Type  EvType
	Text  string
	Text2 string
	Index int
}

type View struct {
	Status string
	Good   bool
	Code   string
	Rows   []string
	Name   string
	Server string
}

type MsgHook func(m uint32, wParam, lParam uintptr) bool

// Window is unavailable on non-Windows platforms.
type Window struct{}

var errUnsupported = errors.New("window: unsupported platform")

func New() (*Window, error)                     { return nil, errUnsupported }
func (w *Window) Events() <-chan EvMsg          { return nil }
func (w *Window) SetView(v View)                {}
func (w *Window) SetMsgHook(h MsgHook)          {}
func (w *Window) Hwnd() uintptr                 { return 0 }
func (w *Window) Show()                         {}
func (w *Window) Hide()                         {}
func (w *Window) Stop()                         {}
func (w *Window) Done() <-chan struct{}         { return nil }
func (w *Window) Run() error                    { return errUnsupported }
func CopyToClipboard(hwnd uintptr, text string) {}
func IsElevated() bool                          { return false }
func RelaunchElevated() error                   { return errUnsupported }
