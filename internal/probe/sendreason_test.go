package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/heistp/irtt"

	"github.com/timdebruijn/smokeng/internal/store"
)

// The classifier is the whole point of recording a reason, and it had no test:
// replacing its body with a constant left the suite green.
//
// The errors are wrapped the way the net stack really wraps them — a bare
// syscall.Errno would pass a check that errors.Is on a *net.OpError would not.
func TestSendReasonClassification(t *testing.T) {
	wrapOp := func(e error) error {
		return &net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("sendto", e)}
	}
	cases := []struct {
		name string
		err  error
		want uint8
	}{
		{"refused, wrapped as the net stack does", wrapOp(syscall.ECONNREFUSED), store.SendReasonRefused},
		{"host unreachable", wrapOp(syscall.EHOSTUNREACH), store.SendReasonUnreachable},
		{"net unreachable", wrapOp(syscall.ENETUNREACH), store.SendReasonUnreachable},
		{"our own deadline", context.DeadlineExceeded, store.SendReasonDeadline},
		{"a socket deadline", os.ErrDeadlineExceeded, store.SendReasonDeadline},
		{"something else", errors.New("no idea"), store.SendReasonSocket},
		{"nothing", nil, store.SendReasonSocket},
		// Wrapped a second time, as an error passing through another layer.
		{"doubly wrapped refusal", fmt.Errorf("session: %w", wrapOp(syscall.ECONNREFUSED)),
			store.SendReasonRefused},
	}
	for _, c := range cases {
		if got := sendReasonFor(c.err); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name,
				store.SendReasonName(got), store.SendReasonName(c.want))
		}
	}
}

// A session probe knows more about its own failure than "the socket refused
// the write", and the fallback is where that knowledge lives.
func TestSendReasonFallbacks(t *testing.T) {
	other := errors.New("some other failure")
	if got := sendReasonOr(other, store.SendReasonSessionEnded); got != store.SendReasonSessionEnded {
		t.Errorf("unrecognised mid-session error = %q, want the session fallback",
			store.SendReasonName(got))
	}
	// A recognised error still wins over the fallback.
	if got := sendReasonOr(syscall.ECONNREFUSED, store.SendReasonSessionEnded); got != store.SendReasonRefused {
		t.Errorf("a refusal mid-session = %q, want refused", store.SendReasonName(got))
	}
}

// An open that timed out is silence, not refusal.
func TestSendReasonForOpen(t *testing.T) {
	timeout := irtt.Errorf(irtt.OpenTimeout, "no reply from server")
	if got := sendReasonForOpen(timeout); got != store.SendReasonSessionNoReply {
		t.Errorf("an open timeout = %q, want no reply", store.SendReasonName(got))
	}
	if got := sendReasonForOpen(context.DeadlineExceeded); got != store.SendReasonDeadline {
		t.Errorf("our own deadline = %q, want deadline", store.SendReasonName(got))
	}
	if got := sendReasonForOpen(syscall.ECONNREFUSED); got != store.SendReasonRefused {
		t.Errorf("a refused open = %q, want refused", store.SendReasonName(got))
	}
	// Only an unplaceable error falls back to calling it a refusal.
	if got := sendReasonForOpen(errors.New("?")); got != store.SendReasonSessionRefused {
		t.Errorf("an unplaceable open failure = %q", store.SendReasonName(got))
	}
}
