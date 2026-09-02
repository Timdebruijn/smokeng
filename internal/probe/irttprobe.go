package probe

import (
	"context"
	"log"
	"math"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/heistp/irtt"
	"github.com/timdebruijn/smokeng/internal/store"
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
	// Its own timer. NewClientConfig points every client at the package-level
	// irtt.DefaultTimer, a single CompTimer holding one time.Timer and an
	// exponential averager whose fields are written without synchronisation.
	// Every target is probed on its own goroutine, so two irtt targets — an
	// ordinary configuration, and the shape a SmokePing import produces — share
	// it and corrupt each other's pacing state.
	//
	// The failure is not a slow probe. Reproduced with two concurrent sessions
	// against local servers: one completes and the other never returns, wedged
	// in the shared timer's sleep, and probeIRTT's own deadline does not free
	// it because what is stuck is not waiting on that context. A target simply
	// stops delivering measurements, indefinitely, with nothing in the log.
	//
	// NewDefaultCompTimer is not enough on its own: it builds a fresh timer
	// around the *shared* DefaultCompTimerAverage, so the averager — which is
	// the part with the unsynchronised fields — stays common to every client.
	// The averager has to be new too, which the race detector says plainly and
	// reading the constructor's name does not.
	cfg.Timer = irtt.NewCompTimer(irtt.NewDefaultExponentialAverager())
	cfg.RemoteAddress = net.JoinHostPort(addr.String(), strconv.Itoa(port))
	// Keyed on the target's configured host, not the resolved address, so the
	// keyfile stays stable across DNS changes and matches what the operator
	// wrote in the target. An endpoint with no key is probed without HMAC.
	if k := irttKeyFor(net.JoinHostPort(spec.Host, strconv.Itoa(port))); k != nil {
		cfg.HMACKey = k
	}
	cfg.Interval = step
	// One interval of room past the last packet's slot.
	//
	// irtt's sender does not count packets, it runs until a duration elapses,
	// and it keeps to the interval grid: a packet sent more than halfway
	// through its slot makes the client aim at the slot *after* next, which
	// costs a whole interval. The old margin was half a step, so a single late
	// send anywhere in the train ended the session one packet short — about a
	// quarter of intervals on a real target.
	//
	// A wider margin makes that rare, and cannot make it impossible: with no
	// skip this now schedules Pings+1 packets and the collector discards the
	// surplus, and with two skips it still comes up short. That is why the tail
	// below counts what was sent rather than what was asked for. SmokePing's
	// own IRTT probe uses Pings*interval, which has exactly the same arithmetic
	// and exactly the same failure.
	cfg.Duration = irttDuration(spec, step, time.Until(deadline))
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
			col.markSendFailed(i, sendReasonForOpen(err))
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
				// The session opened and broke. Classified from the error, so
				// a far end that refused the traffic (ECONNREFUSED, which on a
				// connected UDP socket means an ICMP unreachable came back) is
				// distinguishable from a local failure to transmit.
				col.markSendFailed(i, sendReasonOr(r.SendErr, store.SendReasonSessionEnded))
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
	recordIRTTSeries(col, r, spec.Pings)
	// A session that broke leaves the tail unsent, and that is a send failure.
	// A session that simply ended early is not: irtt paced fewer packets into
	// the window than were asked for, nothing failed, and nothing was lost.
	//
	// Marking the tail regardless made the interval read 19 of 20 with a
	// quality flag — five percent loss that never happened, on the one target
	// type where loss is the whole point. The probes that were not sent are
	// left uncounted instead, so the interval is 19 of 19: a narrower
	// distribution than was configured, which `sent` records and the graph
	// shows, and no loss, because none occurred.
	markUnsentTail(col, spec, seen, sessionError(r))
}

// sessionError is what broke the session, if anything did.
//
// Either half counts: a receive failure stops the client just as a send failure
// does, and the tail it stopped short of was owed. Keying only on the send
// error left a local receive failure looking like ordinary pacing, which is the
// one thing the tail handling exists to tell apart.
func sessionError(r *irtt.Result) error {
	if r.SendErr != nil {
		return r.SendErr
	}
	return r.ReceiveErr
}

