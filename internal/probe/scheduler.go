package probe

import (
	"crypto/rand"
	"hash/fnv"
	"log"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
)

// bucketStart returns the wall-clock-aligned interval start containing t
// (DESIGN.md §5.3). Every agent computes identical bucket timestamps.
func bucketStart(t int64, intervalS int) int64 {
	iv := int64(intervalS)
	return t - (t % iv)
}

// phaseOffset is the deterministic per-target offset within a bucket, so many
// targets never fire in lockstep while recorded timestamps stay aligned.
// Burst mode shifts the whole burst within the room the bucket leaves for it;
// spread mode shifts the evenly-spaced train by less than one spacing.
func phaseOffset(spec TargetSpec) time.Duration {
	h := fnv.New64a()
	var b [8]byte
	for i := range 8 {
		b[i] = byte(spec.TargetID >> (8 * i))
	}
	h.Write(b[:])
	interval := time.Duration(spec.IntervalS) * time.Second
	var window time.Duration
	if spec.Mode == "spread" {
		window = interval / time.Duration(spec.Pings)
	} else {
		burst := time.Duration(spec.Pings) * time.Duration(spec.BurstGapMS) * time.Millisecond
		window = interval - burst
	}
	if window <= 0 {
		return 0
	}
	return time.Duration(h.Sum64() % uint64(window))
}

// sendTimes returns the transmit schedule for one bucket.
func sendTimes(spec TargetSpec, bucket int64) []time.Time {
	start := time.Unix(bucket, 0).Add(phaseOffset(spec))
	step := time.Duration(spec.BurstGapMS) * time.Millisecond
	if spec.Mode == "spread" {
		step = time.Duration(spec.IntervalS) * time.Second / time.Duration(spec.Pings)
	}
	out := make([]time.Time, spec.Pings)
	for i := range out {
		out[i] = start.Add(time.Duration(i) * step)
	}
	return out
}

// collector gathers the replies for one (target, bucket) and produces the
// measurement at finalization.
type collector struct {
	mu    sync.Mutex
	done  bool
	pings []ping
	late  *atomic.Int64
	// The same instant held twice: once as a bare wall-clock reading, once
	// carrying the monotonic reading. Comparing how far each has advanced by
	// finalization is what reveals a clock step, since only the wall clock
	// can jump.
	startWall time.Time
	startMono time.Time
	// series holds the extra per-packet distributions a probe measured beside
	// the round trip. Only irtt fills this in, and only for the series its peer
	// gave it the timestamps to compute; the rest stay absent rather than
	// present-and-zero. See store.SeriesIPDVSend.
	series map[string][]int32
}

// recordSeries attaches one extra per-packet distribution to this bucket. The
// values are sorted here rather than at the call site: what is stored is a
// distribution, so the order packets arrived in is not part of it, and the blob
// codec requires ascending order anyway.
//
// measured says whether this probe was in a position to measure the series at
// all, and is the whole reason it is a separate argument from the values. An
// interval that measured it and computed nothing — one reply, so no consecutive
// pair to difference — is stored empty, and reads as "there was nothing to
// compare". An interval that could not measure it is absent, and reads as "not
// measured". Dropping on len(vals) == 0 collapsed the two into the second, which
// let a lossy target report an instrumentation problem it did not have.
func (c *collector) recordSeries(name string, vals []int32, measured bool) {
	if !measured {
		return
	}
	slices.Sort(vals)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.series == nil {
		c.series = make(map[string][]int32, 2)
	}
	if vals == nil {
		// A nil slice and an empty one are the same fact, but only one of them
		// survives the map lookup on the way out as "present".
		vals = []int32{}
	}
	c.series[name] = vals
}

// conditions describe how the measurement was taken, as opposed to what it
// measured. They become the quality flags on the stored row.
type conditions struct {
	rawSocket  bool
	overflowed bool // the socket's receive queue dropped replies
	// truncated marks a bucket finalized before its window closed, which
	// happens on shutdown and on a settings change. A probe that was never
	// given its full timeout has not been shown to be lost, so it is not
	// counted as attempted — otherwise every restart wrote a loss spike that
	// no network event caused.
	truncated bool
}

const (
	// clockStepFloor covers readings taken microseconds apart.
	clockStepFloor = 2 * time.Millisecond
	// Slewing must not register as a step. Whether it shows up here at all
	// is platform-dependent: Linux slews CLOCK_MONOTONIC along with
	// CLOCK_REALTIME, so the two stay together, but macOS leaves
	// mach_absolute_time untouched and the divergence lands squarely in this
	// comparison. NTP implementations slew at up to 500ppm; 600 leaves margin
	// without widening the window further than it has to be.
	//
	// The tolerance is a rate, so it grows with the bucket — and that is a real
	// limit, not an oversight. Comparing only the bucket's endpoints cannot
	// separate a step from slew of the same magnitude, so on a five-minute
	// interval a step under ~180ms is indistinguishable from legitimate slew
	// and is not flagged here. Two things cover that gap: a backwards step
	// makes the RTT negative, which finalize drops and flags on its own, and a
	// forwards step inflates RTTs into a visible excursion rather than a
	// plausible-looking wrong number. Closing it properly means sampling both
	// clocks per ping rather than per bucket.
	clockSlewPPM = 600
)

