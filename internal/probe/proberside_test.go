package probe

import (
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
)

// A truncated bucket carries FlagTruncated even when every sent probe answered
// before the cut — the flag is a property of the bucket, not of a probe left
// in flight. Without it a quarter-width distribution is stored as a whole one.
func TestTruncatedBucketAlwaysFlagged(t *testing.T) {
	spec := TargetSpec{TargetID: 1, Pings: 20, TimeoutMS: 1000, Mode: "burst"}
	var late atomic.Int64
	col := newCollector(spec.Pings, &late)
	now := time.Now()
	for i := 0; i < 5; i++ { // 5 of 20 sent, all promptly answered
		col.markSent(i, uint16(i), now)
		col.onRX(i, now.Add(time.Millisecond), false)
	}
	m := col.finalize(spec, 0, conditions{truncated: true})
	if m.Flags&store.FlagTruncated == 0 {
		t.Fatalf("flags 0x%x lack FlagTruncated; a cut-short interval was stored as complete", m.Flags)
	}
	if m.Received != 5 || m.Sent != 5 {
		t.Fatalf("got %d/%d, want the 5 answered probes counted and the abandoned 15 excluded",
			m.Received, m.Sent)
	}
}

// isLocalResourceError separates the prober's own exhaustion (ours) from a
// statement about the target (loss). Getting this wrong in either direction is
// a wrong measurement: a healthy service drawn as down, or a real refusal
// hidden as a send failure.
func TestLocalResourceErrorClassification(t *testing.T) {
	ours := []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.EADDRNOTAVAIL, syscall.ENOBUFS, syscall.ENOMEM}
	for _, e := range ours {
		if !isLocalResourceError(e) {
			t.Errorf("%v (%d) should count as a local send failure", e, int(e))
		}
	}
	// These describe the target or the path, so they must stay loss.
	theirs := []syscall.Errno{syscall.ECONNREFUSED, syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH}
	for _, e := range theirs {
		if isLocalResourceError(e) {
			t.Errorf("%v (%d) describes the target and must stay loss, not a send failure", e, int(e))
		}
	}
}
