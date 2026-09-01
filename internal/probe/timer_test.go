package probe

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two irtt targets probing at once, which is an ordinary configuration — and
// the shape a SmokePing import produces, since its IRTT probe graphs one figure
// per target.
//
// Before each client got its own timer this hung: one session finished and the
// other never returned, wedged in the pacing state both were writing to, and
// probeIRTT's deadline did not free it because what was stuck was not waiting
// on that context. The test times out rather than failing an assertion, which
// is the honest shape for a deadlock.
func TestTwoIRTTTargetsConcurrently(t *testing.T) {
	pA, pB := irttServer(t), irttServer(t)
	run := func(port, id int) {
		spec := TargetSpec{
			TargetID: int64(id), Host: "127.0.0.1", Family: "v4",
			IntervalS: 10, Pings: 6, Mode: "burst", BurstGapMS: 20,
			TimeoutMS: 1000, PacketSize: 64, ProbeType: "irtt", ProbePort: port,
		}
		var late atomic.Int64
		col := newCollector(spec.Pings, &late)
		probeIRTT(context.Background(), col, netip.MustParseAddr("127.0.0.1"), spec,
			time.Now().Add(5*time.Second))
		m := col.finalize(spec, 0, conditions{})
		if m.Received == 0 {
			t.Errorf("target %d received nothing from a local irtt server", id)
		}
		t.Logf("target %d: %d/%d flags=%d", id, m.Received, m.Sent, m.Flags)
	}
	var wg sync.WaitGroup
	for i, p := range []int{pA, pB} {
		wg.Add(1)
		go func() { defer wg.Done(); run(p, i+1) }()
	}
	wg.Wait()
}
