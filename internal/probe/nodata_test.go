package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
)

func noRouteErr(e syscall.Errno) error {
	return &net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("sendto", e)}
}

// finalizeWith builds an interval of three probes. fail says per probe how it
// ended: an errno means it failed to send with it, nil means it was answered.
func finalizeWith(t *testing.T, fail ...error) store.Measurement {
	t.Helper()
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = len(fail)
	col := newCollector(len(fail), &late)
	base := time.Now().Add(-10 * time.Millisecond)
	for i, err := range fail {
		col.markSent(i, uint16(i), base)
		if err == nil {
			col.onRX(i, base.Add(2*time.Millisecond), true)
			continue
		}
		col.markSendError(i, sendReasonFor(err), err)
	}
	return col.finalize(spec, 1_756_400_100, conditions{})
}

// Protects the decision that a host without a route reports no data, not total
// loss: every probe ENETUNREACH is one row with sent = 0, the flag and the
// reason, and nothing else that would describe an interval that measured.
func TestFinalizeAllNoRouteIsNoData(t *testing.T) {
	e := noRouteErr(syscall.ENETUNREACH)
	m := finalizeWith(t, e, e, e)
	if m.Sent != 0 || m.Received != 0 || len(m.Samples) != 0 {
		t.Fatalf("sent=%d received=%d samples=%d, want an empty interval", m.Sent, m.Received, len(m.Samples))
	}
	if m.Flags != store.FlagSendFailed {
		t.Errorf("flags = %08b, want only FlagSendFailed", m.Flags)
	}
	if m.SendErr == nil || *m.SendErr != store.SendReasonUnreachable {
		t.Errorf("send error = %v, want SendReasonUnreachable", m.SendErr)
	}
	if m.LossMask != nil || m.ICMPErr != nil || m.Series != nil {
		t.Errorf("mask, ICMP error or series on a no-data row: %+v", m)
	}
}

// Protects the other half of the distinction: EHOSTUNREACH can come from a
// router on the path, so it stays what it was, 100% loss with the flag.
func TestFinalizeHostUnreachableStaysLoss(t *testing.T) {
	e := noRouteErr(syscall.EHOSTUNREACH)
	m := finalizeWith(t, e, e, e)
	if m.Sent != 3 || m.Received != 0 || m.Flags&store.FlagSendFailed == 0 {
		t.Fatalf("sent=%d received=%d flags=%08b, want 3 sent, none received, flagged", m.Sent, m.Received, m.Flags)
	}
	if m.SendErr == nil || *m.SendErr != store.SendReasonUnreachable {
		t.Errorf("send error = %v, want SendReasonUnreachable", m.SendErr)
	}
}

// Protects against a route that works part of the time being erased: one probe
// that was sent keeps the interval as the measurement it is.
func TestFinalizeNoRouteMixedWithRepliesIsUnchanged(t *testing.T) {
	e := noRouteErr(syscall.ENETUNREACH)
	m := finalizeWith(t, nil, e, e)
	if m.Sent != 3 || m.Received != 1 {
		t.Fatalf("sent=%d received=%d, want 3 and 1", m.Sent, m.Received)
	}
}

// Protects against one probe failing for another reason being swallowed into a
// no-data interval: ENETUNREACH on some and EHOSTUNREACH on one is mixed.
func TestFinalizeNoRouteMixedWithOtherFailureIsUnchanged(t *testing.T) {
	m := finalizeWith(t, noRouteErr(syscall.ENETUNREACH), noRouteErr(syscall.EHOSTUNREACH))
	if m.Sent != 2 {
		t.Fatalf("sent = %d, want 2", m.Sent)
	}
}

// Protects every other local reason: all probes failing for a reason that is
// not a missing route is N attempted and lost, as before.
func TestFinalizeOtherLocalFailureIsUnchanged(t *testing.T) {
	e := noRouteErr(syscall.ENOBUFS)
	m := finalizeWith(t, e, e, e)
	if m.Sent != 3 || m.Received != 0 || m.Flags&store.FlagSendFailed == 0 {
		t.Fatalf("sent=%d received=%d flags=%08b", m.Sent, m.Received, m.Flags)
	}
	if m.SendErr == nil || *m.SendErr != store.SendReasonSocket {
		t.Errorf("send error = %v, want SendReasonSocket", m.SendErr)
	}
}

