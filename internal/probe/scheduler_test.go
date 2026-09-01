package probe

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
)

func burstSpec() TargetSpec {
	return TargetSpec{
		TargetID: 42, IntervalS: 60, Pings: 20, Mode: "burst",
		BurstGapMS: 10, TimeoutMS: 1000,
	}
}

func TestBucketStartAligned(t *testing.T) {
	for _, now := range []int64{0, 59, 60, 61, 1_756_400_123} {
		bs := bucketStart(now, 60)
		if bs%60 != 0 || bs > now || now-bs >= 60 {
			t.Errorf("bucketStart(%d, 60) = %d", now, bs)
		}
	}
}

func TestPhaseOffsetDeterministicAndBounded(t *testing.T) {
	spec := burstSpec()
	off := phaseOffset(spec)
	if off != phaseOffset(spec) {
		t.Fatal("offset not deterministic")
	}
	// Burst: the whole burst plus offset must fit inside the interval.
	burst := time.Duration(spec.Pings*spec.BurstGapMS) * time.Millisecond
	if off < 0 || off+burst > time.Duration(spec.IntervalS)*time.Second {
		t.Fatalf("burst offset %v leaves no room for burst %v", off, burst)
	}

	spread := spec
	spread.Mode = "spread"
	if o := phaseOffset(spread); o < 0 || o >= 3*time.Second { // spacing = 60s/20
		t.Fatalf("spread offset %v outside one spacing", o)
	}

	// Different targets get different offsets (with overwhelming likelihood).
	other := spec
	other.TargetID = 43
	if phaseOffset(spec) == phaseOffset(other) {
		t.Log("warning: two targets share an offset (possible, but suspicious)")
	}
}

func TestSendTimes(t *testing.T) {
	spec := burstSpec()
	bucket := int64(1_756_400_100 - 1_756_400_100%60)
	times := sendTimes(spec, bucket)
	if len(times) != 20 {
		t.Fatalf("%d send times", len(times))
	}
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d != 10*time.Millisecond {
			t.Fatalf("burst gap %v at %d", d, i)
		}
	}
	if times[0].Before(time.Unix(bucket, 0)) {
		t.Fatal("first send before bucket start")
	}

	spread := spec
	spread.Mode = "spread"
	stimes := sendTimes(spread, bucket)
	if d := stimes[1].Sub(stimes[0]); d != 3*time.Second {
		t.Fatalf("spread spacing %v", d)
	}
	last := stimes[len(stimes)-1]
	if !last.Before(time.Unix(bucket+60, 0)) {
		t.Fatal("last spread send outside bucket")
	}
}

func TestCollectorFinalize(t *testing.T) {
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = 4
	col := newCollector(4, &late)
	base := time.Unix(1_756_400_100, 0)

	// 0: normal reply, kernel TX+RX. 1: userspace TX. 2: reply slower than
	// the timeout (lost). 3: no reply.
	for i := range 4 {
		col.markSent(i, uint16(i), base)
	}
	col.onTXKernel(0, base.Add(100*time.Microsecond))
	col.onRX(0, base.Add(20*time.Millisecond+100*time.Microsecond), true)
	col.onRX(1, base.Add(5*time.Millisecond), true)
	col.onRX(2, base.Add(1500*time.Millisecond), true)

	m := col.finalize(spec, 1_756_400_100, conditions{})
	if m.Sent != 4 || m.Received != 2 {
		t.Fatalf("sent=%d received=%d, want 4/2", m.Sent, m.Received)
	}
	if !slices.Equal(m.Samples, []uint32{5000, 20000}) {
		t.Fatalf("samples = %v", m.Samples)
	}
	// Ping 1 had no kernel TX stamp: the whole measurement is flagged.
	if m.Flags&store.FlagUserspaceTX == 0 {
		t.Fatal("expected FlagUserspaceTX")
	}
	if m.Flags&store.FlagUserspaceRX != 0 {
		t.Fatal("unexpected FlagUserspaceRX")
	}

	// After finalization replies are dropped and counted.
	col.onRX(3, base.Add(30*time.Millisecond), true)
	if late.Load() != 1 {
		t.Fatalf("late = %d, want 1", late.Load())
	}

	// A clean measurement carries no quality flags beyond timestamping.
	if m.Flags&(store.FlagRawSocket|store.FlagSocketOverflow|store.FlagClockStep) != 0 {
		t.Errorf("unexpected condition flags: %08b", m.Flags)
	}

	// A raw-socket measurement taken while the receive queue overflowed
	// carries both, so loss here is not read as network loss.
	col2 := newCollector(1, &late)
	col2.markSent(0, 9, base)
	m2 := col2.finalize(spec, 1_756_400_100, conditions{rawSocket: true, overflowed: true})
	if m2.Flags&store.FlagRawSocket == 0 {
		t.Error("expected FlagRawSocket")
	}
	if m2.Flags&store.FlagSocketOverflow == 0 {
		t.Error("expected FlagSocketOverflow")
	}
	if m2.Sent != 1 || m2.Received != 0 || len(m2.Samples) != 0 {
		t.Fatalf("loss measurement = %+v", m2)
	}
}