// clockStepped reports whether the wall clock moved differently from the
// monotonic clock over the same span by more than slewing could account for.
// The tolerance grows with the span, because slew is a rate.
func clockStepped(wallDelta, monoDelta time.Duration) bool {
	tolerance := clockStepFloor + time.Duration(int64(monoDelta)*clockSlewPPM/1_000_000)
	d := wallDelta - monoDelta
	return d > tolerance || d < -tolerance
}

type ping struct {
	token      [8]byte
	sent       bool
	sendFailed bool
	seq        uint16
	txUser     time.Time
	txKern     time.Time
	rx         time.Time
	rxKernel   bool
	// An ICMP error explains why this ping went unanswered: the probe was
	// refused rather than ignored.
	icmpErr            bool
	icmpType, icmpCode uint8
	// Why this probe could not be sent, when it could not. See
	// store.SendReason*; zero means it was not recorded.
	sendReason uint8
}

func newCollector(n int, late *atomic.Int64) *collector {
	now := time.Now()
	// Round(0) strips the monotonic reading, leaving the wall clock alone.
	c := &collector{pings: make([]ping, n), late: late, startWall: now.Round(0), startMono: now}
	for i := range c.pings {
		rand.Read(c.pings[i].token[:])
	}
	return c
}

// The unlocks below are deferred rather than called, which matters more than
// it looks: an out-of-range index panics with the mutex held, and a contained
// panic would then leave finalize blocked on it forever. A prober wedged on a
// mutex is worse than one that crashed — systemd restarts a crash.
func (c *collector) markSent(idx int, seq uint16, txUser time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pings[idx].sent = true
	c.pings[idx].seq = seq
	c.pings[idx].txUser = txUser
}

// markSendFailed records a probe the local side could not put on the wire.
//
// It marks the ping sent as well, which finalize needs in order to count it at
// all: an attempt that failed locally is still an attempt, and dropping it
// would render an unreachable target as no data rather than as total loss —
// the outcome the loop in finalize says in so many words that it is avoiding.
// The icmp path happened to call markSent first and so was unaffected; the
// userspace types call this on its own, and were silently losing the flag
// along with the row.
// The reason is recorded alongside the flag. The flag said that a send failed
// and nothing said why, so a far end refusing packets and this host never
// getting them out — a network fault and a bug here — were stored identically.
func (c *collector) markSendFailed(idx int, reason uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pings[idx].sent = true
	c.pings[idx].sendFailed = true
	c.pings[idx].sendReason = reason
}

func (c *collector) onRX(idx int, t time.Time, kernel bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		c.late.Add(1)
		return
	}
	p := &c.pings[idx]
	if !p.rx.IsZero() {
		return // duplicate reply
	}
	p.rx = t
	p.rxKernel = kernel
}

// recordRoundTrip stores a round trip that was measured elsewhere and handed
// back as a duration — the irtt case, where a cooperating server paces the
// train and reports each packet itself.
//
// The pair of timestamps is synthesised backwards from the duration, because
// what a measurement stores is the RTT and the bucket, never the wall-clock
// instant of an individual packet. Nothing downstream can tell the difference,
// and the alternative — a second path through finalize that bypasses the
// timeout and clock-step checks — would let irtt samples escape the scrutiny
// every other sample gets.
func (c *collector) recordRoundTrip(idx int, rtt time.Duration) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		c.late.Add(1)
		return
	}
	p := &c.pings[idx]
	if !p.rx.IsZero() {
		return
	}
	p.sent = true
	p.txUser = now.Add(-rtt)
	p.rx = now
	p.rxKernel = false
}

func (c *collector) onICMPError(idx int, icmpType, code uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return
	}
	p := &c.pings[idx]
	// A reply already in hand wins: the error concerns a probe that failed.
	if !p.rx.IsZero() {
		return
	}
	p.icmpErr, p.icmpType, p.icmpCode = true, icmpType, code
}

func (c *collector) onTXKernel(idx int, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.pings[idx].txKern = t
	}
}

