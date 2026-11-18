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

// Protects the decision that ENETUNREACH on a probe is not "this host has no
// route": a router on the path with no route makes a TCP connect fail with the
// same errno, and that is loss on the path. An interval whose probes all failed
// to send with it is what it always was, N sent, none received, flagged.
func TestFinalizeAllENETUNREACHStaysLoss(t *testing.T) {
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = 3
	col := newCollector(3, &late)
	base := time.Now().Add(-10 * time.Millisecond)
	err := &net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("sendto", syscall.ENETUNREACH)}
	for i := range 3 {
		col.markSent(i, uint16(i), base)
		col.markSendFailed(i, sendReasonFor(err))
	}
	m := col.finalize(spec, 1_756_400_100, conditions{})
	if m.Sent != 3 || m.Received != 0 || m.Flags&store.FlagSendFailed == 0 {
		t.Fatalf("sent=%d received=%d flags=%08b, want 3 sent, none received, flagged", m.Sent, m.Received, m.Flags)
	}
}

// engineFor is an Engine whose target resolves to a loopback address, with the
// route check answering noRoute.
func engineFor(t *testing.T, lookup func(context.Context, string, string) (netip.Addr, error), noRoute bool) *Engine {
	t.Helper()
	quietLog(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Engine{
		st:      st,
		lookup:  lookup,
		noRoute: func(netip.Addr, string) bool { return noRoute },
		results: make(chan store.Measurement, 4),
	}
}

// tcpSpec probes a local listener with one tcp probe per second, so a probed
// interval is a row with Sent = 1.
func tcpSpec(t *testing.T) TargetSpec {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	spec := burstSpec()
	spec.IntervalS, spec.Pings, spec.TimeoutMS = 1, 1, 500
	spec.ProbeType, spec.Host, spec.Family = "tcp", "127.0.0.1", "v4"
	spec.ProbePort = ln.Addr().(*net.TCPAddr).Port
	return spec
}

var loopback = func(context.Context, string, string) (netip.Addr, error) {
	return netip.MustParseAddr("127.0.0.1"), nil
}

// Protects the route check: with this host reporting no route to the address,
// the interval is one no-data row with reason unreachable and no probe is sent
// (the listener never sees a connection and the row says Sent = 0).
func TestRunTargetNoRouteIsNoDataAndSendsNothing(t *testing.T) {
	spec := tcpSpec(t)
	e := engineFor(t, loopback, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runTarget(ctx, spec)

	select {
	case m := <-e.results:
		if m.Sent != 0 || m.Received != 0 || m.Flags != store.FlagSendFailed ||
			m.SendErr == nil || *m.SendErr != store.SendReasonUnreachable {
			t.Errorf("row = %+v, want an empty row, FlagSendFailed, SendReasonUnreachable", m)
		}
		if m.TargetID != spec.TargetID || m.AgentID != store.LocalAgentID {
			t.Errorf("row identity = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no row written for an interval without a route")
	}
	// A second row inside the interval would be a probe or a duplicate.
	select {
	case m := <-e.results:
		if m.Sent != 0 {
			t.Errorf("a probe was sent: %+v", m)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// Protects the other side of the check: with a route the interval is probed as
// usual, so a check that says anything but "no route" never costs a row.
func TestRunTargetWithRouteIsProbed(t *testing.T) {
	spec := tcpSpec(t)
	e := engineFor(t, loopback, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runTarget(ctx, spec)

	select {
	case m := <-e.results:
		if m.Sent != 1 || m.Received != 1 {
			t.Errorf("row = %+v, want one probe sent and answered", m)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no row for a probed interval")
	}
}

// Protects the real check: loopback has a route, so it must not say otherwise,
// for either family it is asked about. (A routeless host cannot be made in a
// unit test; the measurement is in docs/probes.md.)
func TestNoRouteToLoopbackHasRoute(t *testing.T) {
	if noRouteTo(netip.MustParseAddr("127.0.0.1"), "v4") {
		t.Error("127.0.0.1 reported as having no route")
	}
	if ln, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		ln.Close()
		if noRouteTo(netip.MustParseAddr("::1"), "v6") {
			t.Error("::1 reported as having no route")
		}
	}
}

// Protects against writing a row for a lookup that failed because the engine
// is stopping or the target's loop was cancelled: that says nothing about the
// name, and a reload would otherwise leave a no-address row behind.
func TestRunTargetDoesNotEmitWhenLookupIsCancelled(t *testing.T) {
	spec := tcpSpec(t)
	ctx, cancel := context.WithCancel(context.Background())
	e := engineFor(t, func(ctx context.Context, _, _ string) (netip.Addr, error) {
		cancel()
		return netip.Addr{}, ctx.Err()
	}, false)
	done := make(chan struct{})
	go func() { e.runTarget(ctx, spec); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runTarget did not return after its context was cancelled")
	}
	select {
	case m := <-e.results:
		t.Errorf("a row was emitted for a cancelled lookup: %+v", m)
	default:
	}
}

// Protects case 1 of the decision: an interval whose hostname gave no address
// is written as a no-data row with SendReasonNoAddress instead of nothing, and
// is counted as a DNS error.
func TestRunTargetWritesNoDataWhenUnresolved(t *testing.T) {
	e := engineFor(t, func(context.Context, string, string) (netip.Addr, error) {
		return netip.Addr{}, errors.New("no such host")
	}, false)
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
		if m.TargetID != spec.TargetID || m.AgentID != store.LocalAgentID {
			t.Errorf("row identity = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no row written for an unresolved interval")
	}
	if e.dnsErrs.Load() == 0 {
		t.Error("the resolve failure was not counted")
	}
}
