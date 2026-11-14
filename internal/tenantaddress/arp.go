package tenantaddress

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
)

// The ARP packets a take sends, as whole Ethernet frames (RFC 826, RFC 5227), and the test for an answer that claims the address.
const (
	etherTypeARP = 0x0806
	etherTypeIP  = 0x0800
	arpRequest   = 1
	arpReply     = 2
	// frameLen is the shortest Ethernet frame without its checksum: an ARP packet is padded to it.
	frameLen = 60
	// arpLen is an Ethernet header and an ARP packet for IPv4 over Ethernet.
	arpLen = 14 + 28
)

var (
	broadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	zeroMAC      = net.HardwareAddr{0, 0, 0, 0, 0, 0}
)

// arpFrame is a broadcast Ethernet frame from src carrying one ARP packet for IPv4.
func arpFrame(src net.HardwareAddr, op uint16, sha net.HardwareAddr, spa netip.Addr, tha net.HardwareAddr, tpa netip.Addr) []byte {
	f := make([]byte, frameLen)
	copy(f[0:6], broadcastMAC)
	copy(f[6:12], src)
	binary.BigEndian.PutUint16(f[12:14], etherTypeARP)
	a := f[14:]
	binary.BigEndian.PutUint16(a[0:2], 1) // Ethernet
	binary.BigEndian.PutUint16(a[2:4], etherTypeIP)
	a[4], a[5] = 6, 4
	binary.BigEndian.PutUint16(a[6:8], op)
	copy(a[8:14], sha)
	s := spa.As4()
	copy(a[14:18], s[:])
	copy(a[18:24], tha)
	t := tpa.As4()
	copy(a[24:28], t[:])
	return f
}

// probeFrame is an RFC 5227 ARP probe for addr: a broadcast request with sender IP 0.0.0.0 and the target hardware address zero, so no
// machine's ARP cache learns anything from it.
func probeFrame(mac net.HardwareAddr, addr netip.Addr) []byte {
	return arpFrame(mac, arpRequest, mac, netip.IPv4Unspecified(), zeroMAC, addr)
}

// announceFrame is an RFC 5227 ARP announcement (a gratuitous ARP) for addr: a broadcast request with sender and target IP both addr, so
// every machine that has addr in its ARP cache (the router) updates it to mac.
func announceFrame(mac net.HardwareAddr, addr netip.Addr) []byte {
	return arpFrame(mac, arpRequest, mac, addr, zeroMAC, addr)
}

// claimIn reads a received frame: a Claim when another machine (a hardware address that is not own) answers for addr (an ARP request or
// reply with addr as its sender IP), or is itself probing for addr (a request with sender IP 0.0.0.0 and target addr).
func claimIn(frame []byte, own net.HardwareAddr, addr netip.Addr) (Claim, bool) {
	if len(frame) < arpLen || binary.BigEndian.Uint16(frame[12:14]) != etherTypeARP {
		return Claim{}, false
	}
	a := frame[14:]
	if binary.BigEndian.Uint16(a[0:2]) != 1 || binary.BigEndian.Uint16(a[2:4]) != etherTypeIP || a[4] != 6 || a[5] != 4 {
		return Claim{}, false
	}
	op := binary.BigEndian.Uint16(a[6:8])
	if op != arpRequest && op != arpReply {
		return Claim{}, false
	}
	sha := net.HardwareAddr(append([]byte(nil), a[8:14]...))
	if bytes.Equal(sha, own) {
		return Claim{}, false
	}
	spa := netip.AddrFrom4([4]byte(a[14:18]))
	tpa := netip.AddrFrom4([4]byte(a[24:28]))
	switch {
	case spa == addr:
		return Claim{MAC: sha}, true
	case op == arpRequest && spa == netip.IPv4Unspecified() && tpa == addr:
		return Claim{MAC: sha, Probing: true}, true
	}
	return Claim{}, false
}