// Protects tcp and http, which count a failed dial as plain loss: ENETUNREACH
// there makes no-data only when every probe got it, and a mixed interval keeps
// the loss and carries no send-failure flag it did not have before.
func TestFinalizeNoRouteOnDialIsNoDataOnlyWhenAll(t *testing.T) {
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = 2
	base := time.Now().Add(-10 * time.Millisecond)
	build := func(second bool) store.Measurement {
		col := newCollector(2, &late)
		for i := range 2 {
			col.markSent(i, 0, base)
		}
		col.markNoRoute(0)
		if second {
			col.markNoRoute(1)
		}
		return col.finalize(spec, 1_756_400_100, conditions{})
	}
	if m := build(true); m.Sent != 0 || m.SendErr == nil || *m.SendErr != store.SendReasonUnreachable {
		t.Errorf("all probes without a route: %+v, want a no-data row", m)
	}
	m := build(false)
	if m.Sent != 2 || m.Received != 0 || m.Flags != 0 {
		t.Errorf("mixed: sent=%d received=%d flags=%08b, want plain loss", m.Sent, m.Received, m.Flags)
	}
}

// Protects the classification the whole feature rests on, through the wrapping
// the net stack and an irtt open really produce.
func TestIsNoRoute(t *testing.T) {
	if !isNoRoute(noRouteErr(syscall.ENETUNREACH)) {
		t.Error("ENETUNREACH is not recognised")
	}
	if isNoRoute(noRouteErr(syscall.EHOSTUNREACH)) || isNoRoute(errors.New("x")) || isNoRoute(nil) {
		t.Error("something that is not ENETUNREACH was taken for it")
	}
}

// Protects case 1 of the decision: an interval whose hostname gave no address
// is written as a no-data row with SendReasonNoAddress instead of nothing, and
// is counted as a DNS error.
func TestRunTargetWritesNoDataWhenUnresolved(t *testing.T) {
	quietLog(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &Engine{
		st: st,
		lookup: func(context.Context, string, string) (netip.Addr, error) {
			return netip.Addr{}, errors.New("no such host")
		},
		results: make(chan store.Measurement, 4),
	}
	spec := burstSpec()
	spec.IntervalS = 1
	spec.Host = "nowhere.invalid"
	spec.Family = "v6"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runTarget(ctx, spec)

	select {
	case m := <-e.results:
		if m.Sent != 0 || m.Received != 0 || m.Flags != store.FlagSendFailed ||
			m.SendErr == nil || *m.SendErr != store.SendReasonNoAddress {
			t.Errorf("row = %+v, want an empty row, FlagSendFailed, SendReasonNoAddress", m)
		}
		if m.TargetID != spec.TargetID || m.AgentID != store.LocalAgentID || m.TS%int64(spec.IntervalS) != 0 {
			t.Errorf("row identity = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no row written for an unresolved interval")
	}
	if e.dnsErrs.Load() == 0 {
		t.Error("the resolve failure was not counted")
	}
}

// Protects the irtt open path: a session that never opened because the host has
// no route must keep the ENETUNREACH on the way to finalize, and one that failed
// for any other reason must not become no data. The tail of a session that broke
// part-way is marked the same way.
func TestFinalizeIRTTNoRouteIsNoData(t *testing.T) {
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = 3
	for _, tc := range []struct {
		name   string
		mark   func(col *collector, err error)
		err    error
		noData bool
	}{
		{"never opened, no route", func(c *collector, err error) { markNeverOpened(c, 3, err) }, noRouteErr(syscall.ENETUNREACH), true},
		{"never opened, host unreachable", func(c *collector, err error) { markNeverOpened(c, 3, err) }, noRouteErr(syscall.EHOSTUNREACH), false},
		{"broke at once, no route", func(c *collector, err error) { markUnsentTail(c, spec, 0, err) }, noRouteErr(syscall.ENETUNREACH), true},
	} {
		col := newCollector(3, &late)
		tc.mark(col, tc.err)
		m := col.finalize(spec, 1_756_400_100, conditions{})
		if got := m.Sent == 0; got != tc.noData {
			t.Errorf("%s: no data = %v, want %v (%+v)", tc.name, got, tc.noData, m)
		}
	}
}
