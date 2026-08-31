// Package vnic wraps a Wintun virtual adapter (M4). It hands the caller raw
// IP packets that the OS wants to send onto the virtual LAN and lets the
// caller inject packets back in as if they arrived from it, so the OS believes
// it is on a real LAN segment.
//
// Creating a Wintun adapter requires an elevated process.
package vnic

import (
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

const (
	// DefaultMTU is the virtual adapter MTU.
	DefaultMTU = 1500
	// ringCapacity is the wintun session ring size (4 MiB).
	ringCapacity = 0x400000
)

// Adapter wraps a Wintun session.
type Adapter struct {
	name string
	a    *wintun.Adapter
	sess wintun.Session
}

// Open creates (or opens) the adapter with the given name, assigns it an IP
// address on the virtual subnet and starts its session.
func Open(name, ip, mask string) (*Adapter, error) {
	a, err := wintun.CreateAdapter(name, "Eliauk", nil)
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
	return ad, nil
}

// Name returns the adapter's network interface name.
func (ad *Adapter) Name() string { return ad.name }

// Close ends the session and closes the adapter handle.
func (ad *Adapter) Close() {
	if ad == nil {
		return
	}
	ad.sess.End()
	_ = ad.a.Close()
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

// Read blocks until an inbound IP packet is available. The returned slice is
// owned by the adapter; call Release on it when done reading.
func (ad *Adapter) Read() ([]byte, error) {
	for {
		pkt, err := ad.sess.ReceivePacket()
		if err == windows.ERROR_NO_MORE_ITEMS {
			// ReceivePacket is non-blocking: wait for the ring to get data.
			windows.WaitForSingleObject(ad.sess.ReadWaitEvent(), windows.INFINITE)
			continue
		}
		return pkt, err
	}
}

// Release returns a packet obtained from Read to the session.
func (ad *Adapter) Release(pkt []byte) {
	ad.sess.ReleaseReceivePacket(pkt)
}

// Write injects an IP packet into the adapter as if it had arrived from the
// virtual LAN.
func (ad *Adapter) Write(pkt []byte) error {
	out, err := ad.sess.AllocateSendPacket(len(pkt))
	if err != nil {
		return err
	}
	copy(out, pkt)
	ad.sess.SendPacket(out)
	return nil
}