// An ICMP error means the probe was refused rather than ignored, which is a
// different fact about the network than silence and must survive to the row.
func TestCollectorRecordsICMPError(t *testing.T) {
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = 4
	col := newCollector(4, &late)
	base := time.Unix(1_756_400_100, 0)
	for i := range 4 {
		col.markSent(i, uint16(i), base)
	}
	// Two hosts prohibited, one TTL exceeded, one plain timeout.
	col.onICMPError(0, 3, 10)
	col.onICMPError(1, 3, 10)
	col.onICMPError(2, 11, 0)

	m := col.finalize(spec, 1_756_400_100, conditions{})
	if m.Flags&store.FlagICMPError == 0 {
		t.Fatalf("expected FlagICMPError, flags = %08b", m.Flags)
	}
	if m.ICMPErr == nil {
		t.Fatal("no ICMP error recorded")
	}
	// The most frequent error represents the interval.
	if want := store.ICMPError(3, 10); *m.ICMPErr != want {
		t.Errorf("ICMPErr = %#04x, want %#04x (host prohibited)", *m.ICMPErr, want)
	}
	if m.Sent != 4 || m.Received != 0 {
		t.Errorf("sent=%d received=%d, want 4/0", m.Sent, m.Received)
	}
}

// A probe the kernel refuses to transmit is still an attempted, failed probe.
// Dropping it from the count would render an unreachable target as an empty
// graph instead of total loss.
func TestSendFailureCountsAsLoss(t *testing.T) {
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = 3
	col := newCollector(3, &late)
	base := time.Unix(1_756_400_100, 0)
	for i := range 3 {
		col.markSent(i, uint16(i), base)
	}
	col.markSendFailed(1, store.SendReasonSocket)
	col.markSendFailed(2, store.SendReasonSocket)
	col.onRX(0, base.Add(2*time.Millisecond), true)

	m := col.finalize(spec, 1_756_400_100, conditions{})
	if m.Sent != 3 {
		t.Errorf("sent = %d, want 3 (every attempt counts)", m.Sent)
	}
	if m.Received != 1 {
		t.Errorf("received = %d, want 1", m.Received)
	}
	if m.Flags&store.FlagSendFailed == 0 {
		t.Errorf("expected FlagSendFailed, flags = %08b", m.Flags)
	}
}

// A ping that was answered is not an error, even if an error arrives late.
func TestICMPErrorDoesNotOverrideAReply(t *testing.T) {
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = 1
	col := newCollector(1, &late)
	base := time.Unix(1_756_400_100, 0)
	col.markSent(0, 1, base)
	col.onRX(0, base.Add(3*time.Millisecond), true)
	col.onICMPError(0, 3, 1)

	m := col.finalize(spec, 1_756_400_100, conditions{})
	if m.Flags&store.FlagICMPError != 0 {
		t.Error("a replied ping was marked as an ICMP error")
	}
	if m.Received != 1 {
		t.Errorf("received = %d, want 1", m.Received)
	}
}

