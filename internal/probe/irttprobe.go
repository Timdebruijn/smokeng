package probe

import (
	"context"
	"log"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/heistp/irtt"
)

// defaultIRTTPort is where `irtt server` listens unless told otherwise.
const defaultIRTTPort = 2112

// probeIRTT runs one IRTT session for the whole bucket.
//
// This type is the odd one out, and deliberately so. Every other probe is N
// independent round trips that smokeng schedules itself; IRTT is a session
// with a cooperating server at the far end, which paces the train itself and
// hands back a per-packet result set. So it is called once per interval rather
// than once per probe, and the N samples come out of one Run.
//
// What it buys over ICMP is a measurement the network has no reason to treat
// specially: IRTT is ordinary UDP, so it is not rate-limited by the control
// plane the way ICMP echo is on most routers, and it is not answered by a
// middlebox on the target's behalf. The cost is that it only works where you
// control both ends — the far side has to be running `irtt server`.
//
// IRTT also measures one-way delay in each direction, which is genuinely more
// than smokeng stores. Only the round trip is kept: the schema holds one
// distribution per interval, and splitting it would be a change to what a
// measurement *is* rather than an addition to it.
func probeIRTT(ctx context.Context, col *collector, addr netip.Addr, spec TargetSpec, deadline time.Time) {
	port := spec.ProbePort
	if port == 0 {
		port = defaultIRTTPort
	}

	// Pace the session the way the target's probe mode asks for, so switching
	// a target between icmp and irtt does not silently change when the packets
	// go out — only what they are.
	step := time.Duration(spec.BurstGapMS) * time.Millisecond
	if spec.Mode == "spread" {
		step = time.Duration(spec.IntervalS) * time.Second / time.Duration(spec.Pings)
	}

	cfg := irtt.NewClientConfig()
	cfg.RemoteAddress = net.JoinHostPort(addr.String(), strconv.Itoa(port))
	cfg.Interval = step
	// The first packet leaves at t=0, so N packets need the run to last past
	// the (N-1)th interval. Half a step of margin keeps a scheduling hiccup
	// from costing the last packet of every bucket.
	cfg.Duration = step*time.Duration(spec.Pings-1) + step/2
	cfg.Length = spec.PacketSize
	cfg.DSCP = spec.DSCP
	cfg.IPVersion = irtt.IPv4
	if spec.Family == "v6" {
		cfg.IPVersion = irtt.IPv6
	}

	// Bound the session by the bucket's own finalization deadline. Without
	// this an unreachable server could keep retrying its handshake past the
	// end of the interval, and the bucket that is waiting on this goroutine
	// would finalize late — pushing a measurement into the wrong minute.
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	r, err := irtt.NewClient(cfg).Run(rctx)
	if err != nil {
		// The session never opened, so no test packet was sent. It is still
		// recorded as a fully attempted, fully lost interval rather than as a
		// gap: an unreachable irtt server is total loss in the same sense that
		// an unreachable host is, and leaving a hole in the graph would show
		// nothing wrong where something is.
		now := time.Now()
		for i := range spec.Pings {
			col.markSent(i, 0, now)
		}
		log.Printf("probe: target %d (%s): irtt session to %s failed: %v; interval recorded as total loss",
			spec.TargetID, spec.Host, cfg.RemoteAddress, err)
		return
	}

	now := time.Now()
	for i, rt := range r.RoundTrips {
		// Never write past the collector: a server that paced differently than
		// asked could return more results than the interval has room for.
		if i >= spec.Pings {
			break
		}
		if rt.RoundTripData == nil || !rt.ReplyReceived() {
			col.markSent(i, 0, now) // sent, unanswered: loss
			continue
		}
		col.recordRoundTrip(i, rt.RTT())
	}
}
