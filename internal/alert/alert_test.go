package alert

import "testing"

// Quality flags as the store maps them onto trust, mirrored here so the test
// can express "this measurement is partly our own fault".
type flaw int

const (
	clean flaw = iota
	ourOwnDrops
	clockStepped
)

func lossRule() *Rule {
	return &Rule{
		ID: 1, TargetID: 2, Name: "loss", Metric: MetricLoss, Op: OpGreater,
		Threshold: 20, For: 3, ClearFor: 2, Enabled: true,
	}
}

// m builds a measurement with the given loss, at a 60s interval grid.
func m(step int, sent, received int, f flaw) *Input {
	samples := make([]uint32, received)
	for i := range samples {
		samples[i] = uint32(1000 * (i + 1)) // 1ms, 2ms, ...
	}
	return &Input{
		TS: int64(1_756_400_000 + step*60), Sent: sent, Received: received,
		Samples:     samples,
		LossTrusted: f != ourOwnDrops,
		RTTTrusted:  f != clockStepped,
	}
}

// The point of hysteresis: a single bad interval must not page anyone.
func TestDoesNotFireOnOneBadInterval(t *testing.T) {
	r, st := lossRule(), &State{}
	if tr := Evaluate(r, st, m(0, 10, 5, clean), 60); tr != NoChange {
		t.Fatalf("fired on the first bad interval: %v", tr)
	}
	if tr := Evaluate(r, st, m(1, 10, 10, clean), 60); tr != NoChange {
		t.Fatalf("transition on recovery: %v", tr)
	}
	if st.Streak != 0 {
		t.Errorf("streak = %d, want 0 after a good interval", st.Streak)
	}
}

func TestFiresAndResolvesWithHysteresis(t *testing.T) {
	r, st := lossRule(), &State{}
	// Three consecutive bad intervals: fires on the third, not before.
	for i := range 2 {
		if tr := Evaluate(r, st, m(i, 10, 5, clean), 60); tr != NoChange {
			t.Fatalf("interval %d: %v, want NoChange", i, tr)
		}
	}
	if tr := Evaluate(r, st, m(2, 10, 5, clean), 60); tr != Fired {
		t.Fatalf("third bad interval: %v, want Fired", tr)
	}
	if !st.Firing || st.Since != m(2, 0, 0, clean).TS {
		t.Errorf("state after firing = %+v", st)
	}

	// One good interval is not enough to clear (ClearFor is 2).
	if tr := Evaluate(r, st, m(3, 10, 10, clean), 60); tr != NoChange {
		t.Fatalf("cleared after one good interval: %v", tr)
	}
	if !st.Firing {
		t.Error("stopped firing too early")
	}
	if tr := Evaluate(r, st, m(4, 10, 10, clean), 60); tr != Resolved {
		t.Fatalf("second good interval: %v, want Resolved", tr)
	}
	if st.Firing {
		t.Error("still firing after resolve")
	}
}

// A bad interval part-way through the count restarts it: "consecutive" has to
// mean consecutive, or a flapping link fires eventually regardless.
func TestStreakResetsOnRecovery(t *testing.T) {
	r, st := lossRule(), &State{}
	Evaluate(r, st, m(0, 10, 5, clean), 60)
	Evaluate(r, st, m(1, 10, 5, clean), 60)
	Evaluate(r, st, m(2, 10, 10, clean), 60) // recovery breaks the run
	Evaluate(r, st, m(3, 10, 5, clean), 60)
	if tr := Evaluate(r, st, m(4, 10, 5, clean), 60); tr != NoChange {
		t.Fatalf("fired after an interrupted run: %v", tr)
	}
	if tr := Evaluate(r, st, m(5, 10, 5, clean), 60); tr != Fired {
		t.Fatalf("did not fire once three really were consecutive: %v", tr)
	}
}

// A gap in the data is not evidence either way; counting across it would
// bridge intervals that were never measured.
func TestGapBreaksTheStreak(t *testing.T) {
	r, st := lossRule(), &State{}
	Evaluate(r, st, m(0, 10, 5, clean), 60)
	Evaluate(r, st, m(1, 10, 5, clean), 60)
	// Ten intervals later: a gap, not a third consecutive bad interval.
	if tr := Evaluate(r, st, m(11, 10, 5, clean), 60); tr != NoChange {
		t.Fatalf("fired across a gap: %v", tr)
	}
	if st.Streak != 0 {
		t.Errorf("streak = %d, want 0 after a gap", st.Streak)
	}
}

// Loss that smokeng caused must never page anyone: a measurement taken while
// our own receive queue overflowed is not evidence about the network.
func TestUntrustworthyMeasurementsDoNotCount(t *testing.T) {
	r, st := lossRule(), &State{}
	for i := range 5 {
		if tr := Evaluate(r, st, m(i, 10, 5, ourOwnDrops), 60); tr != NoChange {
			t.Fatalf("interval %d fired on self-inflicted loss: %v", i, tr)
		}
	}

	// A clock step makes RTTs unreliable, so latency rules skip it — but it
	// says nothing about loss, so a loss rule still counts it.
	lat := &Rule{
		ID: 2, Name: "slow", Metric: MetricMedian, Op: OpGreater,
		Threshold: 1, For: 2, ClearFor: 2, Enabled: true,
	}
	ls := &State{}
	for i := range 4 {
		if tr := Evaluate(lat, ls, m(i, 10, 10, clockStepped), 60); tr != NoChange {
			t.Fatalf("latency rule fired across a clock step: %v", tr)
		}
	}
	lossDuringStep := &State{}
	Evaluate(r, lossDuringStep, m(0, 10, 5, clockStepped), 60)
	if lossDuringStep.Streak != 1 {
		t.Errorf("a clock step should not stop loss being counted: streak = %d", lossDuringStep.Streak)
	}
}

func TestMetrics(t *testing.T) {
	// 10 samples: 1ms..10ms, 2 of 12 lost.
	meas := m(0, 12, 10, clean)
	cases := map[Metric]float64{
		MetricLoss:   100 * 2.0 / 12,
		MetricMedian: 6, // sorted[5]
		MetricP95:    10,
		MetricSpread: 10 - 1,
	}
	for metric, want := range cases {
		got, ok := Value(meas, metric)
		if !ok {
			t.Errorf("%s: not available", metric)
			continue
		}
		if diff := got - want; diff > 0.01 || diff < -0.01 {
			t.Errorf("%s = %v, want %v", metric, got, want)
		}
	}

	// An interval with no replies has loss but no latency.
	empty := m(0, 10, 0, clean)
	if v, ok := Value(empty, MetricLoss); !ok || v != 100 {
		t.Errorf("loss on a fully lost interval = %v (ok=%v), want 100", v, ok)
	}
	for _, metric := range []Metric{MetricMedian, MetricP95, MetricSpread} {
		if _, ok := Value(empty, metric); ok {
			t.Errorf("%s reported a value with no samples", metric)
		}
	}
}

func TestValidate(t *testing.T) {
	valid := lossRule()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
	bad := map[string]func(*Rule){
		"unknown metric": func(r *Rule) { r.Metric = "jitter" },
		"unknown op":     func(r *Rule) { r.Op = "~" },
		"no name":        func(r *Rule) { r.Name = "" },
		"loss over 100":  func(r *Rule) { r.Threshold = 150 },
		"for zero":       func(r *Rule) { r.For = 0 },
		"clear zero":     func(r *Rule) { r.ClearFor = 0 },
	}
	for name, mutate := range bad {
		r := lossRule()
		mutate(r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
