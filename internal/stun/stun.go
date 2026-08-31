// Package stun implements a minimal STUN (RFC 5389) client used to discover
// the public endpoint of a machine sitting behind a NAT.
package stun

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// magicCookie is fixed by RFC 5389.
const magicCookie = 0x2112A442

// STUN attribute types we care about.
const (
	attrChangeRequest    = 0x0003
	attrXORMappedAddress = 0x0020
)

// MappedAddress is the public (IP, port) the STUN server observed.
type MappedAddress struct {
	IP   net.IP
	Port int
}

func (m MappedAddress) String() string {
	if m.IP == nil {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d", m.IP, m.Port)
}

// response pairs a parsed mapped address with the source address the reply
// actually came from (used to tell whether a server honoured CHANGE-REQUEST).
type response struct {
	mapped *MappedAddress
	src    *net.UDPAddr
}

// NewTransactionID returns a random 96-bit STUN transaction ID.
func NewTransactionID() ([12]byte, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("stun: generate transaction id: %w", err)
	}
	return id, nil
}

// buildBindingRequest builds an RFC 5389 Binding Request. Setting
// changeIP/changePort adds a CHANGE-REQUEST attribute asking the server to
// reply from a different address/port (used for NAT-type probing).
func buildBindingRequest(txID [12]byte, changeIP, changePort bool) []byte {
	var body []byte
	if changeIP || changePort {
		attr := make([]byte, 8)
		binary.BigEndian.PutUint16(attr[0:2], attrChangeRequest)
		binary.BigEndian.PutUint16(attr[2:4], 4) // attribute length
		var flags uint32
		if changeIP {
			flags |= 0x04
		}
		if changePort {
			flags |= 0x02
		}
		binary.BigEndian.PutUint32(attr[4:8], flags)
		body = append(body, attr...)
	}

	hdr := make([]byte, 20)
	binary.BigEndian.PutUint16(hdr[0:2], 0x0001) // Binding Request
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(body)))
	binary.BigEndian.PutUint32(hdr[4:8], magicCookie)
	copy(hdr[8:20], txID[:])
	return append(hdr, body...)
}

// parseXORMappedAddress extracts the XOR-MAPPED-ADDRESS attribute from a
// Binding Response. The XOR key is the magic cookie for IPv4 and the
// transaction ID for IPv6.
func parseXORMappedAddress(data []byte, txID [12]byte) (*MappedAddress, error) {
	if len(data) < 20 {
		return nil, errors.New("stun: response too short")
	}
	if mtype := binary.BigEndian.Uint16(data[0:2]); mtype == 0x0111 {
		return nil, errors.New("stun: binding error response")
	}
	if binary.BigEndian.Uint32(data[4:8]) != magicCookie {
		return nil, errors.New("stun: bad magic cookie")
	}
	var respTX [12]byte
	copy(respTX[:], data[8:20])
	if respTX != txID {
		return nil, errors.New("stun: transaction id mismatch")
	}

	ln := int(binary.BigEndian.Uint16(data[2:4]))
	if 20+ln > len(data) {
		return nil, errors.New("stun: truncated body")
	}
	attrs := data[20 : 20+ln]
	for len(attrs) >= 4 {
		atype := binary.BigEndian.Uint16(attrs[0:2])
		alen := int(binary.BigEndian.Uint16(attrs[2:4]))
		if len(attrs) < 4+alen {
			break
		}
		value := attrs[4 : 4+alen]
		if atype == attrXORMappedAddress {
			if alen < 8 {
				return nil, errors.New("stun: short XOR-MAPPED-ADDRESS")
			}
			family := value[1]
			xport := binary.BigEndian.Uint16(value[2:4])
			port := int(xport ^ uint16(magicCookie>>16))
			switch family {
			case 0x01: // IPv4
				xip := binary.BigEndian.Uint32(value[4:8])
				ip := make(net.IP, 4)
				binary.BigEndian.PutUint32(ip, xip^magicCookie)
				return &MappedAddress{IP: ip, Port: port}, nil
			case 0x02: // IPv6
				if alen < 20 {
					continue
				}
				ip := make(net.IP, 16)
				xor := append([]byte{0, 0, 0, 0}, txID[:]...)
				for i := 0; i < 16; i++ {
					ip[i] = value[4+i] ^ xor[i]
				}
				return &MappedAddress{IP: ip, Port: port}, nil
			}
		}
		attrs = attrs[4+alen:]
	}
	return nil, errors.New("stun: no XOR-MAPPED-ADDRESS in response")
}

// roundTrip sends one binding request and waits for the matching response.
func roundTrip(conn *net.UDPConn, server *net.UDPAddr, changeIP, changePort bool, timeout time.Duration) (*response, error) {
	txID, err := NewTransactionID()
	if err != nil {
		return nil, err
	}
	// Never leave a read deadline behind. The socket we probe on is the same
	// socket the p2p tunnel later reads from, and a stale deadline would make
	// Run() exit with an i/o timeout the first time it blocks. Clear it on
	// every exit path.
	defer conn.SetReadDeadline(time.Time{})
	if _, err := conn.WriteToUDP(buildBindingRequest(txID, changeIP, changePort), server); err != nil {
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	buf := make([]byte, 1024)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, err
		}
		// Ignore responses for other transactions and keep reading.
		if ma, err := parseXORMappedAddress(buf[:n], txID); err == nil {
			return &response{mapped: ma, src: src}, nil
		}
	}
}
