//go:build !linux

package tenantaddress

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

// packetARP needs a Linux packet socket; elsewhere (where the agent is only built for its tests) it refuses.
type packetARP struct{}

var errNoPacketSocket = errors.New("ARP probes and announcements need Linux")

func (packetARP) Probe(context.Context, Link, netip.Addr, time.Duration) (Claim, error) {
	return Claim{}, errNoPacketSocket
}

func (packetARP) Announce(context.Context, Link, netip.Addr) error { return errNoPacketSocket }
