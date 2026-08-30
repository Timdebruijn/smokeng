package probe

import (
	"context"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/heistp/irtt"
)

// defaultIRTTPort is where `irtt server` listens unless told otherwise.
const defaultIRTTPort = 2112

// irttHMACKeys maps an irtt endpoint — the target's configured host:port — to
// the shared secret that server requires. It authenticates this prober to a
// server configured with `--hmac`, so only smokeng may use it, closing the
// reflection/amplification and off-path spoofing an open UDP server otherwise
// invites. Set once at startup, read per probe.
//
// The key belongs to the server, not to the smokeng target, so it is keyed on
// where the server listens and lives in a file on whichever host runs the
// probe — the master, or an agent — the same shape as --tls-ca-file. This is
// deliberately NOT a tree setting: a secret in the tree would travel through
// the API and the exported TOML, which is exactly where it must not be. Keeping
// it out of the tree entirely makes that structural rather than a matter of
// discipline. An endpoint with no entry is probed without HMAC; if its server
// requires one, the session is refused and recorded as a flagged send failure.
var irttHMACKeys atomic.Pointer[map[string][]byte]

// SetIRTTHMACKeys sets the endpoint→secret map every irtt probe authenticates
// with. A nil or empty map clears it. A copy is stored, so the caller may reuse
// or wipe the map and its values.
func SetIRTTHMACKeys(m map[string][]byte) {
	if len(m) == 0 {
		irttHMACKeys.Store(nil)
		return
	}
	cp := make(map[string][]byte, len(m))
	for k, v := range m {
		cp[k] = append([]byte(nil), v...)
	}
	irttHMACKeys.Store(&cp)
}

// irttKeyFor returns the HMAC key for an endpoint, or nil if none is configured.
func irttKeyFor(endpoint string) []byte {
	m := irttHMACKeys.Load()
	if m == nil {
		return nil
	}
	return (*m)[endpoint]
}

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

	step := irttStep(spec)

	cfg := irtt.NewClientConfig()
	cfg.RemoteAddress = net.JoinHostPort(addr.String(), strconv.Itoa(port))
	// Keyed on the target's configured host, not the resolved address, so the
	// keyfile stays stable across DNS changes and matches what the operator
	// wrote in the target. An endpoint with no key is probed without HMAC.
	if k := irttKeyFor(net.JoinHostPort(spec.Host, strconv.Itoa(port))); k != nil {
		cfg.HMACKey = k
	}
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
		// The session never opened, so not one test packet reached the wire.
		// That is a send failure, not network loss: recording it as clean
		// total loss would blame the far end for a round trip that was never
		// attempted — and when the cause is local (a rejected config, a socket
		// the host would not give us) it says the server is down when it is
		// fine. markSendFailed flags the interval so the loss rail still shows
		// it, but labelled as ours rather than the network's.
		for i := range spec.Pings {
			col.markSendFailed(i)
		}
		log.Printf("probe: target %d (%s): irtt session to %s never opened: %v; recorded as a send failure",
			spec.TargetID, spec.Host, cfg.RemoteAddress, err)
		return
	}

	// Run returns nil even when the session broke part-way: irtt reports a send
	// or receive error inside the Result rather than from Run. A send error
	// means the un-sent tail never left the host, and a receive error means
	// replies that did arrive were dropped on our side — both are our loss, not
	// the network's, and the ICMP path has FlagSocketOverflow for exactly this.
	if r.SendErr != nil || r.ReceiveErr != nil {
		log.Printf("probe: target %d (%s): irtt session to %s ended early (send=%v receive=%v); "+
			"the loss in this interval may be ours, not the network's",
			spec.TargetID, spec.Host, cfg.RemoteAddress, r.SendErr, r.ReceiveErr)
	}

	now := time.Now()
	seen := 0
	for i, rt := range r.RoundTrips {
		// Never write past the collector: a server that paced differently than
		// asked could return more results than the interval has room for.
		if i >= spec.Pings {
			break
		}
		seen++
		if rt.RoundTripData == nil || !rt.ReplyReceived() {
			// A send error truncates RoundTrips, so an unanswered slot below the
			// break is a genuine send failure; above it, an ordinary lost reply.
			if r.SendErr != nil {
				col.markSendFailed(i)
			} else {
				col.markSent(i, 0, now) // sent, unanswered: loss
			}
			continue
		}
		// A negative round trip is an irtt anomaly, not our clock stepping.
		// recordRoundTrip synthesises txUser = now - rtt, so a negative rtt puts
		// txUser in the future and finalize would drop the sample as a clock
		// step — correct to drop, wrong to blame the local clock. Treat it as a
		// lost reply instead: no sample, no misleading flag.
		if rt.RTT() < 0 {
			col.markSent(i, 0, now)
			continue
		}
		col.recordRoundTrip(i, rt.RTT())
	}
	// A send error leaves RoundTrips short of Pings; the tail never went out, so
	// it is a send failure, not silent absence — otherwise Sent would under-count
	// and the interval would look narrower than it was asked to be.
	for i := seen; i < spec.Pings; i++ {
		col.markSendFailed(i)
	}
}

// irttStep is the spacing between an irtt session's packets, paced to match the
// target's probe mode so switching a target between icmp and irtt changes what
// the packets are, not when they go out. Shared with validateSpec, which
// refuses a target whose step is not positive.
func irttStep(spec TargetSpec) time.Duration {
	if spec.Mode == "spread" && spec.Pings > 0 {
		return time.Duration(spec.IntervalS) * time.Second / time.Duration(spec.Pings)
	}
	return time.Duration(spec.BurstGapMS) * time.Millisecond
}
