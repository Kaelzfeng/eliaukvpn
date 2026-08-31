//go:build !windows

package tray

import "errors"

// Item mirrors the Windows build's menu model so callers compile everywhere.
type Item struct {
	Label     string
	Separator bool
	Disabled  bool
	Submenu   []Item
	ID        int
}

// Tray is a stub on non-Windows platforms; the notification-area icon is a
// Windows feature.
type Tray struct{}

// New returns an error on non-Windows platforms.
func New() (*Tray, error) { return nil, errors.New("tray: unsupported platform") }

func (t *Tray) SetMenu([]Item)        {}
func (t *Tray) SetTooltip(string)     {}
func (t *Tray) Run(func(int)) error   { return errors.New("tray: unsupported platform") }
func (t *Tray) Stop()                 {}
func (t *Tray) Done() <-chan struct{} { ch := make(chan struct{}); close(ch); return ch }
