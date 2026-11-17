package losspattern

import (
	"math/rand/v2"
	"reflect"
	"testing"
)

const probes = 20

// A source makes one interval's losses. The generators below are the cases
// the detector exists for, and the ones it must leave alone.
type source func(rng *rand.Rand) []bool

// some applies a pattern to a share of the intervals and leaves the rest clean.
func some(share float64, at ...int) source {
	return func(rng *rand.Rand) []bool {
		lost := make([]bool, probes)
		if rng.Float64() < share {
			for _, i := range at {
				lost[i] = true
			}
		}
		return lost
	}
}

func span(from, to int) []int {
	var out []int
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// bernoulli loses each probe independently.
func bernoulli(p float64) source {
	return func(rng *rand.Rand) []bool {
		lost := make([]bool, probes)
		for i := range lost {
			lost[i] = rng.Float64() < p
		}
		return lost
	}
}

// gilbertElliott is bursty loss: a two-state channel that loses everything
// while bad. toBad and toGood are the chances of changing state per probe, so
// 1/toGood is the mean burst length in probes. Each interval starts in the
// channel's stationary state, as an interval a minute after the last one does.
func gilbertElliott(toBad, toGood float64) source {
	return func(rng *rand.Rand) []bool {
		lost := make([]bool, probes)
		bad := rng.Float64() < toBad/(toBad+toGood)
		for i := range lost {
			lost[i] = bad
			if bad {
				bad = rng.Float64() >= toGood
			} else {
				bad = rng.Float64() < toBad
			}
		}
		return lost
	}
}

// both overlays two sources: rate limiting with network loss on top.
func both(a, b source) source {
	return func(rng *rand.Rand) []bool {
		x, y := a(rng), b(rng)
		for i := range x {
			x[i] = x[i] || y[i]
		}
		return x
	}
}

func partial(lost []bool) bool {
	n := 0
	for _, l := range lost {
		if l {
			n++
		}
	}
	return n > 0 && n < len(lost)
}

// window draws a day of intervals and keeps those with partial loss, which is
// what the store records a mask for, a minute apart from each other at least.
func window(rng *rand.Rand, src source) []Interval {
	var out []Interval
	for i := range MaxAgeS / 60 {
		if lost := src(rng); partial(lost) {
			out = append(out, Interval{TS: int64(i) * 60, Lost: lost})
		}
	}
	return out
}

// flagged counts how many of n independent windows the detector flags.
func flagged(seed uint64, n int, src source) (tested, hits int) {
	rng := rand.New(rand.NewPCG(seed, 1))
	for range n {
		v := Test(window(rng, src), false)
		if v.Tested {
			tested++
		}
		if v.Flagged {
			hits++
		}
	}
	return
}

// Protects what the detector is for: loss that sits at fixed places in the
// send order is flagged, whichever places those are. A token bucket that
// empties part-way through a burst, one position that never gets an answer,
// every fourth probe, and the first probe of a burst (a neighbour entry that
// has to be resolved first).
func TestFlagsLossAtFixedPositions(t *testing.T) {
	for name, src := range map[string]source{
		"token bucket, tail of the burst in 30% of intervals": some(0.3, span(15, 20)...),
		"token bucket, tail in 10% of intervals":              some(0.1, span(15, 20)...),
		"token bucket, last two probes in 20%":                some(0.2, 18, 19),
		"one position in 30%":                                 some(0.3, 7),
		"one position in 30%, with 1% random loss":            both(some(0.3, 7), bernoulli(0.01)),
		"every fourth probe in 10%":                           some(0.1, 3, 7, 11, 15, 19),
		"the first probe in 20%":                              some(0.2, 0),
	} {
		tested, hits := flagged(1, 200, src)
		if tested != 200 || hits < 190 {
			t.Errorf("%s: flagged %d of %d windows (tested %d), want nearly all", name, hits, 200, tested)
		}
	}
}

// Protects against the false positive that matters. Real network loss is
// bursty, a burst cut by the edge of an interval looks like loss at the start
// or the end of it, and a few of those on the same side look exactly like a
// token bucket. The target is at most 0.1% of windows flagged at the default
// threshold, for independent loss and for bursts from three to twenty probes
// long. The seed is fixed; the measured rates are in the failure message and
// in the log (go test -v).
func TestDoesNotFlagNetworkLoss(t *testing.T) {
	const windows = 10000
	for _, c := range []struct {
		name string
		src  source
	}{
		{"independent, 3%", bernoulli(0.03)},
		{"bursty, mean burst 3 probes", gilbertElliott(0.01, 0.30)},
		{"bursty, mean burst 10 probes", gilbertElliott(0.01, 0.10)},
		{"bursty, mean burst 20 probes", gilbertElliott(0.005, 0.05)},
		{"bursty and heavy, mean burst 7 probes", gilbertElliott(0.03, 0.15)},
	} {
		tested, hits := flagged(2, windows, c.src)
		rate := 100 * float64(hits) / float64(tested)
		t.Logf("%-40s %d of %d windows flagged (%.3f%%)", c.name, hits, tested, rate)
		if tested < windows*9/10 {
			t.Errorf("%s: only %d of %d windows had enough loss to test, so the rate proves little", c.name, tested, windows)
		}
		if rate > 0.1 {
			t.Errorf("%s: %.3f%% of windows flagged, want at most 0.1%%", c.name, rate)
		}
	}
}

// Protects the mixed case from being read as a promise: rate limiting under
// heavy bursty loss is harder to see, and this pins how much of it is still
// caught so that a change to the test that loses it does not go unnoticed.
func TestRateLimitingUnderBurstyLoss(t *testing.T) {
	strong := both(some(0.3, span(15, 20)...), gilbertElliott(0.01, 0.10))
	if _, hits := flagged(3, 200, strong); hits < 180 {
		t.Errorf("tail loss in 30%% of intervals under long bursts: flagged %d of 200, want most", hits)
	}
}

func tailEvery(n int, lost ...int) []Interval {
	var out []Interval
	for i := range n {
		iv := Interval{TS: int64(i) * 60, Lost: make([]bool, probes)}
		for _, p := range lost {
			iv.Lost[p] = true
		}
		out = append(out, iv)
	}
	return out
}

// Protects the edge cases a real outage produces. An outage that ends inside
// one interval and one that starts inside another are the two halves of the
// same thing and must not add up to a pattern; neither must a single interval,
// however much of it was lost.
func TestOutagesAreNotPatterns(t *testing.T) {
	// One big outage in one interval.
	one := tailEvery(1, span(3, 20)...)
	if v := Test(one, true); v.Tested || v.Flagged {
		t.Errorf("a single lossy interval was judged: %+v", v)
	}

	// Outages across the boundary of two intervals, twenty times: the tail of
	// one interval and the head of the next, in equal measure.
	var straddle []Interval
	for i := range 20 {
		tail := Interval{TS: int64(2*i) * 60, Lost: make([]bool, probes)}
		head := Interval{TS: int64(2*i+1) * 60, Lost: make([]bool, probes)}
		for p := 14; p < probes; p++ {
			tail.Lost[p] = true
			head.Lost[probes-1-p] = true
		}
		straddle = append(straddle, tail, head)
	}
	if v := Test(straddle, true); !v.Tested || v.Flagged {
		t.Errorf("outages straddling interval boundaries were flagged or not tested: %+v", v)
	}

	// The same tails without their other halves are a pattern.
	if v := Test(tailEvery(20, span(14, 20)...), true); !v.Flagged {
		t.Errorf("twenty intervals that all lost their tail were not flagged: %+v", v)
	}
}

// Protects the blind spot from being forgotten: a pattern that is its own
// mirror image is invisible to this test, and says so by not being flagged.
func TestMirrorSymmetricPatternIsNotSeen(t *testing.T) {
	if v := Test(tailEvery(30, 0, probes-1), true); v.Flagged {
		t.Errorf("first and last probe lost together was flagged: %+v", v)
	}
}

// Protects "do not mix configurations": positions in a 20-probe burst do not
// mean what they mean in a 10-probe one or in spread mode, so the window ends
// where the settings changed, and so does it a day back.
func TestWindowStopsWhereTheMeasurementChanged(t *testing.T) {
	recent := tailEvery(8, span(15, 20)...) // too few on their own
	shift := func(ivs []Interval, by int64) []Interval {
		for i := range ivs {
			ivs[i].TS += by
		}
		return ivs
	}

	older := tailEvery(30, span(15, 20)...)
	if v := Test(append(older, shift(recent, 30*60)...), true); !v.Flagged || v.Intervals != 38 {
		t.Fatalf("38 like intervals: flagged=%v over %d", v.Flagged, v.Intervals)
	}

	fewerProbes := tailEvery(30, span(15, 20)...)
	for i := range fewerProbes {
		fewerProbes[i].Lost = fewerProbes[i].Lost[10:]
	}
	if v := Test(append(fewerProbes, recent...), true); v.Tested || v.Intervals != 8 {
		t.Errorf("a window reached past a change in probe count: %+v", v)
	}

	spread := tailEvery(30, span(15, 20)...)
	for i := range spread {
		spread[i].Spread = true
	}
	if v := Test(append(spread, recent...), true); v.Tested || v.Intervals != 8 {
		t.Errorf("a window reached past a change of probe mode: %+v", v)
	}

	stale := tailEvery(30, span(15, 20)...)
	if v := Test(append(stale, shift(tailEvery(8, span(15, 20)...), MaxAgeS+40*60)...), true); v.Tested || v.Intervals != 8 {
		t.Errorf("a window reached back more than a day: %+v", v)
	}

	// No more than Window intervals, however many there are.
	if v := Test(tailEvery(200, span(15, 20)...), true); v.Intervals != Window {
		t.Errorf("window held %d intervals, want %d", v.Intervals, Window)
	}
}

// Protects the minimum evidence: too little loss is not judged, and the
// verdict says what was missing rather than passing it off as clean.
func TestTooLittleEvidenceIsNotJudged(t *testing.T) {
	if v := Test(nil, true); v.Tested || v.Why == "" {
		t.Errorf("no history: %+v", v)
	}
	if v := Test(tailEvery(10, span(15, 20)...), true); v.Tested || v.Flagged || v.Why == "" {
		t.Errorf("ten lopsided intervals cannot reach the threshold and must not be tested: %+v", v)
	}
	if v := Test(tailEvery(MinIntervals, span(15, 20)...), true); !v.Tested || !v.Flagged {
		t.Errorf("eleven intervals that all lost their tail: %+v", v)
	}
	// Intervals with no loss, or with all of it, are not evidence either way.
	all := tailEvery(30, span(0, 20)...)
	if v := Test(all, true); v.Tested || v.Intervals != 0 {
		t.Errorf("total loss was counted as evidence: %+v", v)
	}
}

// Protects reproducibility: the same history gives the same verdict on every
// evaluation, and stopping early changes how exactly p is known, never which
// way the verdict falls.
func TestVerdictIsDeterministicAndEarlyStopAgrees(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 4))
	for _, src := range []source{
		some(0.2, span(15, 20)...), gilbertElliott(0.01, 0.10), bernoulli(0.03),
		both(some(0.1, span(15, 20)...), gilbertElliott(0.01, 0.30)),
	} {
		for range 50 {
			w := window(rng, src)
			a, b := Test(w, true), Test(w, true)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("two evaluations of one window differ:\n%+v\n%+v", a, b)
			}
			if early := Test(w, false); early.Flagged != a.Flagged || early.P > a.P {
				t.Fatalf("early stop disagrees: early %+v, full %+v", early, a)
			}
		}
	}
}