// A clock step lands directly in kernel (CLOCK_REALTIME) timestamps, so it
// must be detectable; NTP slewing moves both clocks together and must not
// register.
func TestClockStepped(t *testing.T) {
	cases := map[string]struct {
		wall, mono time.Duration
		want       bool
	}{
		"identical":           {60 * time.Second, 60 * time.Second, false},
		"slew, both together": {60*time.Second + 30*time.Millisecond, 60*time.Second + 30*time.Millisecond, false},
		"jitter":              {60*time.Second + 100*time.Microsecond, 60 * time.Second, false},
		// 500ppm of slew over a minute is 30ms of divergence on a platform
		// whose monotonic clock is not slewed alongside. It is not a step.
		"slew, monotonic unaffected": {60*time.Second + 30*time.Millisecond, 60 * time.Second, false},
		"slew over a short bucket":   {10*time.Second + 5*time.Millisecond, 10 * time.Second, false},
		"step forward":               {60*time.Second + 500*time.Millisecond, 60 * time.Second, true},
		"step backward":              {59 * time.Second, 60 * time.Second, true},
		"step on a short bucket":     {10*time.Second + 128*time.Millisecond, 10 * time.Second, true},
	}
	for name, c := range cases {
		if got := clockStepped(c.wall, c.mono); got != c.want {
			t.Errorf("%s: clockStepped(%v, %v) = %v, want %v", name, c.wall, c.mono, got, c.want)
		}
	}
}

// A measurement taken across a real clock step must be flagged rather than
// silently reporting the step as latency.
func TestFinalizeFlagsClockStep(t *testing.T) {
	var late atomic.Int64
	spec := burstSpec()
	spec.Pings = 1
	col := newCollector(1, &late)
	// Move the wall-clock reading without touching the monotonic one, which
	// is exactly the shape of a clock step.
	col.startWall = col.startWall.Add(-30 * time.Second)
	col.markSent(0, 1, time.Now())
	m := col.finalize(spec, 1_756_400_100, conditions{})
	if m.Flags&store.FlagClockStep == 0 {
		t.Fatalf("expected FlagClockStep, flags = %08b", m.Flags)
	}
}

// A kernel transmit stamp is trusted only if it sits between this ping's own
// send and its reply. The kernel labels TX stamps with a counter it keeps
// itself; userspace keeps a parallel one, and a send that fails before the
// kernel assigns an id advances one and not the other. After that every stamp
// lands on a neighbouring packet — frequently a different target, since one
// socket serves every target of a (family, DSCP) pair — and produces an RTT
// that is wrong without looking impossible.
//
// The bounds catch it whatever caused it, and the measurement degrades to
// userspace timestamps with the flag that says so, rather than reporting a
// confident wrong number.
func TestKernelTXStampOutsideItsOwnPingIsRejected(t *testing.T) {
	spec := TargetSpec{TargetID: 1, Host: "192.0.2.1", Pings: 3, TimeoutMS: 1000}
	var late atomic.Int64

	cases := []struct {
		name    string
		txKern  time.Duration // relative to txUser
		wantFlg bool
	}{
		{"plausible", 50 * time.Microsecond, false},
		{"before this ping was sent", -3 * time.Second, true},
		{"after this ping's reply", 900 * time.Millisecond, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCollector(1, &late)
			base := time.Now()
			c.markSent(0, 1, base)
			c.pings[0].txKern = base.Add(tc.txKern)
			c.pings[0].rx = base.Add(5 * time.Millisecond)
			c.pings[0].rxKernel = true

			m := c.finalize(spec, base.Unix(), conditions{})
			got := m.Flags&store.FlagUserspaceTX != 0
			if got != tc.wantFlg {
				t.Fatalf("FlagUserspaceTX = %v, want %v (flags %08b)", got, tc.wantFlg, m.Flags)
			}
			if len(m.Samples) != 1 {
				t.Fatalf("got %d samples, want the reply to still count", len(m.Samples))
			}
			// Whichever stamp was used, the RTT must be the real one measured
			// from this ping's own send.
			if tc.wantFlg && m.Samples[0] != 5000 {
				t.Errorf("RTT = %dµs, want 5000 from the userspace stamp", m.Samples[0])
			}
		})
	}
}

