package probe

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"smokeng/internal/store"
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
	col.markSendFailed(1)
	col.markSendFailed(2)
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