// Protects the wording a badge and the diagnosis command are built from.
func TestWhereNamesThePositions(t *testing.T) {
	for want, lost := range map[string][]int{
		"16–20":            span(15, 20),
		"8":                {7},
		"4, 8, 12, 16, 20": {3, 7, 11, 15, 19},
		"1":                {0},
	} {
		if got := Test(tailEvery(20, lost...), true).Where(); got != want {
			t.Errorf("lost %v: Where() = %q, want %q", lost, got, want)
		}
	}
	if got := (Verdict{}).Where(); got != "" {
		t.Errorf("empty verdict: Where() = %q", got)
	}
}

// What one evaluation costs, for a window that is flagged (every round runs)
// and for ordinary bursty loss (it stops early), at 20 probes and at 1000.
func BenchmarkTest(b *testing.B) {
	rng := rand.New(rand.NewPCG(5, 5))
	wide := func(n int) []Interval {
		ivs := make([]Interval, Window)
		for i := range ivs {
			ivs[i] = Interval{TS: int64(i) * 60, Lost: make([]bool, n)}
			for p := n * 3 / 4; p < n; p++ {
				ivs[i].Lost[p] = true
			}
		}
		return ivs
	}
	for name, w := range map[string][]Interval{
		"flagged/20 probes":   wide(20),
		"flagged/1000 probes": wide(1000),
		"bursty/20 probes":    window(rng, gilbertElliott(0.01, 0.10)),
	} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				Test(w, false)
			}
		})
	}
}

