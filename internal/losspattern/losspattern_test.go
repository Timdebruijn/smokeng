package losspattern

import (
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
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

// carriedOver is the same channel without the fresh start: its state runs on
// from the last probe of one interval into the first of the next, as it does
// when probes are spread across the interval rather than sent as a burst. A
// burst of loss then straddles the boundary.
func carriedOver(toBad, toGood float64) source {
	bad := false
	return func(rng *rand.Rand) []bool {
		lost := make([]bool, probes)
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

// window draws intervals a minute apart and keeps those with partial loss,
// which is what the store records a mask for, until there are as many as the
// detector looks at or a day has passed. Drawing the rest of the day would only
// make intervals the detector never reaches, and it was most of what these
// tests cost: under the race detector, minutes.
func window(rng *rand.Rand, src source) []Interval {
	var out []Interval
	for i := 0; i < MaxAgeS/60 && len(out) < Window; i++ {
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
		n := 30
		if raceBuild {
			n = 5
		}
		tested, hits := flagged(1, n, src)
		if tested != n || hits < n-2 {
			t.Errorf("%s: flagged %d of %d windows (tested %d), want nearly all", name, hits, n, tested)
		}
	}
}

// Protects against the false positive that matters. Real network loss is
// bursty, a burst cut by the edge of an interval looks like loss at the start
// or the end of it, and a few of those on the same side look exactly like a
// token bucket. The target is at most 0.1% of windows flagged at the default
// threshold, for independent loss, for bursts from three to twenty probes long,
// and for bursts that carry over from one interval into the next.
//
// The target is asserted on all the windows together, where a fixed seed is a
// fair sample of it: fifty thousand windows at the threshold's nominal 0.05%
// expect about twenty-five flags against the fifty that 0.1% allows. One
// channel alone is ten thousand windows and five expected flags, too few to
// hold to 0.1%, so each is held to 0.2%, which a null that breaks on that
// channel still fails by a wide margin. The measured rates are in the log
// (go test -v).
func TestDoesNotFlagNetworkLoss(t *testing.T) {
	windows := 10000
	if raceBuild {
		windows = 200
	}
	allTested, allHits := 0, 0
	for _, c := range []struct {
		name string
		src  source
	}{
		{"independent, 3%", bernoulli(0.03)},
		{"bursty, mean burst 3 probes", gilbertElliott(0.01, 0.30)},
		{"bursty, mean burst 10 probes", gilbertElliott(0.01, 0.10)},
		{"bursty, mean burst 20 probes", gilbertElliott(0.005, 0.05)},
		{"bursty and heavy, mean burst 7 probes", gilbertElliott(0.03, 0.15)},
		{"bursty across intervals, mean burst 10", carriedOver(0.01, 0.10)},
	} {
		tested, hits := flagged(2, windows, c.src)
		rate := 100 * float64(hits) / float64(tested)
		t.Logf("%-40s %d of %d windows flagged (%.3f%%)", c.name, hits, tested, rate)
		if tested < windows*9/10 {
			t.Errorf("%s: only %d of %d windows had enough loss to test, so the rate proves little", c.name, tested, windows)
		}
		if rate > 0.2 && !raceBuild {
			t.Errorf("%s: %.3f%% of windows flagged, want at most 0.2%% on one channel", c.name, rate)
		}
		allTested, allHits = allTested+tested, allHits+hits
	}
	rate := 100 * float64(allHits) / float64(allTested)
	t.Logf("%-40s %d of %d windows flagged (%.3f%%)", "all of the above", allHits, allTested, rate)
	if rate > 0.1 && !raceBuild {
		t.Errorf("%.3f%% of all windows flagged, want at most 0.1%%", rate)
	}
}

// Protects the mixed case from being read as a promise: rate limiting under
// heavy bursty loss is harder to see, and this pins how much of it is still
// caught so that a change to the test that loses it does not go unnoticed.
func TestRateLimitingUnderBurstyLoss(t *testing.T) {
	strong := both(some(0.3, span(15, 20)...), gilbertElliott(0.01, 0.10))
	if raceBuild {
		t.Skip("a rate over forty windows; run without the race detector")
	}
	if _, hits := flagged(3, 40, strong); hits < 34 {
		t.Errorf("tail loss in 30%% of intervals under long bursts: flagged %d of 40, want most", hits)
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
	if v := Test(tailEvery(MinIntervals-1, span(15, 20)...), true); v.Tested {
		t.Errorf("one interval short of the minimum was tested: %+v", v)
	}
	if v := Test(tailEvery(MinIntervals, span(15, 20)...), true); !v.Tested || !v.Flagged {
		t.Errorf("the minimum number of intervals, all having lost their tail: %+v", v)
	}
	// An interval whose loss is its own mirror image leans neither way and is
	// not evidence: with it counted, a window would be tested on fewer
	// lopsided intervals than can reach the threshold.
	mixed := append(tailEvery(20, 0, probes-1), tailEvery(MinIntervals-1, span(15, 20)...)...)
	for i := range mixed {
		mixed[i].TS = int64(i) * 60
	}
	if v := Test(mixed, true); v.Tested || v.Intervals != 20+MinIntervals-1 {
		t.Errorf("symmetric intervals were counted as lopsided ones: %+v", v)
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
		n := 15
		if raceBuild {
			n = 2
		}
		for range n {
			w := window(rng, src)
			a, b := Test(w, true), Test(w, true)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("two evaluations of one window differ:\n%+v\n%+v", a, b)
			}
			// Stopped early because it could not be flagged, P is a lower bound;
			// settled by the bound, an upper one.
			early := Test(w, false)
			if early.Flagged != a.Flagged || (early.AtLeast && early.P > a.P) {
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
	// A verdict that judged nothing names nothing, whatever shape it is in:
	// no counts at all, or probes without counts.
	for _, v := range []Verdict{{}, {Probes: 5}, {Probes: 5, Counts: []int{1}}} {
		if got := v.Where(); got != "" {
			t.Errorf("verdict %+v: Where() = %q", v, got)
		}
	}
}

// What one evaluation costs: for a strong pattern, which the bound settles;
// for a weaker one that is still flagged, where every round runs; and for
// ordinary bursty loss, where counting stops early. At 20 probes and at 1000.
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
	// A pattern strong enough to be flagged and too weak for the bound to
	// settle, so that every round is run: the last three probes lost one time
	// in five, three and two. This is what a partly rate-limited target costs.
	partly := func(n int) []Interval {
		for {
			ivs := make([]Interval, 0, Window)
			for i := range Window {
				lost := make([]bool, n)
				for k, p := range []float64{0.2, 0.35, 0.5} {
					lost[n-3+k] = rng.Float64() < p
				}
				if partial(lost) {
					ivs = append(ivs, Interval{TS: int64(i) * 60, Lost: lost})
				}
			}
			// Keep drawing until it is the case this benchmark is named for.
			if v := Test(ivs, false); v.Flagged && !v.AtMost && !v.AtLeast {
				return ivs
			}
		}
	}
	for name, w := range map[string][]Interval{
		"flagged by the bound/20 probes":   wide(20),
		"flagged by the bound/1000 probes": wide(1000),
		"flagged by counting/20 probes":    partly(20),
		"flagged by counting/1000 probes":  partly(1000),
		"bursty/20 probes":                 window(rng, gilbertElliott(0.01, 0.10)),
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
// near it. Checked against a brute-force count on windows of twelve to
// fifteen intervals, with an odd probe count too, where the middle probe is
// its own mirror image.
func TestExactCountMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(6, 6))
	for _, n := range []int{20, 7} {
		for m := MinIntervals; m <= MinIntervals+3; m++ {
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

// Protects the estimate that full windows use. It is a different piece of code
// from the exact count and must be an estimate of the same number: on windows
// small enough to count exactly, the two agree to within what ten thousand
// random rounds can resolve. And when no round can be as lopsided as the data,
// the estimate is exactly one in rounds plus one, the data itself.
func TestEstimateAgreesWithExactCount(t *testing.T) {
	rng := rand.New(rand.NewPCG(8, 8))
	const half = probes / 2
	for trial := range 20 {
		// Fourteen intervals leaning one way more often than the other, so
		// that p comes out somewhere between a few percent and a half.
		tilts := make([][]int8, 14)
		for j := range tilts {
			tilts[j] = make([]int8, half)
			for i := range tilts[j] {
				switch r := rng.Float64(); {
				case r < 0.20:
					tilts[j][i] = 1
				case r < 0.30:
					tilts[j][i] = -1
				}
			}
		}
		data := make([]int, half)
		for _, row := range tilts {
			for i, v := range row {
				data[i] += int(v)
			}
		}
		observed := lopsidedness(data)
		a, of, _ := countExact(tilts, append([]int(nil), data...), observed, true)
		exact := float64(a) / float64(of)
		b, of, _ := countRandom(tilts, append([]int(nil), data...), observed, true)
		est := float64(b) / float64(of)
		// Four standard errors of a proportion estimated from `rounds` draws.
		tol := 4 * math.Sqrt(exact*(1-exact)/rounds)
		if math.Abs(est-exact) > tol+1.0/rounds {
			t.Errorf("trial %d: estimate %.4f, exact %.4f, more than %.4f apart", trial, est, exact, tol)
		}
	}

	if v := Test(tailEvery(30, span(15, 20)...), true); v.AtLeast || v.P != 1.0/(rounds+1) {
		t.Errorf("thirty intervals that all lost their tail: P = %v, want exactly %v", v.P, 1.0/(rounds+1))
	}
}

// Protects the shortcut that settles a strong pattern without counting. It is
// only sound if the bound is never below the true p-value, so it is checked
// against the exact count on windows small enough to count; and it has to
// actually settle the plain case it exists for, a tail lost in every interval,
// with the same verdict as counting to the end gives.
func TestBoundIsNeverBelowTheExactCount(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 9))
	const half = probes / 2
	for trial := range 300 {
		// From nearly symmetric to nearly one-sided, so that p ranges from
		// about one down to the smallest sixteen intervals can give.
		lean := rng.Float64()
		tilts := make([][]int8, 16)
		for j := range tilts {
			tilts[j] = make([]int8, half)
			for i := range tilts[j] {
				if rng.Float64() < 0.4 {
					tilts[j][i] = 1
					if rng.Float64() > 0.5+lean/2 {
						tilts[j][i] = -1
					}
				}
			}
		}
		data := make([]int, half)
		for _, row := range tilts {
			for i, v := range row {
				data[i] += int(v)
			}
		}
		observed := lopsidedness(data)
		a, of, _ := countExact(tilts, data, observed, true)
		if exact, bound := float64(a)/float64(of), chanceBound(tilts, observed); bound < exact {
			t.Fatalf("trial %d: bound %g is below the exact p-value %g", trial, bound, exact)
		}
	}

	w := tailEvery(30, span(15, 20)...)
	quick, counted := Test(w, false), Test(w, true)
	if !quick.Flagged || !quick.AtMost || quick.P >= PMax {
		t.Errorf("thirty intervals that all lost their tail were not settled by the bound: %+v", quick)
	}
	if !counted.Flagged || counted.AtMost {
		t.Errorf("counted to the end: %+v", counted)
	}
}

// The numbers the browser gets and the words the CLI prints are one rule.
func TestPositionsAreTheOnesAboveTwiceAnEvenShare(t *testing.T) {
	counts := make([]int, 20)
	for i := 15; i < 20; i++ {
		counts[i] = 8
	}
	if got := Positions(counts, 40); !slices.Equal(got, []int{16, 17, 18, 19, 20}) {
		t.Errorf("tail: %v", got)
	}
	// Twice the share is 4: a count of 3 is under it, 4 is on it.
	if got := Positions([]int{3, 4, 0, 0, 0}, 10); !slices.Equal(got, []int{2}) {
		t.Errorf("boundary: %v", got)
	}
	flat := slices.Repeat([]int{2}, 20)
	if got := Positions(flat, 40); len(got) != 0 {
		t.Errorf("an even profile has no standout position, got %v", got)
	}
	if got := Positions(nil, 0); got != nil {
		t.Errorf("nothing lost: %v", got)
	}
}
