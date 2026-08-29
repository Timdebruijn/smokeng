package alert

import (
	"testing"
	"time"
)

// Since is when the condition started holding, and Evaluate clears it as an
// alert resolves — so reading it unconditionally stamped every resolved
// transition with the zero time, which is the year 1. The log is a record of
// when things happened; an entry dated -62135596800 is not one.
func TestResolvedTransitionsAreStampedWhenTheyHappened(t *testing.T) {
	rule := &Rule{ID: 1, Name: "loss", Metric: MetricLoss, Op: OpGreater, Threshold: 10}
	before := time.Now().Unix()

	for _, a := range []Alert{
		{Rule: rule, Firing: false},                                    // resolved: Since cleared
		{Rule: rule, Firing: true, Since: time.Unix(1_700_000_000, 0)}, // firing: Since kept
	} {
		ts := transitionTS(a, time.Now().Unix())
		if ts < 0 {
			t.Fatalf("firing=%v stamped %d, which is before the epoch", a.Firing, ts)
		}
		if a.Firing && ts != 1_700_000_000 {
			t.Errorf("a firing transition lost the moment the condition started: %d", ts)
		}
		if !a.Firing && ts < before {
			t.Errorf("a resolved transition is dated %d, before this test started", ts)
		}
	}
}