// A negative RTT means the clock moved under us: the sample is not a
// measurement and must not be stored. It used to be clamped to zero, which put
// a fabricated 0 µs reading into the distribution, indistinguishable from a
// real sub-microsecond one and counted as a received reply.
func TestBackwardsClockDropsTheSampleRatherThanStoringZero(t *testing.T) {
	spec := TargetSpec{TargetID: 1, Host: "192.0.2.1", Pings: 1, TimeoutMS: 1000}
	var late atomic.Int64
	c := newCollector(1, &late)

	base := time.Now()
	c.markSent(0, 1, base)
	// The reply appears to arrive before it was sent.
	c.pings[0].rx = base.Add(-2 * time.Second)
	c.pings[0].rxKernel = true

	m := c.finalize(spec, base.Unix(), conditions{})
	if len(m.Samples) != 0 {
		t.Fatalf("stored %v as a measurement; a negative RTT is not one", m.Samples)
	}
	if m.Received != 0 {
		t.Errorf("Received = %d, want 0: nothing usable came back", m.Received)
	}
	if m.Flags&store.FlagClockStep == 0 {
		t.Error("the sample was dropped without recording why")
	}
	if m.Sent != 1 {
		t.Errorf("Sent = %d, want 1: the probe was still attempted", m.Sent)
	}
}

// A bucket cut short by shutdown must not report the probes that were still in
// flight as lost. They were abandoned, not answered for — and counting them
// turned every restart into a loss spike that no network event caused, which
// alert rules would then fire on.
func TestTruncatedIntervalDoesNotInventLoss(t *testing.T) {
	spec := TargetSpec{TargetID: 1, Host: "192.0.2.1", Pings: 3, TimeoutMS: 1000}
	var late atomic.Int64
	c := newCollector(3, &late)

	base := time.Now()
	// One answered, two sent moments ago and still well inside their timeout.
	c.markSent(0, 1, base.Add(-500*time.Millisecond))
	c.pings[0].rx = base.Add(-495 * time.Millisecond)
	c.pings[0].rxKernel = true
	c.markSent(1, 2, base)
	c.markSent(2, 3, base)

	cut := c.finalize(spec, base.Unix(), conditions{truncated: true})
	if cut.Sent != 1 || cut.Received != 1 {
		t.Errorf("truncated: Sent=%d Received=%d, want 1 and 1 — the two in flight were abandoned, not lost",
			cut.Sent, cut.Received)
	}
	if cut.Flags&store.FlagTruncated == 0 {
		t.Error("a truncated interval is not comparable with a whole one and must say so")
	}

	// The same bucket finalized normally does count them: there the timeout
	// really did elapse without a reply.
	c2 := newCollector(3, &late)
	c2.markSent(0, 1, base.Add(-5*time.Second))
	c2.pings[0].rx = base.Add(-4995 * time.Millisecond)
	c2.pings[0].rxKernel = true
	c2.markSent(1, 2, base.Add(-5*time.Second))
	c2.markSent(2, 3, base.Add(-5*time.Second))

	whole := c2.finalize(spec, base.Unix(), conditions{})
	if whole.Sent != 3 || whole.Received != 1 {
		t.Errorf("whole: Sent=%d Received=%d, want 3 and 1 — two really were lost", whole.Sent, whole.Received)
	}
	if whole.Flags&store.FlagTruncated != 0 {
		t.Error("a complete interval was flagged as cut short")
	}
}

// The tolerance is a rate, so it grows with the bucket. That is inherent to
// comparing only the endpoints — over a long span a slow enough step and real
// slew are the same observation — but the window should be no wider than slew
// actually needs, and real slew must never be reported as a step.
func TestClockStepToleranceTracksSlewAndNoMore(t *testing.T) {
	// NTP slews at up to 500ppm. Nothing at or under that is a step, at any
	// span.
	for _, d := range []time.Duration{time.Second, time.Minute, time.Hour} {
		slewed := d + time.Duration(int64(d)*500/1_000_000)
		if clockStepped(slewed, d) {
			t.Errorf("slew at NTP's maximum rate over %v was reported as a step", d)
		}
	}
	// A jump well beyond any slew rate is a step, and the shorter the bucket
	// the smaller the jump that can be told apart.
	if !clockStepped(time.Minute+100*time.Millisecond, time.Minute) {
		t.Error("a 100ms jump over a one-minute bucket was not flagged")
	}
	if !clockStepped(10*time.Second+50*time.Millisecond, 10*time.Second) {
		t.Error("a 50ms jump over a ten-second bucket was not flagged")
	}
}
