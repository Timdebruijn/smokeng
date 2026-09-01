package probe

import (
	"errors"
	"syscall"

	"github.com/timdebruijn/smokeng/internal/store"
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
	switch {
	case err == nil:
		return store.SendReasonSocket
	case errors.Is(err, syscall.ECONNREFUSED):
		return store.SendReasonRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return store.SendReasonUnreachable
	default:
		return store.SendReasonSocket
	}
}