// bruteP is the p-value worked out the long way, sharing nothing with Test:
// every way of mirroring the intervals, each mirrored by reversing its slice,
// and the profile's distance from its mirror image counted from the profile.
func bruteP(ivs []Interval) float64 {
	n := len(ivs[0].Lost)
	stat := func(pick int) int {
		counts := make([]int, n)
		for j, iv := range ivs {
			for i, l := range iv.Lost {
				if !l {
					continue
				}
				if pick>>j&1 == 1 {
					counts[n-1-i]++
				} else {
					counts[i]++
				}
			}
		}
		s := 0
		for i := 0; i < n/2; i++ {
			d := counts[i] - counts[n-1-i]
			s += d * d
		}
		return s
	}
	observed, asExtreme := stat(0), 0
	for pick := range 1 << len(ivs) {
		if stat(pick) >= observed {
			asExtreme++
		}
	}
	return float64(asExtreme) / float64(int(1)<<len(ivs))
}

// Protects the exact count that small windows use, which is where a pattern is
// first caught: it must be the p-value of the test as defined, not something
// near it. Checked against a brute-force count on windows of eleven to
// fourteen intervals, with an odd probe count too, where the middle probe is
// its own mirror image.
func TestExactCountMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(6, 6))
	for _, n := range []int{20, 7} {
		for m := 11; m <= 14; m++ {
			for range 5 {
				var ivs []Interval
				for len(ivs) < m {
					lost := make([]bool, n)
					for i := range lost {
						// Mostly towards the tail, so that p is small enough for
						// a wrong count to show, with some noise elsewhere.
						lost[i] = rng.Float64() < 0.05+0.5*float64(i)/float64(n)
					}
					if partial(lost) && !reflect.DeepEqual(lost, mirrored(lost)) {
						ivs = append(ivs, Interval{TS: int64(len(ivs)) * 60, Lost: lost})
					}
				}
				v := Test(ivs, true)
				if want := bruteP(ivs); !v.Tested || v.AtLeast || v.P != want {
					t.Fatalf("%d probes, %d intervals: P = %v (tested %v, bound %v), brute force says %v",
						n, m, v.P, v.Tested, v.AtLeast, want)
				}
			}
		}
	}
}

func mirrored(lost []bool) []bool {
	out := make([]bool, len(lost))
	for i, l := range lost {
		out[len(lost)-1-i] = l
	}
	return out
}
