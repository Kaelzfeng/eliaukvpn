// Package vnic wraps a Wintun virtual adapter (M4). It hands the caller raw
// IP packets that the OS wants to send onto the virtual LAN and lets the
// caller inject packets back in as if they arrived from it, so the OS believes
// it is on a real LAN segment.
//
// Creating a Wintun adapter requires an elevated process.
package vnic

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

const (
	// DefaultMTU is the virtual adapter MTU.
	DefaultMTU = 1500
	// ringCapacity is the wintun session ring size (4 MiB).
	ringCapacity = 0x400000
	// addrReadyTimeout bounds how long we wait for the assigned IP to become
	// usable after netsh configures it.
	addrReadyTimeout = 15 * time.Second
)

// ErrClosed is returned by Read after Close, and by Write once the adapter is
// shut down.
var ErrClosed = errors.New("vnic: adapter closed")

// Adapter wraps a Wintun session.
type Adapter struct {
	name     string
	a        *wintun.Adapter
	sess     wintun.Session
	closed   atomic.Bool
	readers  atomic.Int32 // number of goroutines inside Read
	closeOnce sync.Once
}

// Open creates (or opens) the adapter with the given name, assigns it an IP
// address on the virtual subnet and starts its session.
//
// The tunnel type MUST be "Wintun" — this is part of the adapter's driver
// identity. A custom value still returns success but the interface never
// materializes in Windows' network stack, silently breaking all delivery.
//
// Open does not return until the assigned IP is actually usable. Injecting
// packets (or binding sockets to the IP) before the stack finishes configuring
// the address causes tcpip.sys to drop them as "not locally destined"; the
// address takes ~3 seconds to become bindable after netsh sets it.
func Open(name, ip, mask string) (*Adapter, error) {
	a, err := wintun.CreateAdapter(name, "Wintun", nil)
	if err != nil {
		return nil, fmt.Errorf("create adapter: %w", err)
	}
	sess, err := a.StartSession(ringCapacity)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("start session: %w", err)
	}
	ad := &Adapter{name: name, a: a, sess: sess}
	if err := ad.setIP(ip, mask); err != nil {
		ad.Close()
		return nil, err
	}
	if err := ad.waitAddressReady(ip); err != nil {
		ad.Close()
		return nil, err
	}
	return ad, nil
}

// Name returns the adapter's network interface name.
func (ad *Adapter) Name() string { return ad.name }

// IfIndex returns the Windows interface index for the adapter, or 0 if the
// interface is not present. The index is used to install per-peer host routes
// so traffic to a peer's virtual IP is forced into this adapter.
func (ad *Adapter) IfIndex() int {
	iface, err := net.InterfaceByName(ad.name)
	if err != nil {
		return 0
	}
	return iface.Index
}

// Close shuts the adapter down. It is safe to call while a goroutine is
// blocked in Read: the read waiter is woken and given a moment to leave the
// wintun call before the session (and its mapped ring memory) is torn down,
// avoiding the access violation that otherwise occurs when ReceivePacket is
// in flight during End.
func (ad *Adapter) Close() {
	if ad == nil {
		return
	}
	ad.closeOnce.Do(func() {
		ad.closed.Store(true)
		if ad.sess != (wintun.Session{}) {
			// Wake a goroutine blocked in Read so it can observe ErrClosed and
			// exit its receive loop.
			windows.SetEvent(ad.sess.ReadWaitEvent())
			// Give in-flight ReceivePacket/Release calls up to 100ms to return
			// before the ring memory is unmapped.
			for i := 0; i < 50 && ad.readers.Load() > 0; i++ {
				time.Sleep(2 * time.Millisecond)
			}
			ad.sess.End()
		}
		_ = ad.a.Close()
	})
}

// setIP assigns a static IPv4 address to the adapter via netsh.
func (ad *Adapter) setIP(ip, mask string) error {
	out, err := exec.Command("netsh", "interface", "ip", "set", "address",
		"name="+ad.name, "static", ip, mask).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh set ip %s: %v: %s", ip, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitAddressReady blocks until a socket can be bound to the assigned IP, which
// is the point at which the address is a real local destination.
func (ad *Adapter) waitAddressReady(ip string) error {
	deadline := time.Now().Add(addrReadyTimeout)
	for time.Now().Before(deadline) {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: 0})
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("address %s never became bindable after netsh", ip)
}

// Read blocks until an inbound IP packet is available. The returned slice is
// owned by the adapter; call Release on it when done reading.
func (ad *Adapter) Read() ([]byte, error) {
	if ad.closed.Load() {
		return nil, ErrClosed
	}
	ad.readers.Add(1)
	defer ad.readers.Add(-1)
	for {
		pkt, err := ad.sess.ReceivePacket()
		if err == windows.ERROR_NO_MORE_ITEMS {
			// ReceivePacket is non-blocking: wait for the ring to get data.
			if ad.closed.Load() {
				return nil, ErrClosed
			}
			windows.WaitForSingleObject(ad.sess.ReadWaitEvent(), windows.INFINITE)
			continue
		}
		return pkt, err
	}
}

// Release returns a packet obtained from Read to the session.
func (ad *Adapter) Release(pkt []byte) {
	if pkt != nil {
		ad.sess.ReleaseReceivePacket(pkt)
	}
}

// Write injects an IP packet into the adapter as if it had arrived from the
// virtual LAN.
func (ad *Adapter) Write(pkt []byte) error {
	if ad.closed.Load() {
		return ErrClosed
	}
	out, err := ad.sess.AllocateSendPacket(len(pkt))
	if err != nil {
		return err
	}
	copy(out, pkt)
	ad.sess.SendPacket(out)
	return nil
}
