package probe

import (
	"crypto/rand"
	"hash/fnv"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"smokeng/internal/store"
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
}

func newCollector(n int, late *atomic.Int64) *collector {
	c := &collector{pings: make([]ping, n), late: late}
	for i := range c.pings {
		rand.Read(c.pings[i].token[:])
	}
	return c
}

func (c *collector) markSent(idx int, seq uint16, txUser time.Time) {
	c.mu.Lock()
	c.pings[idx].sent = true
	c.pings[idx].seq = seq
	c.pings[idx].txUser = txUser
	c.mu.Unlock()
}

func (c *collector) markSendFailed(idx int) {
	c.mu.Lock()
	c.pings[idx].sendFailed = true
	c.mu.Unlock()
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
func (c *collector) finalize(spec TargetSpec, bucket int64, rawSocket bool) store.Measurement {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = true

	timeout := time.Duration(spec.TimeoutMS) * time.Millisecond
	m := store.Measurement{
		TargetID: spec.TargetID,
		AgentID:  store.LocalAgentID,
		TS:       bucket,
	}
	if rawSocket {
		m.Flags |= store.FlagRawSocket
	}
	for i := range c.pings {
		p := &c.pings[i]
		if !p.sent || p.sendFailed {
			continue
		}
		m.Sent++
		if p.rx.IsZero() {
			continue
		}
		tx := p.txKern
		if tx.IsZero() {
			tx = p.txUser
			m.Flags |= store.FlagUserspaceTX
		}
		if !p.rxKernel {
			m.Flags |= store.FlagUserspaceRX
		}
		rtt := p.rx.Sub(tx)
		if rtt < 0 {
			rtt = 0
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
	return m
}
