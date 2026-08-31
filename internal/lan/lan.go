// Package lan implements software-layer LAN discovery emulation for the
// virtual network (M5).
//
// The virtual NIC is Wintun, an L3 adapter with no broadcast or multicast
// support, so anything that relies on a shared LAN segment must be emulated in
// userspace. Minecraft's "Open to LAN" is the motivating case: a hosting
// server periodically advertises its world as a UDP datagram to the multicast
// group 224.0.2.60:4445, and joining clients discover it by listening on that
// group and connecting back to the packet's source address.
//
// Emulation strategy:
//
//   - Host side: this client binds the discovery port (SO_REUSEADDR, group
//     joined), so the hosting player's own MC broadcast is sniffed no matter
//     which interface the OS routed it out of. The packet's IPv4 source is
//     rewritten to our virtual IP — a joiner would otherwise try to connect to
//     the host's real LAN address, which is unreachable over the internet —
//     and the result is fanned out to every connected peer.
//   - Join side: the decapsulated advertisement is written into the virtual
//     NIC (already done by the dataSink), where Windows delivers it to the MC
//     client's multicast socket. As a belt-and-braces fallback the same packet
//     is also looped back onto the discovery group locally, in case adapter
//     injection does not reach joined sockets.
//
// Loop guard: a packet is forwarded only if its source is NOT a virtual-subnet
// address. Forwarded advertisements carry the host's virtual IP, so a peer
// that locally re-emits them never forwards them again.
package lan

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/windows"
)

// Minecraft's LAN discovery endpoint.
const (
	DiscoveryPort  = 4445
	DiscoveryGroup = "224.0.2.60"
)

// Listener receives MC LAN advertisements sent from the local machine so they
// can be re-broadcast through the tunnel, and can re-emit an advertisement
// onto the local discovery group for a joining MC client that has not seen the
// adapter-injected copy.
type Listener struct {
	pc  net.PacketConn
	ip  *ipv4.PacketConn
	onP func(pkt []byte, src net.IP)
}

// Listen binds the discovery port with SO_REUSEADDR and joins the discovery
// multicast group. Every packet received is reported to onPacket. It is safe
// to bind alongside the MC client's own discovery socket.
func Listen(onPacket func(pkt []byte, src net.IP)) (*Listener, error) {
	lc := net.ListenConfig{Control: reuseAddr}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", DiscoveryPort))
	if err != nil {
		return nil, fmt.Errorf("bind udp %d: %w", DiscoveryPort, err)
	}
	p := ipv4.NewPacketConn(pc)
	group := &net.UDPAddr{IP: net.ParseIP(DiscoveryGroup)}
	// Join the group on every up interface — a LAN-world advertisement can
	// arrive on the real NIC (host side) or be injected into the virtual NIC
	// (join side), and membership is interface-scoped. The Wintun adapter does
	// not advertise FlagMulticast but JoinGroup still succeeds on it, which is
	// what lets adapter-injected advertisements reach us; excluding it would
	// silently break the join side.
	joined := 0
	ifs, _ := net.Interfaces()
	for _, iface := range ifs {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if err := p.JoinGroup(&iface, group); err != nil {
			continue
		}
		joined++
	}
	if joined == 0 {
		log.Printf("lan: no interface could join %s; binding alone (unicast delivery may still work)", DiscoveryGroup)
	}
	l := &Listener{pc: pc, ip: p, onP: onPacket}
	go l.readLoop()
	return l, nil
}

// Close stops the listener.
func (l *Listener) Close() { l.pc.Close() }

// LocalEmit sends one advertisement onto the discovery group on the local
// machine, reaching any socket (like the MC client's) that joined it.
func (l *Listener) LocalEmit(pkt []byte) {
	if _, err := l.pc.WriteTo(pkt, &net.UDPAddr{IP: net.ParseIP(DiscoveryGroup), Port: DiscoveryPort}); err != nil {
		return // best-effort; adapter injection may already have delivered it
	}
}

func (l *Listener) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := l.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var src net.IP
		if ua, ok := addr.(*net.UDPAddr); ok {
			src = ua.IP
		}
		l.onP(append([]byte(nil), buf[:n]...), src)
	}
}

// IsDiscovery reports whether an IPv4 packet is a LAN-discovery advertisement:
// a UDP datagram addressed to the discovery port and to a multicast group or a
// (limited) broadcast address.
func IsDiscovery(pkt []byte) bool {
	if UDPDestPort(pkt) != DiscoveryPort {
		return false
	}
	ip := net.ParseIP(IPv4Dst(pkt))
	return ip != nil && (ip.IsMulticast() || ip.Equal(net.IPv4bcast))
}

