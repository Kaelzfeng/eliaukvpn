// Command readpath verifies the OS->adapter read path once the wintun adapter's
// address is ready: it sends UDP to a non-local on-link address (10.99.0.99)
// and reports whether that exact packet reaches ReceivePacket. It holds the
// adapter for 60s so routing/ARP state can be inspected externally. Only a
// packet whose dst is 10.99.0.99 carrying the "readpath" payload counts as our
// packet (mDNS/IGMP traffic routed into the adapter is logged but not counted).
// Exits via os.Exit to avoid the read-loop/Close race.
package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"time"

	"eliaukvpn/internal/vnic"
)

func main() {
	ad, err := vnic.Open("Eliauk-readpath", "10.99.0.2", "255.255.255.0")
	if err != nil {
		fmt.Println("adapter:", err)
		return
	}
	defer ad.Close()

	for i := 0; i < 20; i++ {
		c, berr := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("10.99.0.2"), Port: 0})
		if berr == nil {
			c.Close()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("address ready — sending UDP to 10.99.0.99:4444 for 60s")

	readSeen := make(chan []byte, 256)
	go func() {
		for {
			pkt, err := ad.Read()
			if err != nil {
				return
			}
			ad.Release(pkt)
			select {
			case readSeen <- append([]byte(nil), pkt...):
			default:
			}
		}
	}()

	gen, err := net.Dial("udp4", "10.99.0.99:4444")
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	defer gen.Close()

	deadline := time.Now().Add(45 * time.Second)
	nextSend := time.Now()
	lastLog := time.Now()
	count := 0
	for time.Now().Before(deadline) {
		select {
		case pkt := <-readSeen:
			src := net.IP(pkt[12:16]).String()
			dst := net.IP(pkt[16:20]).String()
			proto := pkt[9]
			label := ""
			if proto == 17 && len(pkt) >= 28 {
				payload := pkt[28:]
				if bytes.Contains(payload, []byte("readpath")) && dst == "10.99.0.99" {
					label = " *** THIS IS OUR PACKET"
				}
			}
			fmt.Printf("read: %d bytes %s > %s proto %d%s\n", len(pkt), src, dst, proto, label)
			if label != "" {
				count++
				fmt.Printf("  (our packet #%d — continuing hold)\n", count)
			}
		default:
		}
		if time.Now().After(nextSend) {
			_, _ = gen.Write([]byte("readpath"))
			nextSend = nextSend.Add(500 * time.Millisecond)
		}
		if time.Since(lastLog) > 10*time.Second {
			fmt.Printf("still waiting, %d of our packets so far\n", count)
			lastLog = time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("READ PATH: %d unicast packets reached the adapter in 45s\n", count)
	if count > 0 {
		os.Exit(0)
	}
	os.Exit(1)
}
