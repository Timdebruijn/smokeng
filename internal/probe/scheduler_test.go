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

	m := col.finalize(spec, 1_756_400_100, false)
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

	// A raw-socket measurement carries the raw flag.
	col2 := newCollector(1, &late)
	col2.markSent(0, 9, base)
	m2 := col2.finalize(spec, 1_756_400_100, true)
	if m2.Flags&store.FlagRawSocket == 0 {
		t.Fatal("expected FlagRawSocket")
	}
	if m2.Sent != 1 || m2.Received != 0 || len(m2.Samples) != 0 {
		t.Fatalf("loss measurement = %+v", m2)
	}
}
