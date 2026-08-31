// Command injvar isolates what triggers tcpip.sys's "Not locally destined" drop
// for wintun-injected packets. It waits for the adapter address to be ready,
// then injects unicast variants to the wintun IP, the real LAN IP and loopback,
// with several source addresses, and reports which are delivered.
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"eliaukvpn/internal/vnic"
)

func main() {
	ad, err := vnic.Open("Eliauk-injvar", "10.99.0.2", "255.255.255.0")
	if err != nil {
		fmt.Println("adapter:", err)
		return
	}
	defer ad.Close()
	fmt.Println("adapter open")

	// Wait for the address to be bindable.
	for i := 0; i < 20; i++ {
		c, berr := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("10.99.0.2"), Port: 0})
		if berr == nil {
			c.Close()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("10.99.0.2 is bindable — running variants")

	any, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 5555})
	if err != nil {
		fmt.Println("bind 0.0.0.0:5555:", err)
		return
	}
	defer any.Close()

	peer := [4]byte{10, 99, 0, 3}
	external := [4]byte{1, 2, 3, 4}
	self := [4]byte{10, 99, 0, 2}
	own := [4]byte{10, 99, 0, 2}
	lan := [4]byte{192, 168, 0, 100}
	loopback := [4]byte{127, 0, 0, 1}
	bcast := [4]byte{10, 99, 0, 255}

	tests := []struct {
		name string
		src  [4]byte
		dst  [4]byte
	}{
		{"peer -> own wintun IP", peer, own},
		{"external -> own wintun IP", external, own},
		{"own -> own wintun IP", self, own},
		{"peer -> real LAN IP", peer, lan},
		{"external -> real LAN IP", external, lan},
		{"peer -> loopback", peer, loopback},
		{"peer -> 10.99.0.255 broadcast", peer, bcast},
	}
	for _, t := range tests {
		pkt := buildUDP(t.src, t.dst, 1234, 5555, []byte("X"))
		if err := ad.Write(pkt); err != nil {
			fmt.Printf("%-32s : inject error %v\n", t.name, err)
			continue
		}
		if probe(any, 900*time.Millisecond) {
			fmt.Printf("%-32s : DELIVERED\n", t.name)
		} else {
			fmt.Printf("%-32s : NOT delivered\n", t.name)
		}
	}
	fmt.Println("done")
}

func probe(c *net.UDPConn, d time.Duration) bool {
	buf := make([]byte, 512)
	_ = c.SetReadDeadline(time.Now().Add(d))
	n, _, err := c.ReadFromUDP(buf)
	return err == nil && n > 0
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
