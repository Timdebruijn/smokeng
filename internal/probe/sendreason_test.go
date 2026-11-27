package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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
			t.Errorf("%s: got reason %d, want %d", c.name, got, c.want)
		}
	}
}

// A session probe knows more about its own failure than "the socket refused
// the write", and the fallback is where that knowledge lives.
func TestSendReasonFallbacks(t *testing.T) {
	other := errors.New("some other failure")
	if got := sendReasonOr(other, store.SendReasonSessionEnded); got != store.SendReasonSessionEnded {
		t.Errorf("unrecognised mid-session error = reason %d, want the session fallback", got)
	}
	// A recognised error still wins over the fallback.
	if got := sendReasonOr(syscall.ECONNREFUSED, store.SendReasonSessionEnded); got != store.SendReasonRefused {
		t.Errorf("a refusal mid-session = reason %d, want refused", got)
	}
}

// An open that timed out is silence, not refusal.
func TestSendReasonForOpen(t *testing.T) {
	timeout := irtt.Errorf(irtt.OpenTimeout, "no reply from server")
	if got := sendReasonForOpen(timeout); got != store.SendReasonSessionNoReply {
		t.Errorf("an open timeout = reason %d, want no reply", got)
	}
	if got := sendReasonForOpen(context.DeadlineExceeded); got != store.SendReasonDeadline {
		t.Errorf("our own deadline = reason %d, want deadline", got)
	}
	if got := sendReasonForOpen(syscall.ECONNREFUSED); got != store.SendReasonRefused {
		t.Errorf("a refused open = reason %d, want refused", got)
	}
	// Only an unplaceable error falls back to calling it a refusal.
	if got := sendReasonForOpen(errors.New("?")); got != store.SendReasonSessionRefused {
		t.Errorf("an unplaceable open failure = reason %d", got)
	}
}

// A probe abandoned because its own context ended (a shutdown, a settings
// change, the bucket deadline) learned nothing about the far end, so it has no
// reason; anything else is classified.
func TestConnErrorSaysNothingForAnAbandonedProbe(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if got := connError(done, context.Canceled); got != 0 {
		t.Errorf("abandoned probe: %#x, want no reason", got)
	}
	live := context.Background()
	for err, want := range map[error]uint16{
		syscall.ECONNREFUSED:        store.ProbeConnRefused,
		syscall.ECONNRESET:          store.ProbeConnReset,
		syscall.EHOSTUNREACH:        store.ProbeConnUnreachable,
		context.DeadlineExceeded:    store.ProbeConnTimeout,
		io.EOF:                      store.ProbeConnClosed,
		errors.New("something new"): store.ProbeConnOther,
	} {
		if got := connError(live, err); got != store.ProbeError(store.ProbeErrConn, want) {
			t.Errorf("%v: %#x, want detail %d", err, got, want)
		}
	}
}

// A probe that failed with an answer of its own finished; it was not abandoned
// by the truncation, so it counts. One still in flight with no answer does not.
func TestTruncatedIntervalCountsAProbeThatAlreadyFailed(t *testing.T) {
	var late atomic.Int64
	col := newCollector(2, &late)
	now := time.Now()
	col.markSent(0, 0, now)
	col.onProbeError(0, store.ProbeError(store.ProbeErrConn, store.ProbeConnRefused))
	col.markSent(1, 0, now)
	spec := TargetSpec{TargetID: 1, Pings: 2, TimeoutMS: 60_000, ProbeType: "tcp"}
	m := col.finalize(spec, 0, conditions{truncated: true})
	if m.Sent != 1 || m.Received != 0 {
		t.Fatalf("sent %d received %d, want the refused probe counted and the one in flight left out", m.Sent, m.Received)
	}
	if m.ProbeErr == nil || *m.ProbeErr != store.ProbeError(store.ProbeErrConn, store.ProbeConnRefused) {
		t.Errorf("ProbeErr = %v, want refused", m.ProbeErr)
	}
}
