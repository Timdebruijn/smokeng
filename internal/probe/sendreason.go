package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"

	"github.com/heistp/irtt"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// sendReasonFor classifies a failed send.
//
// The distinction that pays for this function is ECONNREFUSED. On a connected
// UDP socket it is not a local fault at all: it means an earlier packet drew an
// ICMP unreachable, so the far end — or a router speaking for it — refused the
// traffic, and the kernel reports that on the *next* send. A remote condition
// arriving as a local error is precisely why "the send failed" on its own was
// not enough to tell a network fault from a bug in this prober.
func sendReasonFor(err error) uint8 {
	return sendReasonOr(err, store.SendReasonSocket)
}

// noRouteTo asks this host's routing table whether it has a route to addr, by
// connecting a UDP socket of the family to it and closing it again: connect on
// UDP sends nothing, and fails with ENETUNREACH exactly when there is no route.
// Any other outcome, including any other error, is "go and probe", so a check
// that cannot be made never hides an interval.
func noRouteTo(addr netip.Addr, family string) bool {
	network := "udp4"
	if family == "v6" {
		network = "udp6"
	}
	c, err := net.DialUDP(network, nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr.Unmap(), 9)))
	if err == nil {
		c.Close()
		return false
	}
	return errors.Is(err, syscall.ENETUNREACH)
}

// noDataRow is an interval in which the prober could not ask the target
// anything, for a reason that lies with its own host. Sent is 0, which is what
// tells it from "N probes attempted, all failed locally": a reader treats it as
// no data, neither loss nor a clean interval. The flag and reason say why.
func noDataRow(spec TargetSpec, bucket int64, reason uint8) store.Measurement {
	return store.Measurement{
		TargetID: spec.TargetID,
		AgentID:  store.LocalAgentID,
		Family:   tree.FamilyOf(spec.Family),
		TS:       bucket,
		Flags:    store.FlagSendFailed,
		SendErr:  &reason,
	}
}

// sendReasonOr is sendReasonFor with a caller-chosen answer for an error it
// cannot place. A session probe knows more about its own failure than "the
// socket refused the write", and saying the generic thing throws that away.
func sendReasonOr(err error, fallback uint8) uint8 {
	switch {
	case err == nil:
		return fallback
	case errors.Is(err, syscall.ECONNREFUSED):
		return store.SendReasonRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return store.SendReasonUnreachable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		// The bucket's own deadline, which this prober set. Nothing was asked
		// of the target.
		return store.SendReasonDeadline
	default:
		return fallback
	}
}

// sendReasonForOpen classifies a session that never opened.
//
// It exists because recording every one of those as "refused" put this
// prober's own timing under the far end's name — the exact confusion the
// reason field was added to end, reintroduced by the code that added it. A
// black-holed address answers nothing; a deadline this prober chose too tight
// expires; neither is a refusal.
func sendReasonForOpen(err error) uint8 {
	var ie *irtt.Error
	if errors.As(err, &ie) && ie.Code == irtt.OpenTimeout {
		return store.SendReasonSessionNoReply
	}
	return sendReasonOr(err, store.SendReasonSessionRefused)
}
