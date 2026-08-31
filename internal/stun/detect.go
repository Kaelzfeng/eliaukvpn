package stun

import (
	"fmt"
	"net"
	"time"
)

// NATType classifies the NAT behaviour of the local network.
type NATType string

const (
	NATUnknown        NATType = "unknown"
	NATFullCone       NATType = "full_cone"
	NATRestrictedCone NATType = "restricted_cone" // address- or port-restricted
	NATSymmetric      NATType = "symmetric"
	NATUnreachable    NATType = "unreachable" // STUN could not be reached
)

// Result is the outcome of a NAT detection run.
type Result struct {
	Mapped MappedAddress
	NAT    NATType
}

// Detect probes the NAT using up to two STUN servers:
//
//   - primary:   baseline mapping and the CHANGE-REQUEST (cone) test.
//   - secondary: a different IP used to detect symmetric NAT — the mapped
//     port changing per destination is the definitive symmetric fingerprint.
//
// A symmetric NAT cannot be hole-punched reliably and will need a relay
// (TURN, milestone M3); every cone type can be hole-punched.
func Detect(primary, secondary *net.UDPAddr, timeout time.Duration) (*Result, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("stun: open udp socket: %w", err)
	}
	defer conn.Close()

	// Baseline mapped address from the primary server.
	m1, err := roundTrip(conn, primary, false, false, timeout)
	if err != nil {
		return &Result{NAT: NATUnreachable}, fmt.Errorf("stun: primary %s unreachable: %w", primary, err)
	}
	res := &Result{Mapped: *m1.mapped, NAT: NATUnknown}

	// Symmetry probe: ask a second, different server. If the mapped port
	// changes, the NAT allocates per-destination ports -> symmetric.
	if secondary != nil {
		if m2, err := roundTrip(conn, secondary, false, false, timeout); err == nil {
			if m2.mapped.Port != m1.mapped.Port || !m2.mapped.IP.Equal(m1.mapped.IP) {
				res.NAT = NATSymmetric
				return res, nil
			}
		}
		// Secondary probe failure is not fatal; fall through to the cone test.
	}

	// Cone refinement: ask the primary server to reply from another address.
	// Only a full-cone NAT (or no NAT) lets a packet from an uncontacted
	// address/port back in. If the server ignores CHANGE-REQUEST and answers
	// from its normal address, we can't tell the sub-types apart and report
	// the conservative "restricted" label (still hole-punchable).
	resp, err := roundTrip(conn, primary, true, true, timeout)
	if err != nil {
		res.NAT = NATRestrictedCone
	} else if resp.src.IP.Equal(primary.IP) && resp.src.Port == primary.Port {
		res.NAT = NATRestrictedCone
	} else {
		res.NAT = NATFullCone
	}
	return res, nil
}

// DefaultServers returns two well-known public STUN servers.
func DefaultServers() (primary, secondary *net.UDPAddr) {
	primary, _ = net.ResolveUDPAddr("udp", "stun.l.google.com:19302")
	secondary, _ = net.ResolveUDPAddr("udp", "stun.cloudflare.com:3478")
	return
}
