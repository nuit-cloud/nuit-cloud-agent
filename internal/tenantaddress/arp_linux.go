//go:build linux

package tenantaddress

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"time"
)

// packetARP sends and hears ARP on one interface through a packet socket (AF_PACKET, ETH_P_ARP), bound to that interface so it sees only
// that interface's frames. It needs CAP_NET_RAW, which the agent has as root.
type packetARP struct{}

// htons is v in network byte order, as the packet socket's protocol fields want it.
func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

func open(link Link) (int, error) {
	if link.Index <= 0 {
		return -1, fmt.Errorf("%s has no interface index", link.Name)
	}
	proto := htons(etherTypeARP)
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return -1, fmt.Errorf("could not open a packet socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: proto, Ifindex: link.Index}); err != nil {
		syscall.Close(fd)
		return -1, fmt.Errorf("could not bind a packet socket to %s: %w", link.Name, err)
	}
	return fd, nil
}

func send(fd int, link Link, frame []byte) error {
	to := &syscall.SockaddrLinklayer{Protocol: htons(etherTypeARP), Ifindex: link.Index, Halen: 6}
	copy(to.Addr[:], broadcastMAC)
	if err := syscall.Sendto(fd, frame, 0, to); err != nil {
		return fmt.Errorf("could not send on %s: %w", link.Name, err)
	}
	return nil
}

func (packetARP) Probe(ctx context.Context, link Link, addr netip.Addr, wait time.Duration) (Claim, error) {
	fd, err := open(link)
	if err != nil {
		return Claim{}, err
	}
	defer syscall.Close(fd)
	// short receive timeouts, so the wait and the task's deadline are kept without a poller
	tv := syscall.NsecToTimeval(int64(100 * time.Millisecond))
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return Claim{}, fmt.Errorf("could not set a receive timeout: %w", err)
	}
	if err := send(fd, link, probeFrame(link.MAC, addr)); err != nil {
		return Claim{}, err
	}
	deadline := time.Now().Add(wait)
	buf := make([]byte, 2048)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return Claim{}, err
		}
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return Claim{}, fmt.Errorf("could not listen on %s: %w", link.Name, err)
		}
		if c, ok := claimIn(buf[:n], link.MAC, addr); ok {
			return c, nil
		}
	}
	return Claim{}, nil
}

func (packetARP) Announce(ctx context.Context, link Link, addr netip.Addr) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := open(link)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return send(fd, link, announceFrame(link.MAC, addr))
}