// markUnsentTail decides what to do about probes the session never produced a
// result for.
//
// It is a function of its own because it is the whole distinction: a session
// that *broke* left its tail unsent and that is a send failure, while a session
// that simply ended early sent fewer packets and nothing failed at all. Inline,
// the second case could only be reached by waiting for irtt's pacing to skip a
// slot, which is timing-dependent; here it is a two-line test.
func markUnsentTail(col *collector, spec TargetSpec, seen int, sendErr error) {
	if seen >= spec.Pings {
		return
	}
	if sendErr != nil {
		for i := seen; i < spec.Pings; i++ {
			col.markSendFailed(i, sendReasonOr(sendErr, store.SendReasonSessionEnded))
		}
		return
	}
	// Nothing failed. irtt paced fewer packets into the window than were asked
	// for, so the probes below were never attempted — counting them made the
	// interval read 19 of 20 with a quality flag, five percent loss that never
	// happened, on the one probe type where loss is the whole point. Left
	// uncounted, the interval is 19 of 19: a narrower distribution than was
	// configured, which `sent` records, and no loss, because none occurred.
	//
	// Once per interval, because it is this prober's pacing rather than the
	// network's doing, and because it is the only trace the measurement leaves.
	log.Printf("probe: target %d (%s): irtt paced %d of %d probes into the interval; "+
		"the rest were never sent, so they are not counted as lost",
		spec.TargetID, spec.Host, seen, spec.Pings)
}

