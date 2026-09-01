package probe

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/heistp/irtt"

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
	return sendReasonOr(err, store.SendReasonSocket)
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
