// Command injtest isolates why wintun-injected packets are dropped by tcpip.sys
// ("Not locally destined") despite the adapter having a proper local address.
//
// It opens a wintun adapter, verifies the read path (OS -> adapter), then
// injects several packet variants (different source/destination/protocol) and
// reports which ones Windows delivers to local sockets or answers itself.
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"eliaukvpn/internal/lan"
	"eliaukvpn/internal/vnic"
)

const ownIP = "10.99.0.2"

func main() {
	ad, err := vnic.Open("Eliauk-injtest", ownIP, "255.255.255.0")
	if err != nil {
		fmt.Println("adapter:", err)
		return
	}
	defer ad.Close()
	fmt.Println("adapter open")

	// Interface flags — is the wintun adapter multicast-capable for group joins?
	if iface, err := net.InterfaceByName("Eliauk-injtest"); err == nil {
		fmt.Printf("interface flags: %v (Multicast=%v Up=%v)\n", iface.Flags, iface.Flags&net.FlagMulticast != 0, iface.Flags&net.FlagUp != 0)
	}

	// Read path: OS packets routed into the adapter should appear here.
	readSeen := make(chan []byte, 16)
	go func() {
		for {
			pkt, err := ad.Read()
			if err != nil {
				fmt.Println("read loop:", err)
				return
			}
			ad.Release(pkt)
			select {
			case readSeen <- append([]byte(nil), pkt...):
			default:
			}
		}
	}()

	// Test 1: does the OS send packets into the adapter? Send UDP to a
	// non-local address on the wintun subnet -> routes via the adapter.
	gen, err := net.Dial("udp4", "10.99.0.99:4444")
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	_, _ = gen.Write([]byte("readpath"))
	select {
	case pkt := <-readSeen:
		fmt.Printf("READ PATH OK: OS->adapter packet (%d bytes): %s > %s\n", len(pkt), net.IP(pkt[12:16]), net.IP(pkt[16:20]))
	case <-time.After(2 * time.Second):
		fmt.Println("READ PATH FAIL: no packet from OS into adapter within 2s")
	}
	gen.Close()

	// Sockets to receive injected packets.
	any := listenUDP("0.0.0.0", 5555)
	if any != nil {
		defer any.Close()
	}
	own := listenUDP(ownIP, 5555)
	if own != nil {
		defer own.Close()
	}

	// Variant tests: inject and see if delivered.
	peer := [4]byte{10, 99, 0, 3}
	external := [4]byte{1, 2, 3, 4}
	self := [4]byte{10, 99, 0, 2}
	ownAddr := [4]byte{10, 99, 0, 2}
	nonlocal := [4]byte{10, 99, 0, 99}
	type variant struct {
		name    string
		src     [4]byte
		dst     [4]byte
		sport   int
		dport   int
		payload []byte
	}
	tests := []variant{
		{"same-subnet peer -> own IP", peer, ownAddr, 1234, 5555, []byte("AAA")},
		{"external -> own IP", external, ownAddr, 1234, 5555, []byte("BBB")},
		{"own IP -> own IP", self, ownAddr, 1234, 5555, []byte("CCC")},
		{"peer -> non-local 10.99.0.99 (control)", peer, nonlocal, 1234, 5555, []byte("EEE")},
	}
	for _, t := range tests {
		pkt := buildUDP(t.src, t.dst, t.sport, t.dport, t.payload)
		if err := ad.Write(pkt); err != nil {
			fmt.Printf("%-42s : inject error %v\n", t.name, err)
			continue
		}
		if probeBoth(any, own, 900*time.Millisecond) {
			fmt.Printf("%-42s : DELIVERED\n", t.name)
		} else {
			fmt.Printf("%-42s : NOT delivered\n", t.name)
		}
	}

	// ICMP echo request to own IP — does the stack process & answer it?
	pkt := buildICMP(peer, ownAddr, 8, 1, []byte("ping!"))
	if err := ad.Write(pkt); err != nil {
		fmt.Println("icmp inject:", err)
	}
	select {
	case pkt := <-readSeen:
		fmt.Printf("ICMP: stack answered with %d-byte packet %s > %s (proto %d)\n",
			len(pkt), net.IP(pkt[12:16]), net.IP(pkt[16:20]), pkt[9])
	case <-time.After(2 * time.Second):
		fmt.Println("ICMP: no response (injected echo not processed)")
	}

	// Multicast: a peer advertises to the MC discovery group. lan.Listen binds
	// :4445 and joins 224.0.2.60 on every up+multicast interface — the same
	// membership the MC client would use.
	ll, err := lan.Listen(func(pkt []byte, src net.IP) {
		fmt.Printf("LAN LISTENER got datagram src=%s len=%d %q\n", src, len(pkt), pkt)
	})
	if err != nil {
		fmt.Println("lan listen:", err)
		return
	}
	defer ll.Close()
	fmt.Println("lan listener up (group joined on all up+multicast interfaces)")
	mc := [4]byte{224, 0, 2, 60}
	pkt = buildUDP(peer, mc, 4445, 4445, []byte("[MOTD]Injected[/MOTD][AD]25565[/AD]"))
	if err := ad.Write(pkt); err != nil {
		fmt.Println("mcast inject:", err)
	} else {
		fmt.Println("injected multicast advertisement")
	}
	time.Sleep(2 * time.Second)
}

func listenUDP(host string, port int) *net.UDPConn {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(host), Port: port})
	if err != nil {
		fmt.Println("listen", host, port, ":", err)
		return nil
	}
	return c
}

func probeBoth(a, o *net.UDPConn, d time.Duration) bool {
	if a != nil {
		buf := make([]byte, 512)
		_ = a.SetReadDeadline(time.Now().Add(d))
		if n, _, err := a.ReadFromUDP(buf); err == nil && n > 0 {
			return true
		}
	}
	if o != nil {
		buf := make([]byte, 512)
		_ = o.SetReadDeadline(time.Now().Add(d))
		if n, _, err := o.ReadFromUDP(buf); err == nil && n > 0 {
			return true
		}
	}
	return false
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
	checksumIP(pkt)
	return pkt
}

func buildICMP(sa, da [4]byte, typ, code byte, payload []byte) []byte {
	total := 20 + 8 + len(payload)
	pkt := make([]byte, total)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	pkt[8] = 64
	pkt[9] = 1
	copy(pkt[12:16], sa[:])
	copy(pkt[16:20], da[:])
	pkt[20] = typ
	pkt[21] = code
	var sum uint32
	for i := 20; i+1 < total; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(pkt[i : i+2]))
	}
	if total%2 == 1 {
		sum += uint32(pkt[total-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(pkt[22:24], ^uint16(sum))
	checksumIP(pkt)
	return pkt
}

func checksumIP(pkt []byte) {
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(pkt[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(pkt[10:12], ^uint16(sum))
}

var _ = strconv.Itoa
var _ = strings.TrimSpace
