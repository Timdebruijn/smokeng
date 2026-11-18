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
// loss discount it. Protects against widening the exclusion to latency.
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

// Under exclude a loss rule reads a flagged interval as one without loss, so it
// counts towards clearing: an alert that fired before the detector had seen
// enough resolves once the flags begin, however long the limiter keeps hitting.
// This is the case the setting exists for; treating a flagged interval as
// untrusted instead left that alert firing for good.
func TestPositionalLossClearsAFiringLossAlert(t *testing.T) {
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
	// The limiter keeps hitting; from here on the intervals are flagged.
	for ; i < 5; i++ {
		if tr := s.Step(loss, lossyInput(i, true), nil, app); tr != NoChange {
			t.Fatalf("interval %d: transition %v before clear_intervals flagged intervals", i, tr)
		}
	}
	if tr := s.Step(loss, lossyInput(i, true), nil, app); tr != Resolved {
		t.Fatalf("three flagged intervals in a row did not resolve the alert: %v, state %+v", tr, s.State)
	}
	// And it stays resolved while the same loss goes on.
	for i++; i < 30; i++ {
		if tr := s.Step(loss, lossyInput(i, true), nil, app); tr != NoChange {
			t.Fatalf("interval %d: transition %v on flagged loss after resolving", i, tr)
		}
	}
	// With flag_only the same intervals keep it firing.
	var f Series
	for i := range 30 {
		if f.Step(loss, lossyInput(i, i >= 3), nil, &Applicable{IntervalS: 60}) == Resolved {
			t.Fatalf("flag_only: resolved at interval %d on flagged loss", i)
		}
	}
	if !f.State.Firing {
		t.Error("flag_only: not firing on thirty lossy intervals")
	}
}

// A flagged interval is a non-matching one, so it also breaks a streak that was
// building towards firing: two lossy intervals, a flagged one and one more are
// not three in a row. And an interval that is not flagged is read as it is,
// whatever came before it: loss the detector does not put down to the limiter
// still fires on a target that is rate limited.
func TestPositionalLossBreaksAStreakAndHidesNothingElse(t *testing.T) {
	loss := ruleOn(MetricLoss, 5)
	app := &Applicable{IntervalS: 60, ExcludePositionalLoss: true}
	var s Series
	for i, flagged := range []bool{false, false, true, false, false} {
		if tr := s.Step(loss, lossyInput(i, flagged), nil, app); tr != NoChange {
			t.Fatalf("interval %d: transition %v, want none yet", i, tr)
		}
	}
	if tr := s.Step(loss, lossyInput(5, false), nil, app); tr != Fired {
		t.Errorf("three unflagged lossy intervals after a flagged one: %v, want Fired", tr)
	}
}

// A rule reads a flagged interval as 0% loss, but the figure its state carries
// into a notification is the loss that was measured. Also pins that the caller's
// input is left alone, since other rules on the series share it.
func TestPositionalLossCarriesTheMeasuredValue(t *testing.T) {
	loss := ruleOn(MetricLoss, 5)
	app := &Applicable{IntervalS: 60, ExcludePositionalLoss: true}
	var s Series
	s.Step(loss, lossyInput(0, false), nil, app)
	if s.State.Value != 15 {
		t.Fatalf("setup: value read from an unflagged interval = %v, want 15", s.State.Value)
	}
	in := lossyInput(1, true)
	s.Step(loss, in, nil, app)
	if s.State.Value != 15 {
		t.Errorf("value carried from a flagged interval = %v, want the measured 15", s.State.Value)
	}
	if in.Received != 17 {
		t.Errorf("the caller's input was changed: received %d, want 17", in.Received)
	}
	var f Series
	f.Step(loss, in, nil, &Applicable{IntervalS: 60})
	if f.State.Value != 15 {
		t.Errorf("flag_only: value = %v, want 15", f.State.Value)
	}
}

// An interval that does not update the value on any other path must not on a
// flagged one either: an untrusted interval and a disabled rule keep the last value.
func TestPositionalLossKeepsTheValueWhereEvaluateDoes(t *testing.T) {
	app := &Applicable{IntervalS: 60, ExcludePositionalLoss: true}
	loss := ruleOn(MetricLoss, 5)
	var s Series
	s.Step(loss, lossyInput(0, false), nil, app)
	untrusted := lossyInput(1, true)
	untrusted.LossTrusted = false
	s.Step(loss, untrusted, nil, app)
	if s.State.Value != 15 {
		t.Errorf("untrusted flagged interval changed the value to %v", s.State.Value)
	}
	off := ruleOn(MetricLoss, 5)
	off.Enabled = false
	var d Series
	d.Step(off, lossyInput(0, true), nil, app)
	if d.State.Value != 0 {
		t.Errorf("disabled rule stored value %v", d.State.Value)
	}
}

// The resolve transition of an alert that clears on flagged intervals reports
// the measured loss, and happens at the same interval as it always did (the
// third flagged one): only the figure changed, not the hysteresis.
func TestPositionalLossResolveReportsMeasuredLoss(t *testing.T) {
	loss := ruleOn(MetricLoss, 5)
	app := &Applicable{IntervalS: 60, ExcludePositionalLoss: true}
	var s Series
	for i := range 3 {
		s.Step(loss, lossyInput(i, false), nil, app)
	}
	resolvedAt := 0
	for i := 3; i < 10 && resolvedAt == 0; i++ {
		if s.Step(loss, lossyInput(i, true), nil, app) == Resolved {
			resolvedAt = i
			if s.State.Value != 15 {
				t.Errorf("resolve carries value %v, want the measured 15", s.State.Value)
			}
		}
	}
	if resolvedAt != 5 {
		t.Errorf("resolved at interval %d, want 5 (clear_for flagged intervals)", resolvedAt)
	}
}
