package probe

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heistp/irtt"
)

// irttServer starts a real irtt server on a free loopback port. The probe is a
// session against a cooperating far end, so the only test that proves anything
// is one that talks to that far end.
func irttServer(t *testing.T) int {
	t.Helper()
	// Ask the kernel for a free UDP port, then hand the number to irtt: it
	// binds its own socket, so this one is released first.
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()

	cfg := irtt.NewServerConfig()
	cfg.Addrs = []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}

	// Wait for the server's own ListenerStart event.
	//
	// The obvious check — dial the address and see whether it connects — is
	// worthless for UDP: connect performs no handshake, so it succeeds
	// immediately whether or not anything is bound. A readiness loop built on
	// it returns at once and tests nothing, which is how this arrived in CI as
	// an intermittent "0 of 5 received over loopback".
	ready := make(chan struct{})
	cfg.Handler = handlerFunc(func(e *irtt.Event) {
		if e.Code == irtt.ListenerStart {
			select {
			case <-ready:
			default:
				close(ready)
			}
		}
	})

	srv := irtt.NewServer(cfg)
	go func() {
		// Returns when Shutdown is called; a failure here surfaces as the
		// probe finding nothing, which is what the assertions describe.
		_ = srv.ListenAndServe()
	}()
	t.Cleanup(srv.Shutdown)

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the irtt server never reported a listener; the test would otherwise " +
			"measure a server that is not there")
	}
	return port
}

// One session per interval must yield one sample per configured ping, not one
// sample for the whole bucket — the premise of the whole project is that an
// interval is a distribution.
func TestProbeIRTTFillsTheWholeInterval(t *testing.T) {
	port := irttServer(t)

	spec := TargetSpec{
		TargetID: 1, Host: "127.0.0.1", Family: "v4",
		IntervalS: 10, Pings: 5, Mode: "burst", BurstGapMS: 20,
		TimeoutMS: 2000, PacketSize: 64,
		ProbeType: "irtt", ProbePort: port,
	}

	var late atomic.Int64
	col := newCollector(spec.Pings, &late)
	probeIRTT(context.Background(), col, netip.MustParseAddr("127.0.0.1"), spec,
		time.Now().Add(5*time.Second))
	m := col.finalize(spec, 0, conditions{})

	if m.Sent != spec.Pings {
		t.Fatalf("sent %d, want %d — the session did not cover the interval", m.Sent, spec.Pings)
	}
	if m.Received != spec.Pings {
		t.Fatalf("received %d of %d over loopback; nothing should be lost here", m.Received, spec.Pings)
	}
	// Loopback RTTs are microseconds, but the assertion worth making is only
	// that they are real durations rather than the zeros a broken mapping
	// would produce.
	for _, s := range m.Samples {
		if s == 0 {
			t.Fatalf("a sample is 0µs: %v — the round trips are not being read off the result", m.Samples)
		}
	}
}

// An irtt server that is not there is total loss, not a gap. A gap would show
// nothing wrong on the graph where something is very wrong.
func TestProbeIRTTUnreachableServerIsTotalLoss(t *testing.T) {
	quietLog(t)
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close() // nothing is listening here now

	spec := TargetSpec{
		TargetID: 1, Host: "127.0.0.1", Family: "v4",
		IntervalS: 10, Pings: 4, Mode: "burst", BurstGapMS: 20,
		TimeoutMS: 500, PacketSize: 64,
		ProbeType: "irtt", ProbePort: port,
	}

	var late atomic.Int64
	col := newCollector(spec.Pings, &late)
	probeIRTT(context.Background(), col, netip.MustParseAddr("127.0.0.1"), spec,
		time.Now().Add(2*time.Second))
	m := col.finalize(spec, 0, conditions{})

	if m.Sent != spec.Pings || m.Received != 0 {
		t.Fatalf("got %d sent %d received, want the interval recorded as fully attempted and fully lost",
			m.Sent, m.Received)
	}
}

// handlerFunc adapts a function to irtt.Handler.
type handlerFunc func(*irtt.Event)

func (f handlerFunc) OnEvent(e *irtt.Event) { f(e) }
