package alert

import "testing"

// lossyInput is an interval that lost 3 of 20 probes, flagged or not.
func lossyInput(i int, flagged bool) *Input {
	s := make([]uint32, 17)
	for j := range s {
		s[j] = 5000
	}
	return &Input{
		TS: 1_756_400_000 + int64(i)*60, Sent: 20, Received: 17, Samples: s,
		LossTrusted: true, RTTTrusted: true, PositionalLoss: flagged,
	}
}

func ruleOn(m Metric, thr float64) *Rule {
	return &Rule{ID: 1, Name: "r", Metric: m, Op: OpGreater, Threshold: thr, For: 3, ClearFor: 3, Enabled: true}
}

// run feeds n intervals (flagged or not) and returns the 1-based interval a
// rule fired at, or 0.
func run(r *Rule, app *Applicable, n int, flagged bool) int {
	var s Series
	for i := range n {
		if s.Step(r, lossyInput(i, flagged), nil, app) == Fired {
			return i + 1
		}
	}
	return 0
}

// A loss rule must not fire on loss the target's own rate limiter produces,
// under the default; flag_only must leave the rule as it was; and intervals that
// are not flagged must still fire under exclude. Protects the whole point of the
// setting and its off switch.
func TestPositionalLossExcludedFromLossRules(t *testing.T) {
	loss := ruleOn(MetricLoss, 5)
	excl := &Applicable{IntervalS: 60, ExcludePositionalLoss: true}
	flag := &Applicable{IntervalS: 60}
	if got := run(loss, excl, 6, true); got != 0 {
		t.Errorf("exclude: fired at interval %d on flagged loss", got)
	}
	if got := run(loss, flag, 6, true); got != 3 {
		t.Errorf("flag_only: fired at %d, want 3", got)
	}
	if got := run(loss, excl, 6, false); got != 3 {
		t.Errorf("exclude, unflagged loss: fired at %d, want 3", got)
	}
}

// The round-trip times of a rate-limited interval are real, so only rules on
// loss leave it out. Protects against widening the exclusion to latency.
func TestPositionalLossDoesNotTouchLatencyRules(t *testing.T) {
	p95 := ruleOn(MetricP95, 1) // 5 ms > 1 ms
	for name, app := range map[string]*Applicable{
		"exclude":   {IntervalS: 60, ExcludePositionalLoss: true},
		"flag_only": {IntervalS: 60},
	} {
		if got := run(p95, app, 6, true); got != 3 {
			t.Errorf("%s: p95 rule fired at %d over flagged intervals, want 3", name, got)
		}
	}
}

// A flagged interval counts neither towards firing nor towards clearing, and
// resets the streak, as an interval with dropped replies does: a firing loss
// alert must not clear on flagged intervals. Protects the "both ways" half.
func TestPositionalLossNeitherClearsNorBuildsStreak(t *testing.T) {
	loss := ruleOn(MetricLoss, 5)
	app := &Applicable{IntervalS: 60, ExcludePositionalLoss: true}
	var s Series
	i := 0
	for ; i < 3; i++ {
		s.Step(loss, lossyInput(i, false), nil, app)
	}
	if !s.State.Firing {
		t.Fatal("setup: not firing after three unflagged lossy intervals")
	}
	// Clean flagged intervals would clear it if they counted; they are skipped.
	for ; i < 12; i++ {
		in := lossyInput(i, true)
		in.Received, in.Samples = 20, in.Samples[:0]
		if s.Step(loss, in, nil, app) == Resolved || !s.State.Firing {
			t.Fatalf("a flagged interval cleared the alert at %d", i)
		}
	}
	// Two clear intervals, a flagged one, then one more: the flagged one reset
	// the streak, so three clears are not yet in a row.
	clean := func(i int, flagged bool) Transition {
		in := lossyInput(i, flagged)
		in.Received = 20
		return s.Step(loss, in, nil, app)
	}
	clean(i, false)
	clean(i+1, false)
	clean(i+2, true)
	if clean(i+3, false) == Resolved {
		t.Error("the streak survived a flagged interval")
	}
	clean(i+4, false)
	if clean(i+5, false) != Resolved {
		t.Error("three clean unflagged intervals in a row did not resolve it")
	}
}
