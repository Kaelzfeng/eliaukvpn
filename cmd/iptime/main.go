// Command iptime tests the hypothesis that wintun-injected packets are dropped
// ("Not locally destined") only because they arrive before the adapter's IPv4
// address becomes usable. It creates the adapter, then injects a unicast packet
// to 10.99.0.2:5555 once per second for 15 seconds, also checking whether
// 10.99.0.2 is bindable at each step.
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"eliaukvpn/internal/vnic"
)

func main() {
	ad, err := vnic.Open("Eliauk-iptime", "10.99.0.2", "255.255.255.0")
	if err != nil {
		fmt.Println("adapter:", err)
		return
	}
	defer ad.Close()
	fmt.Println("adapter open")

	any, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 5555})
	if err != nil {
		fmt.Println("bind 0.0.0.0:5555:", err)
		return
	}
	defer any.Close()

	peer := [4]byte{10, 99, 0, 3}
	own := [4]byte{10, 99, 0, 2}

	// Immediate injection right after Open.
	_ = ad.Write(buildUDP(peer, own, 1234, 5555, []byte("early")))
	fmt.Printf("t=0s  (right after open) : %s\n", probe(any, 400*time.Millisecond))

	// Then once per second for 15s.
	for i := 1; i <= 15; i++ {
		c, berr := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("10.99.0.2"), Port: 0})
		bindable := berr == nil
		if bindable {
			c.Close()
		}
		_ = ad.Write(buildUDP(peer, own, 1234, 5555, []byte("tick")))
		fmt.Printf("t=%2ds bind10.99.0.2=%-5v : %s\n", i, bindable, probe(any, 400*time.Millisecond))
		time.Sleep(1 * time.Second)
	}
	fmt.Println("done")
}

func probe(c *net.UDPConn, d time.Duration) string {
	buf := make([]byte, 512)
	_ = c.SetReadDeadline(time.Now().Add(d))
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		return "NOT delivered"
	}
	return fmt.Sprintf("DELIVERED %q", buf[:n])
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
