// Command twonet tests the M5b same-machine topology that avoids the
// shared-/24 ambiguity: two wintun adapters on DIFFERENT subnets, with a /32
// host route from each adapter to the other's virtual IP (the per-peer route
// each client would install). It verifies both directions deliver outbound
// unicast into the correct adapter's read loop.
//
//	A = 10.99.0.2/24   B = 10.98.0.3/24
//	B -> 10.99.0.2 via B's adapter (B's client dialing the host's VIP)
//	A -> 10.98.0.3 via A's adapter (host's server replying to the joiner)
package main

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"eliaukvpn/internal/vnic"
)

func main() {
	a, err := vnic.Open("Eliauk-twonetA", "10.99.0.2", "255.255.255.0")
	if err != nil {
		fmt.Println("adapter A:", err)
		return
	}
	defer a.Close()
	b, err := vnic.Open("Eliauk-twonetB", "10.99.0.3", "255.255.255.0")
	if err != nil {
		fmt.Println("adapter B:", err)
		return
	}
	defer b.Close()
	fmt.Println("both adapters open")

	idxA := ifIndex("Eliauk-twonetA")
	idxB := ifIndex("Eliauk-twonetB")
	fmt.Printf("ifIndex A=%d B=%d\n", idxA, idxB)

	seenA := make(chan []byte, 256)
	seenB := make(chan []byte, 256)
	go readLoop(a, seenA)
	go readLoop(b, seenB)

	// Same subnet, but per-peer /32 routes to the peer's OWN local IP:
	// B -> 10.99.0.2 (A's local IP) via B; A -> 10.99.0.3 (B's local IP) via A.
	addRoute("10.99.0.2", idxB)
	addRoute("10.99.0.3", idxA)
	defer exec.Command("route", "delete", "10.99.0.2").Run()
	defer exec.Command("route", "delete", "10.99.0.3").Run()

	fmt.Println("--- direction B->A: dial 10.99.0.2:4444 (A's local IP), expect B's read loop ---")
	genB, err := net.Dial("udp4", "10.99.0.2:4444")
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	gotB := pump(seenB, genB, "toA")
	genB.Close()
	fmt.Printf("B->A: adapter B read %d of our packets (dst 10.99.0.2)\n", gotB)

	fmt.Println("--- direction A->B: dial 10.99.0.3:4444 (B's local IP), expect A's read loop ---")
	genA, err := net.Dial("udp4", "10.99.0.3:4444")
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	gotA := pump(seenA, genA, "toB")
	genA.Close()
	fmt.Printf("A->B: adapter A read %d of our packets (dst 10.99.0.3)\n", gotA)

	if gotB > 0 && gotA > 0 {
		fmt.Println("VERDICT: same-subnet + /32-to-local-VIP delivers both directions — M5b topology WORKS")
	} else if gotB > 0 {
		fmt.Println("VERDICT: B->A works; A->B broken (likely same-machine local-delivery artifact)")
	} else {
		fmt.Println("VERDICT: still broken — investigate further")
	}
}

func addRoute(ip string, idx int) {
	out, err := exec.Command("route", "add", ip, "mask", "255.255.255.255", "0.0.0.0", "IF", fmt.Sprint(idx)).CombinedOutput()
	fmt.Printf("route add %s via if%d: %v %q\n", ip, idx, err, strings.TrimSpace(string(out)))
}

// pump sends the payload every 300ms for up to 8s, counting matching reads.
func pump(seen chan []byte, gen net.Conn, payload string) int {
	deadline := time.Now().Add(8 * time.Second)
	count := 0
	last := time.Now()
	for time.Now().Before(deadline) {
		select {
		case pkt := <-seen:
			if len(pkt) >= 28 && string(pkt[28:]) == payload {
				fmt.Printf("  read: %d bytes %s > %s payload %q\n", len(pkt),
					net.IP(pkt[12:16]), net.IP(pkt[16:20]), pkt[28:])
				count++
			}
		default:
		}
		_, _ = gen.Write([]byte(payload))
		if time.Since(last) > 3*time.Second {
			fmt.Printf("  ...waiting (%d so far)\n", count)
			last = time.Now()
		}
		time.Sleep(300 * time.Millisecond)
		if count >= 3 {
			break
		}
	}
	return count
}

func readLoop(ad *vnic.Adapter, ch chan []byte) {
	for {
		pkt, err := ad.Read()
		if err != nil {
			return
		}
		ad.Release(pkt)
		select {
		case ch <- append([]byte(nil), pkt...):
		default:
		}
	}
}

func ifIndex(name string) int {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		fmt.Println("interface lookup:", err)
		return 0
	}
	return iface.Index
}