// finalize closes the collector and builds the measurement. A reply that took
// longer than the per-ping timeout counts as lost, matching the classic
// semantics; replies after finalization are dropped and counted.
func (c *collector) finalize(spec TargetSpec, bucket int64, cond conditions) store.Measurement {
	// Counted across the bucket so the log line is one per interval, not one
	// per ping.
	suspectTX := 0
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = true

	timeout := time.Duration(spec.TimeoutMS) * time.Millisecond
	m := store.Measurement{
		TargetID: spec.TargetID,
		AgentID:  store.LocalAgentID,
		TS:       bucket,
	}
	if cond.rawSocket {
		m.Flags |= store.FlagRawSocket
	}
	if cond.overflowed {
		m.Flags |= store.FlagSocketOverflow
	}
	// A truncated bucket is not comparable in width with a whole one, and that
	// is exactly what the flag says. It was only ever set inside the per-ping
	// loop, for a probe still in flight when the bucket was cut short — so an
	// interval whose sent probes had all answered before shutdown was stored
	// as a complete one, a quarter-width distribution claiming to be whole.
	// The condition is a property of the bucket, so it belongs here, not on a
	// ping that may or may not exist.
	if cond.truncated {
		m.Flags |= store.FlagTruncated
	}
	now := time.Now()
	if clockStepped(now.Round(0).Sub(c.startWall), now.Sub(c.startMono)) {
		m.Flags |= store.FlagClockStep
	}
	// Tally which ICMP error, if any, explains this interval's failures. The
	// most frequent one is stored: a single value has to represent the
	// interval, and the common cause is the useful one.
	errCounts := map[uint16]int{}
	sendCounts := map[uint8]int{}
	deadline := now.Add(-timeout)
	for i := range c.pings {
		p := &c.pings[i]
		if !p.sent {
			continue
		}
		// Cut short: a probe still within its timeout was abandoned, not
		// lost, so it is excluded from the count. (The bucket-level flag is
		// already set above; this only decides not to count the probe.)
		// Counting it as attempted would report loss that never happened, and
		// a service restart would look like an outage.
		if cond.truncated && p.rx.IsZero() && !p.sendFailed && p.txUser.After(deadline) {
			continue
		}
		// A probe the kernel refused to transmit was still attempted, and is
		// lost. Dropping it from the count would render an unreachable target
		// as no data at all rather than as total loss.
		m.Sent++
		if p.sendFailed {
			m.Flags |= store.FlagSendFailed
			if p.sendReason != 0 {
				sendCounts[p.sendReason]++
			}
			continue
		}
		if p.icmpErr {
			errCounts[store.ICMPError(p.icmpType, p.icmpCode)]++
		}
		if p.rx.IsZero() {
			continue
		}
		tx := p.txKern
		// A kernel transmit stamp that is not between this ping's own send and
		// its reply did not belong to this ping. The kernel labels TX stamps
		// with a counter it increments per queued packet; userspace keeps its
		// own count, and the two can drift apart — a send that fails before
		// the kernel reaches skb construction advances one and not the other.
		// After that every stamp lands on a neighbouring packet, often on a
		// different target sharing the socket, and the RTT it produces is
		// wrong without being impossible-looking. Checking the stamp against
		// the bounds this ping already knows catches the drift whatever caused
		// it, and costs a comparison.
		if !tx.IsZero() && (tx.Before(p.txUser) || tx.After(p.rx)) {
			tx = time.Time{}
			suspectTX++
		}
		if tx.IsZero() {
			tx = p.txUser
			m.Flags |= store.FlagUserspaceTX
		}
		if !p.rxKernel {
			m.Flags |= store.FlagUserspaceRX
		}
		rtt := p.rx.Sub(tx)
		if rtt < 0 {
			// Even the userspace stamps disagree, so the clock moved under us.
			// This used to be clamped to zero and stored, which put a
			// fabricated 0 µs reading into the distribution and left it
			// indistinguishable from a real sub-microsecond one. Drop the
			// sample and say why instead.
			m.Flags |= store.FlagClockStep
			continue
		}
		if rtt > timeout {
			continue // too late = lost
		}
		us := rtt.Microseconds()
		if us > math.MaxUint32 {
			us = math.MaxUint32
		}
		m.Samples = append(m.Samples, uint32(us))
	}
	slices.Sort(m.Samples)
	m.Received = len(m.Samples)

	// One line, once per interval that saw it: a desynchronised counter is a
	// property of the socket, not of this bucket, and it will keep happening
	// until the process restarts.
	if suspectTX > 0 {
		log.Printf("probe: target %d (%s): discarded %d kernel transmit stamp(s) that did not fit "+
			"the ping they were attributed to; using userspace timestamps for those",
			spec.TargetID, spec.Host, suspectTX)
	}

	if len(errCounts) > 0 {
		m.Flags |= store.FlagICMPError
		var best uint16
		bestN := -1
		for packed, n := range errCounts {
			// Ties break on the lower packed value so the result is stable.
			if n > bestN || (n == bestN && packed < best) {
				best, bestN = packed, n
			}
		}
		m.ICMPErr = &best
	}
	// The most frequent reason represents the interval, as the ICMP tally
	// above does: one value has to stand for it, and the common cause is the
	// one worth acting on.
	if len(sendCounts) > 0 {
		var best uint8
		bestN := -1
		for reason, n := range sendCounts {
			if n > bestN || (n == bestN && reason < best) {
				best, bestN = reason, n
			}
		}
		m.SendErr = &best
	}
	m.Series = c.series
	return m
}
