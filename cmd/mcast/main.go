// Command mcast tests the join-side delivery of MC LAN advertisements on a
// wintun adapter. The wintun interface has no multicast flag, so lan.Listen
// skips joining the discovery group on it. This test answers which delivery
// path actually reaches a 0.0.0.0:4445 socket that joined 224.0.2.60:
//
//	a) multicast injection into the wintun adapter
//	b) unicast injection to the local virtual IP:4445
//	c) LocalEmit — a normal socket write to the group (loopback)
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/windows"

	"eliaukvpn/internal/vnic"
)

func main() {
	ad, err := vnic.Open("Eliauk-mcast", "10.99.0.2", "255.255.255.0")
	if err != nil {
		fmt.Println("adapter:", err)
		return
	}
	defer ad.Close()
	fmt.Println("adapter open")

	// Wait for the address to be usable.
	for i := 0; i < 20; i++ {
		c, berr := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("10.99.0.2"), Port: 0})
		if berr == nil {
			c.Close()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("address ready")

	// Socket bound on 0.0.0.0:4445, joined to the discovery group.
	lc := net.ListenConfig{Control: reuseAddr}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":4445")
	if err != nil {
		fmt.Println("bind :4445:", err)
		return
	}
	defer pc.Close()
	p := ipv4.NewPacketConn(pc)
	group := &net.UDPAddr{IP: net.ParseIP("224.0.2.60")}
	ifs, _ := net.Interfaces()
	joined := map[string]bool{}
	for _, iface := range ifs {
		err := p.JoinGroup(&iface, group)
		joined[iface.Name] = err == nil
		fmt.Printf("join on %-16s up=%v mc=%v : %v\n", iface.Name,
			iface.Flags&net.FlagUp != 0, iface.Flags&net.FlagMulticast != 0, err)
	}

	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, addr, err := p.ReadFrom(buf)
			if err != nil {
				fmt.Printf("read error: %v\n", err)
				return
			}
			fmt.Printf("GOT datagram src=%s len=%d %q\n", addr, n, buf[:n])
		}
	}()

	peer := [4]byte{10, 99, 0, 3}
	mc := [4]byte{224, 0, 2, 60}
	own := [4]byte{10, 99, 0, 2}
	motd := []byte("[MOTD]Eliauk Test World[/MOTD][AD]25565[/AD]")

	// (a) multicast injection into wintun adapter.
	fmt.Println("--- (a) multicast injection ---")
	_ = ad.Write(buildUDP(peer, mc, 4445, 4445, motd))
	time.Sleep(1500 * time.Millisecond)

	// (b) unicast injection to local virtual IP:4445.
	fmt.Println("--- (b) unicast injection to own IP:4445 ---")
	_ = ad.Write(buildUDP(peer, own, 4445, 4445, motd))
	time.Sleep(1500 * time.Millisecond)

	// (c) LocalEmit — socket write to the group.
	fmt.Println("--- (c) LocalEmit (socket -> group) ---")
	_, _ = pc.WriteTo(motd, group)
	time.Sleep(1500 * time.Millisecond)

	fmt.Println("done")
}

func reuseAddr(network, address string, c syscall.RawConn) error {
	var opErr error
	if err := c.Control(func(fd uintptr) {
		opErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return opErr
}

func buildUDP(sa, da [4]byte, sport, dport int, payload []byte) []byte {
	total := 20 + 8 + len(payload)
	pkt := make([]byte, total)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	pkt[8] = 64
	pkt[9] = 17
	copy(pkt[12:16], sa[:])
	copy(pkt[16:20], da[:])
	binary.BigEndian.PutUint16(pkt[20:22], uint16(sport))
	binary.BigEndian.PutUint16(pkt[22:24], uint16(dport))
	binary.BigEndian.PutUint16(pkt[24:26], uint16(8+len(payload)))
	copy(pkt[28:], payload)
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(pkt[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(pkt[10:12], ^uint16(sum))
	return pkt
}
