package lan

import (
	"encoding/binary"
	"net"
	"testing"
)

// udp4Packet builds a raw IPv4/UDP datagram.
func udp4Packet(src, dst net.IP, srcPort, dstPort int, payload []byte) []byte {
	total := 20 + 8 + len(payload)
	pkt := make([]byte, total)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	pkt[8] = 64 // TTL
	pkt[9] = 17 // UDP
	s, d := src.To4(), dst.To4()
	copy(pkt[12:16], s)
	copy(pkt[16:20], d)
	// UDP header
	binary.BigEndian.PutUint16(pkt[20:22], uint16(srcPort))
	binary.BigEndian.PutUint16(pkt[22:24], uint16(dstPort))
	binary.BigEndian.PutUint16(pkt[24:26], uint16(8+len(payload)))
	copy(pkt[28:], payload)
	return pkt
}

func TestIsDiscovery(t *testing.T) {
	mcAd := udp4Packet(net.IPv4(192, 168, 1, 5), net.IPv4(224, 0, 2, 60), 55555, DiscoveryPort, []byte("[MOTD]hello[/MOTD]"))
	if !IsDiscovery(mcAd) {
		t.Fatal("MC multicast advertisement should be discovery")
	}
	bcast := udp4Packet(net.IPv4(192, 168, 1, 5), net.IPv4(255, 255, 255, 255), 55555, DiscoveryPort, []byte("x"))
	if !IsDiscovery(bcast) {
		t.Fatal("limited broadcast to 4445 should be discovery")
	}
	// Ordinary unicast traffic must not be misclassified.
	unicast := udp4Packet(net.IPv4(10, 0, 0, 8), net.IPv4(10, 0, 0, 7), 1234, 25565, []byte("data"))
	if IsDiscovery(unicast) {
		t.Fatal("unicast UDP must not be discovery")
	}
	// 4445 to a unicast address is not a LAN advertisement either.
	unicast4445 := udp4Packet(net.IPv4(10, 0, 0, 8), net.IPv4(10, 0, 0, 7), 1234, DiscoveryPort, []byte("x"))
	if IsDiscovery(unicast4445) {
		t.Fatal("unicast to 4445 must not be discovery")
	}
	// Wrong protocol (TCP on 4445) is not discovery.
	tcp := udp4Packet(net.IPv4(10, 0, 0, 8), net.IPv4(10, 0, 0, 7), 1234, DiscoveryPort, nil)
	tcp[9] = 6
	if IsDiscovery(tcp) {
		t.Fatal("TCP must not be discovery")
	}
}

func TestIPv4DstAndUDPPort(t *testing.T) {
	pkt := udp4Packet(net.IPv4(10, 0, 0, 8), net.IPv4(10, 0, 0, 7), 4000, 5000, []byte("ab"))
	if got := IPv4Dst(pkt); got != "10.0.0.7" {
		t.Fatalf("IPv4Dst = %q, want 10.0.0.7", got)
	}
	if got := UDPDestPort(pkt); got != 5000 {
		t.Fatalf("UDPDestPort = %d, want 5000", got)
	}
	if got := UDPDestPort([]byte{0x45, 0x00, 0x00, 0x14}); got != 0 {
		t.Fatalf("UDPDestPort on truncated packet = %d, want 0", got)
	}
}

func TestRewriteSource(t *testing.T) {
	orig := udp4Packet(net.IPv4(192, 168, 1, 5), net.IPv4(224, 0, 2, 60), 55555, DiscoveryPort, []byte("m"))
	out := RewriteSource(orig, net.IPv4(10, 0, 0, 7))
	if IPv4Dst(out) != "224.0.2.60" {
		t.Fatal("rewrite must not touch the destination")
	}
	// Source lives 8 bytes before the destination in the IPv4 header.
	if got := net.IP(out[12:16]).String(); got != "10.0.0.7" {
		t.Fatalf("source after rewrite = %q, want 10.0.0.7", got)
	}
	if string(out[20:]) != string(orig[20:]) {
		t.Fatal("rewrite must not touch UDP header or payload")
	}
	if string(out) == string(orig) {
		t.Fatal("rewrite must actually change the packet")
	}
}

func TestInVirtualSubnet(t *testing.T) {
	if !InVirtualSubnet(net.ParseIP("10.0.0.7")) {
		t.Fatal("10.0.0.7 is in the virtual subnet")
	}
	if InVirtualSubnet(net.ParseIP("192.168.1.5")) {
		t.Fatal("192.168.1.5 is not in the virtual subnet")
	}
	if InVirtualSubnet(nil) {
		t.Fatal("nil is not in the virtual subnet")
	}
}
