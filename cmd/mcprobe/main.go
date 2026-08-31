// Command mcprobe verifies the M5 Minecraft LAN-discovery emulation without a
// real Minecraft installation.
//
//   - "server" mode fakes a hosting MC server: it listens on TCP 25565 and
//     periodically advertises its world on UDP 224.0.2.60:4445, exactly like
//     Minecraft's "Open to LAN".
//   - "client" mode fakes a joining MC client: it listens on the discovery
//     group, prints every world advertisement it sees (with the source address
//     a real client would connect to), then tries to open a TCP connection to
//     it.
//
// The eliauk client on the hosting machine sniffs the server's advertisement
// (its local 4445 listener), rewrites the source to its virtual IP and fans it
// out through the tunnel; the joining machine's eliauk client injects it into
// the virtual NIC. If mcprobe client sees a world at the host's virtual IP,
// the full discovery path works.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/ipv4"

	"eliaukvpn/internal/lan"
)

const motdFmt = "[MOTD]Eliauk Test World[/MOTD][AD]%d[/AD]"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	mode := flag.String("mode", "", "server | client")
	port := flag.Int("port", 25565, "fake MC server port (server mode)")
	flag.Parse()

	switch *mode {
	case "server":
		return runServer(*port)
	case "client":
		return runClient()
	default:
		return fmt.Errorf("-mode must be 'server' or 'client'")
	}
}

// runServer fakes a hosting Minecraft server.
func runServer(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return fmt.Errorf("listen tcp %d: %w", port, err)
	}
	defer ln.Close()
	fmt.Printf("fake MC server : listening TCP :%d\n", port)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Printf("fake MC server : TCP accepted from %s (this works when the tunnel routes the join)\n", c.RemoteAddr())
			c.Close()
		}
	}()

	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	defer conn.Close()
	p := ipv4.NewPacketConn(conn)
	_ = p.SetMulticastTTL(4)
	payload := []byte(fmt.Sprintf(motdFmt, port))
	dst := &net.UDPAddr{IP: net.ParseIP(lan.DiscoveryGroup), Port: lan.DiscoveryPort}

	fmt.Printf("fake MC server : advertising %s on %s:%d every 1.5s\n",
		payload, lan.DiscoveryGroup, lan.DiscoveryPort)
	for {
		if _, err := p.WriteTo(payload, nil, dst); err != nil {
			fmt.Printf("fake MC server : broadcast: %v\n", err)
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

// runClient fakes a joining Minecraft client: it listens on the discovery
// group and connects to any world it sees. Only advertisements sourced from the
// virtual subnet are reported — a local MC server on the same machine would
// otherwise deliver its broadcast directly over the real NIC (same-machine
// multicast loopback) and mask the tunnel path the test is meant to prove.
func runClient() error {
	seen := map[string]bool{}
	ll, err := lan.Listen(func(pkt []byte, src net.IP) {
		msg := string(pkt)
		adPort := parseAD(msg)
		if adPort == 0 {
			return
		}
		if !lan.InVirtualSubnet(src) {
			fmt.Printf("fake MC client : ignored non-virtual advertisement from %s (direct local broadcast)\n", src)
			return
		}
		key := fmt.Sprintf("%s:%d", src, adPort)
		if seen[key] {
			return
		}
		seen[key] = true
		world := strings.TrimSuffix(strings.TrimPrefix(msg, "[MOTD]"), "[/MOTD]")
		fmt.Printf("fake MC client : discovered world %q at %s (port %d)\n", world, src, adPort)

		// A real client would now connect to src:port over TCP.
		go func() {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", src, adPort), 3*time.Second)
			if err != nil {
				fmt.Printf("fake MC client : TCP connect to %s:%d failed: %v\n", src, adPort, err)
				return
			}
			fmt.Printf("fake MC client : TCP connect to %s:%d OK\n", src, adPort)
			conn.Close()
		}()
	})
	if err != nil {
		return err
	}
	defer ll.Close()
	fmt.Printf("fake MC client : listening on UDP %d/%s\n", lan.DiscoveryPort, lan.DiscoveryGroup)
	select {}
}

// parseAD extracts the advertised server port from a LAN MOTD payload.
func parseAD(msg string) int {
	i := strings.Index(msg, "[AD]")
	j := strings.Index(msg, "[/AD]")
	if i < 0 || j < 0 || j < i {
		return 0
	}
	p, err := strconv.Atoi(msg[i+4 : j])
	if err != nil {
		return 0
	}
	return p
}