// IPv4Dst returns the destination address of an IPv4 packet, or "" if it is
// not a well-formed IPv4 datagram.
func IPv4Dst(pkt []byte) string {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return ""
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || ihl > len(pkt) {
		return ""
	}
	return net.IP(pkt[ihl-4 : ihl]).String()
}

// UDPDestPort returns the destination UDP port of an IPv4 UDP packet, or 0.
func UDPDestPort(pkt []byte) int {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return 0
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl+8 > len(pkt) || pkt[9] != 17 /* UDP */ {
		return 0
	}
	return int(pkt[ihl+2])<<8 | int(pkt[ihl+3])
}

// RewriteSource returns a copy of pkt with its IPv4 source address replaced.
func RewriteSource(pkt []byte, src net.IP) []byte {
	return rewriteIPv4(pkt, src, false)
}

// RewriteDest returns a copy of pkt with its IPv4 destination address replaced.
func RewriteDest(pkt []byte, dst net.IP) []byte {
	return rewriteIPv4(pkt, dst, true)
}

func rewriteIPv4(pkt []byte, ip net.IP, dest bool) []byte {
	out := make([]byte, len(pkt))
	copy(out, pkt)
	if len(out) < 20 || out[0]>>4 != 4 {
		return out
	}
	ihl := int(out[0]&0x0f) * 4
	if ihl < 20 || ihl > len(out) {
		return out
	}
	s := ip.To4()
	if s == nil {
		return out
	}
	// Source addr lives at ihl-8..ihl-4, destination at ihl-4..ihl.
	lo := ihl - 8
	if dest {
		lo = ihl - 4
	}
	copy(out[lo:lo+4], s)
	return out
}

// InVirtualSubnet reports whether an IP address is on the virtual subnet
// (10.0.0.0/24) — i.e. either our own or a peer's address, used as the loop
// guard for discovery forwarding.
func InVirtualSubnet(ip net.IP) bool {
	return ip != nil && ip.To4() != nil && ip.To4()[0] == 10 && ip.To4()[1] == 0 && ip.To4()[2] == 0
}

// IPv4Src returns the source address of an IPv4 packet, or "" if it is not a
// well-formed IPv4 datagram.
func IPv4Src(pkt []byte) string {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return ""
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || ihl > len(pkt) {
		return ""
	}
	return net.IP(pkt[ihl-8 : ihl-4]).String()
}

// BuildDiscovery wraps a raw LAN-discovery datagram payload (as delivered by a
// socket ReadFrom — payload only, no IP header) into a complete IPv4/UDP packet
// addressed to the discovery group:port and sourced from src. The host side
// uses this so an advertisement sniffed on the real NIC carries the host's
// virtual IP when it is fanned out through the tunnel; without it a joining MC
// client would try to connect to the host's real LAN address, which is
// unreachable over the internet.
func BuildDiscovery(src net.IP, payload []byte) []byte {
	s := src.To4()
	if s == nil {
		return nil
	}
	group := net.ParseIP(DiscoveryGroup).To4()
	if group == nil {
		return nil
	}
	total := 20 + 8 + len(payload)
	pkt := make([]byte, total)
	pkt[0] = 0x45 // IPv4, IHL=5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	pkt[8] = 64  // TTL
	pkt[9] = 17  // UDP
	copy(pkt[12:16], s)
	copy(pkt[16:20], group)
	// UDP header: source and destination ports are both the discovery port; the
	// MC client connects to the packet's source IP and the [AD]-advertised port,
	// so the source port value is not load-bearing.
	binary.BigEndian.PutUint16(pkt[20:22], uint16(DiscoveryPort))
	binary.BigEndian.PutUint16(pkt[22:24], uint16(DiscoveryPort))
	binary.BigEndian.PutUint16(pkt[24:26], uint16(8+len(payload)))
	// UDP checksum is left 0, which IPv4 treats as "not computed" and Windows
	// accepts (the mcast/injvar probes injected the same way successfully).
	copy(pkt[28:], payload)
	// IP header checksum.
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

func reuseAddr(network, address string, c syscall.RawConn) error {
	var opErr error
	if err := c.Control(func(fd uintptr) {
		opErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return opErr
}
