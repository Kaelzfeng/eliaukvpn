// Command admcast verifies that an IP packet injected into a Wintun adapter is
// delivered to a multicast socket that joined the MC discovery group — the
// premise of the M5 receive path. If it works, injected advertisements reach
// the MC client's socket without any local re-emission.
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"eliaukvpn/internal/lan"
	"eliaukvpn/internal/vnic"
)

func main() {
	ad, err := vnic.Open("Eliauk-admcast", "10.99.0.2", "255.255.255.0")
	if err != nil {
		fmt.Println("adapter:", err)
		return
	}
	defer ad.Close()
	fmt.Println("adapter open")

	got := make(chan []byte, 4)
	ll, err := lan.Listen(func(pkt []byte, src net.IP) {
		// ReadFrom on a UDP socket returns the raw datagram (no IP/UDP header).
		fmt.Printf("listener got datagram src=%s len=%d content=%q\n", src, len(pkt), pkt)
		got <- append([]byte(nil), pkt...)
	})
	if err != nil {
		fmt.Println("listener:", err)
		return
	}
	defer ll.Close()
	fmt.Println("listener listening (group joined on all interfaces)")

	// Baseline: inject a unicast packet to the adapter's OWN address — that
	// forces local delivery, proving the adapter's Write (receive) path works.
	uni, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 5555})
	if err != nil {
		fmt.Println("unicast listener:", err)
		return
	}
	defer uni.Close()
	// Real scenario: a PEER's packet (src 10.99.0.3) is injected for local
	// delivery (dst = the adapter's own IP 10.99.0.2).
	for _, dst := range [][4]byte{{10, 99, 0, 2}, {10, 99, 0, 5}} {
		p := buildUDP(10, 99, 0, 3, dst[0], dst[1], dst[2], dst[3], 1234, 5555, []byte("unicast"))
		if err := ad.Write(p); err != nil {
			fmt.Printf("inject unicast to %v: %v\n", dst, err)
			continue
		}
		_ = uni.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		buf := make([]byte, 512)
		n, _, err := uni.ReadFrom(buf)
		if err != nil {
			fmt.Printf("UNICAST to %v.%v.%v.%v: NOT received: %v\n", dst[0], dst[1], dst[2], dst[3], err)
		} else {
			fmt.Printf("UNICAST to %v.%v.%v.%v: received: %q\n", dst[0], dst[1], dst[2], dst[3], buf[:n])
		}
	}

	// Multicast: a proper-checksum advertisement from a PEER to the MC group.
	pkt := buildAd(10, 99, 0, 3) // src 10.99.0.3 (a peer), dst 224.0.2.60
	if err := ad.Write(pkt); err != nil {
		fmt.Println("inject:", err)
		return
	}
	fmt.Println("injected", len(pkt), "bytes into adapter")

	select {
	case <-got:
		fmt.Println("RESULT: adapter injection reaches the multicast socket")
	case <-time.After(4 * time.Second):
		fmt.Println("RESULT: adapter injection did NOT reach the multicast socket")
	}

	// Hold the adapter open and keep injecting so a packet capture can see
	// whether the driver accepts the injected packets at all.
	fmt.Println("holding adapter open, re-injecting every 2s (capture with pktmon)...")
	for i := 0; i < 15; i++ {
		pkt := buildAd(10, 99, 0, 3)
		if err := ad.Write(pkt); err != nil {
			fmt.Printf("re-inject: %v\n", err)
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("done")
}

func buildAd(a, b, c, d byte) []byte {
	return buildUDP(a, b, c, d, 224, 0, 2, 60, 4445, 4445, []byte("[MOTD]Injected[/MOTD][AD]25565[/AD]"))
}

// buildUDP builds an IPv4/UDP datagram with a valid IP header checksum.
func buildUDP(sa, sb, sc, sd byte, da, db, dc, dd byte, sport, dport int, payload []byte) []byte {
	total := 20 + 8 + len(payload)
	pkt := make([]byte, total)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	pkt[8] = 64
	pkt[9] = 17
	pkt[12], pkt[13], pkt[14], pkt[15] = sa, sb, sc, sd
	pkt[16], pkt[17], pkt[18], pkt[19] = da, db, dc, dd
	binary.BigEndian.PutUint16(pkt[20:22], uint16(sport))
	binary.BigEndian.PutUint16(pkt[22:24], uint16(dport))
	binary.BigEndian.PutUint16(pkt[24:26], uint16(8+len(payload)))
	copy(pkt[28:], payload)
	// IP header checksum (RFC 1071) over bytes 0..19.
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