// irttDuration is how long to let an irtt session run for the interval's
// probes. Its own function so the arithmetic that decides whether a skipped
// slot costs a packet is the arithmetic a test can read, rather than a constant
// a test can restate and then agree with itself about.
//
// The ideal is one interval past the last packet's slot, which absorbs a
// skipped slot. It is clamped to the time the bucket actually has left, because
// in spread mode the step is the whole interval divided by the probe count, so
// the ideal runs *past the end of the bucket* — at 300s and 20 probes it asks
// for 315 seconds of a 300-second interval. Unclamped, the deadline cuts every
// session off and the target reads as total loss forever, which is a far worse
// failure than the one the wider window was added to fix.
//
// Clamped, spread mode gets no room for a skip and will occasionally come up a
// probe short. That is what markUnsentTail is for, and it is why the shortfall
// is counted honestly rather than assumed away.
func irttDuration(spec TargetSpec, step time.Duration, available time.Duration) time.Duration {
	want := step * time.Duration(spec.Pings+1)
	// A margin so the session ends before the bucket does rather than being
	// cancelled by it: a cancelled session reports an error, and an error means
	// the tail is counted as owed.
	limit := available - step
	if limit < step {
		// Too little left for even one more slot. Ask for what remains; the
		// session will be short and will say so.
		limit = available
	}
	if want > limit {
		return limit
	}
	return want
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

// seriesTrust says which of the extra series this session's negotiated
// timestamps actually support. The client asks for both clocks and a stamp at
// both ends; the server may restrict either, and irtt reports that with an
// event rather than an error — the session succeeds and the numbers keep
// arriving, quietly meaning something else.
type seriesTrust struct{ sendIPDV, receiveIPDV, serverProcessing bool }

// trustFrom reads the negotiated configuration. Every false here is a series
// that will not be recorded at all, because for each of them the alternative is
// not a slightly worse number but a differently-defined one wearing the label
// of the number the operator asked for.
func trustFrom(cfg *irtt.ClientConfig) seriesTrust {
	if cfg == nil {
		return seriesTrust{}
	}
	// Inter-packet delay variation cancels the offset between two hosts' clocks
	// only because it is a difference taken on the monotonic clock. irtt falls
	// back to the wall clock when the server returns no monotonic value, and
	// then a constant offset still cancels but a *step* does not: a 100 ms NTP
	// correction on the far end draws a 100 000 µs spike indistinguishable from
	// a real network event. That is a measurement of NTP, and this project does
	// not put one on a graph labelled jitter.
	if cfg.Clock&irtt.Monotonic == 0 {
		return seriesTrust{}
	}
	// Everything here needs the server's two stamps to be two.
	//
	// At AtMidpoint they are one value recorded twice, taken half way through
	// the server's handling of the packet. Server processing time is then not
	// unavailable but exactly zero — a distribution of fabricated zeros under a
	// heading that says how long the far end held each packet.
	//
	// The two IPDV figures survive that in form but not in meaning, which is
	// worse because it is harder to see. Both are differences against the
	// midpoint, so both carry half the server's hold time; when that hold time
	// varies between packets, half the variation lands in each direction.
	// Measured against the library with zero network jitter and a hold that
	// steps from 10ms to 20ms: AtBoth reports 0 in both directions, AtMidpoint
	// reports +5ms in both. A loaded server graphs its own scheduling as
	// network jitter, on the graph whose whole claim is that it separates the
	// two directions of the path.
	//
	// An earlier version of this kept the IPDV pair at the midpoint, reasoning
	// that each was still a difference against a real server stamp. That is
	// true and beside the point: what matters is what the difference contains.
	at := cfg.StampAt
	both := at == irtt.AtBoth
	return seriesTrust{sendIPDV: both, receiveIPDV: both, serverProcessing: both}
}

// recordIRTTSeries keeps the per-packet measurements irtt makes beside the
// round trip. They come out of the session smokeng already runs, so this costs
// nothing but the storage — and without it the directional half of what irtt
// measures is read off the wire and thrown away, which is what smokeng did
// until a migration noticed SmokePing had been graphing it all along.
//
// Only inter-packet delay variation is kept, not the absolute one-way delays
// sitting next to it in the same result. IPDV is a difference between
// consecutive packets taken from the monotonic clock, so the offset between the
// two hosts cancels; one-way delay is a difference between two hosts' wall
// clocks, and is wrong by exactly however far apart they have drifted. That
// makes one of them a measurement and the other a measurement of NTP, and only
// the first belongs in a graph labelled latency.
//
// A series this session cannot support is left out entirely rather than stored
// wrong. A series it can support but that produced no values — one reply, so no
// consecutive pair — is stored empty, which is a different fact and is recorded
// as one.
func recordIRTTSeries(col *collector, r *irtt.Result, pings int) {
	trust := trustFrom(r.Config)
	var send, receive, processing []int32
	for i := range r.RoundTrips {
		// The same cap the round-trip loop applies. A server that paced
		// differently than asked can return more results than the interval has
		// room for, and a distribution longer than the interval's own probe
		// count describes a packet set the stored sent/received do not.
		if i >= pings {
			break
		}
		rt := &r.RoundTrips[i]
		if rt.RoundTripData == nil || !rt.ReplyReceived() {
			continue
		}
		// A negative round trip is an irtt anomaly, and the round-trip loop
		// drops the sample for exactly that reason. Its derived figures are no
		// more trustworthy than the round trip they came from, so they go too —
		// otherwise a reading hidden from the latency graph reappears,
		// unflagged, on the jitter one.
		if rt.RTT() < 0 {
			continue
		}
		if trust.sendIPDV {
			if d, ok := irttMicros(rt.SendIPDV); ok {
				send = append(send, d)
			}
		}
		if trust.receiveIPDV {
			if d, ok := irttMicros(rt.ReceiveIPDV); ok {
				receive = append(receive, d)
			}
		}
		if trust.serverProcessing {
			if d, ok := irttMicros(rt.ServerProcessingTime()); ok {
				processing = append(processing, d)
			}
		}
	}
	col.recordSeries(store.SeriesIPDVSend, send, trust.sendIPDV)
	col.recordSeries(store.SeriesIPDVReceive, receive, trust.receiveIPDV)
	col.recordSeries(store.SeriesServerProcessing, processing, trust.serverProcessing)
}

// irttMicros converts an irtt duration to microseconds, reporting whether it
// was measured at all. irtt signals "not available" with a sentinel duration
// rather than an error, and storing that sentinel would put a value near the
// edge of int64 into a jitter distribution.
func irttMicros(d time.Duration) (int32, bool) {
	if d == irtt.InvalidDuration {
		return 0, false
	}
	us := d.Microseconds()
	if us > math.MaxInt32 || us < math.MinInt32 {
		// Not clamped: a value this far out is not a jitter reading that needs
		// rounding, it is a reading that cannot be true, and clamping would put
		// a plausible-looking extreme into the distribution.
		return 0, false
	}
	return int32(us), true
}
