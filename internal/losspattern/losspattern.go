// Package losspattern decides whether a series loses its probes at particular
// places in the send order rather than wherever the loss happens to fall.
//
// A target, its CPE or a router's control plane that rate-limits ICMP drops
// replies at predictable positions: with a burst of probes it is the tail,
// once the token bucket is empty. On a graph that is indistinguishable from
// network loss. In the order the probes were sent it is not.
//
// The test is a randomisation test whose null is time reversal. Mirror an
// interval's loss pattern, first probe for last, and loss that comes from the
// network looks the same as before: a burst of loss is as likely to be cut off
// by the start of an interval as by its end, however long bursts are. Loss
// that depends on the position does not survive the mirror: it is always at
// the same end. So each interval is mirrored or not at random, many times, and
// the question is how often chance alone makes the loss profile as lopsided as
// the one observed.
//
// The mirroring is an exact symmetry when one interval's loss says nothing
// about the next one's, which is the case for a burst of probes followed by
// most of a minute of silence. When loss carries over from one interval into
// the next, as it can with probes spread across the interval, a burst that
// straddles the boundary puts loss at the end of one interval and the start of
// the next. Those two lean opposite ways; mirrored independently they often
// lean the same way, so chance looks more lopsided than the data and the test
// errs towards not flagging.
//
// Two weaker nulls were measured first and rejected. Shuffling the positions
// within an interval treats losses as independent, and real loss is bursty:
// under a Gilbert-Elliott channel it flagged 1.3% to 4.3% of windows at a
// nominal 0.1%. Rotating each interval's pattern keeps a burst a burst but not
// where interval edges cut it, and still flagged up to 0.6%.
//
// What the mirror cannot see is a pattern that is its own mirror image, such
// as the first and the last probe lost equally often. And not flagging is not
// a finding: it says the loss is not lopsided enough to rule chance out.
package losspattern

import (
	"fmt"
	"math"
	"math/bits"
	"math/rand/v2"
	"strings"
)

const (
	// Window is how many intervals the test looks back over. Only intervals
	// that lost some of their probes and not all count towards it; the others
	// say nothing about position and are passed over.
	Window = 60
	// MaxAgeS bounds how far back those intervals may lie. Rate limiting that
	// hits one interval in ten takes hours to fill a window; a day is long
	// enough for that and short enough that the path is still the same path.
	MaxAgeS = 86400
	// MinLost is the least number of lost probes worth testing.
	MinLost = 10
	// MinIntervals is the least number of lopsided intervals that can reach
	// PMax at all: if every one of them leans the same way, chance does that
	// with probability 2/2^n, which first drops below PMax at twelve.
	MinIntervals = 12
	// PMax is the p-value below which a window is flagged. The aim is that at
	// most one window in a thousand of ordinary network loss is flagged, and a
	// threshold of 0.001 sits on that line rather than under it: measured on
	// bursty loss it flagged 0.07% to 0.13% of windows. Half of it leaves room.
	PMax = 0.0005
	// rounds is how many random mirrorings estimate the p-value when there are
	// too many intervals to try every mirroring. Five times 1/PMax, so that the
	// estimate near PMax rests on about five rounds as lopsided as the data
	// rather than on whether there happened to be one. With 1/PMax rounds a
	// window is flagged only when there is none, which makes the estimate twice
	// as strict as the exact count it takes over from.
	rounds = 9999
	// exactWork bounds the exact count: ways of mirroring times positions per
	// way. It is a few times what the estimate costs on a full window.
	exactWork = 4 << 20
)

// Interval is one interval's losses in send order.
type Interval struct {
	TS     int64
	Spread bool   // sent in spread mode rather than as a burst
	Lost   []bool // one per probe sent; true when it drew no valid reply
}

// Verdict is what the test made of a series' recent intervals.
type Verdict struct {
	// Tested is false when there was too little to judge by; Why says what
	// was missing.
	Tested bool
	Why    string
	// Flagged is true when the loss sits at positions chance does not explain.
	Flagged bool
	// P is the p-value: counted exactly for a small window, estimated for a
	// large one. When AtLeast is set the test stopped as soon as the window
	// could no longer be flagged and P is a lower bound. When AtMost is set the
	// window was flagged without counting and P is an upper bound.
	P       float64
	AtLeast bool
	AtMost  bool

	Probes    int   // probes per interval in the window
	Spread    bool  // the window's probe mode
	Intervals int   // intervals in the window
	Lost      int   // probes lost in them
	Counts    []int // per send position, how many intervals lost it
	From, To  int64 // the window's first and last interval
}

// Test judges the newest of the intervals given, by the window that ends in
// it. history is in time order, oldest first, and holds only intervals whose
// lost positions were recorded.
//
// The window is the most recent intervals that share the newest one's probe
// count and mode, no more than Window of them and none older than MaxAgeS. It
// stops at the first interval measured differently: a change of settings
// starts a new regime, and positions in one do not mean what they mean in the
// other.
//
// full works the p-value out to the end. Without it the test stops as soon as
// the verdict is settled: for ordinary loss after a handful of rounds, and for
// a strong pattern before the first one, on a bound that needs no counting.
// The verdict is the same either way; only how exactly P is known differs.
//
// The same history always gives the same verdict: the random mirrorings are
// the same ones every time.
func Test(history []Interval, full bool) Verdict {
	if len(history) == 0 {
		return Verdict{Why: "no interval recorded which probes were lost"}
	}
	newest := history[len(history)-1]
	n := len(newest.Lost)
	v := Verdict{Probes: n, Spread: newest.Spread, Counts: make([]int, n), To: newest.TS}

	// tilts[j][i] is +1 when interval j lost probe i and not its mirror image,
	// -1 the other way round, 0 when both or neither. Mirroring the interval
	// negates the row, which is all a round has to do.
	var tilts [][]int8
	half := n / 2
	for k := len(history) - 1; k >= 0 && v.Intervals < Window; k-- {
		iv := history[k]
		if len(iv.Lost) != n || iv.Spread != newest.Spread || newest.TS-iv.TS > MaxAgeS {
			break
		}
		lost := 0
		for _, l := range iv.Lost {
			if l {
				lost++
			}
		}
		if lost == 0 || lost == n {
			continue // says nothing about position
		}
		v.Intervals++
		v.Lost += lost
		v.From = iv.TS
		row := make([]int8, half)
		lopsided := false
		for i, l := range iv.Lost {
			if l {
				v.Counts[i]++
			}
			if i < half && l != iv.Lost[n-1-i] {
				lopsided = true
				if l {
					row[i] = 1
				} else {
					row[i] = -1
				}
			}
		}
		if lopsided {
			tilts = append(tilts, row)
		}
	}

	switch {
	case v.Lost < MinLost:
		v.Why = fmt.Sprintf("%d probes lost in the window, fewer than %d", v.Lost, MinLost)
		return v
	case len(tilts) < MinIntervals:
		v.Why = fmt.Sprintf("%d intervals with lopsided loss in the window, fewer than %d", len(tilts), MinIntervals)
		return v
	}
	v.Tested = true

	sum := make([]int, half)
	for _, row := range tilts {
		for i, t := range row {
			sum[i] += int(t)
		}
	}
	observed := lopsidedness(sum)

	// A strong pattern is settled by arithmetic. This is what keeps the test
	// affordable where it runs on every lossy interval written: counting to the
	// end costs milliseconds, and a series that is rate-limited all day would
	// pay that on each of them, inside the write.
	bound := chanceBound(tilts, observed)
	if bound < PMax && !full {
		v.P, v.AtMost, v.Flagged = bound, true, true
		return v
	}

	var asExtreme, of int
	var complete bool
	if m := len(tilts); m <= 20 && (1<<m)*half <= exactWork {
		asExtreme, of, complete = countExact(tilts, sum, observed, full)
	} else {
		asExtreme, of, complete = countRandom(tilts, sum, observed, full)
	}
	v.P = float64(asExtreme) / float64(of)
	v.AtLeast = !complete
	// The bound decides here too, so that a window is flagged or not whichever
	// way it was asked. An estimate can land above PMax for a window whose true
	// p the bound has already shown to be below it; the bound is then the
	// better figure, and reporting it keeps a flagged window from being shown
	// beside a p-value that would not flag it.
	if bound < PMax && v.P >= PMax {
		v.P, v.AtLeast = bound, false
	}
	v.Flagged = v.P < PMax
	return v
}

// chanceBound is an upper bound on the p-value that needs no counting.
//
// The statistic is a sum, over mirrored pairs of positions, of the square of
// the pair's summed tilt. If chance makes it at least as large as observed,
// then for at least one of the k pairs that tilt at all, the square is at
// least observed/k. A pair's sum is a sum of independent random signs, one for
// each of the n intervals that tilt there, so by Hoeffding's inequality it
// reaches a in size with probability at most 2·exp(−a²/2n). The pairs' chances
// add up.
//
// It is loose, by a wide margin for a weak pattern, and that costs nothing: a
// window it cannot settle is counted.
func chanceBound(tilts [][]int8, observed int) float64 {
	if len(tilts) == 0 {
		return 1
	}
	n := make([]int, len(tilts[0]))
	for _, row := range tilts {
		for i, t := range row {
			if t != 0 {
				n[i]++
			}
		}
	}
	k := 0
	for _, c := range n {
		if c > 0 {
			k++
		}
	}
	if k == 0 {
		return 1
	}
	bound := 0.0
	for _, c := range n {
		if c > 0 {
			bound += 2 * math.Exp(-float64(observed)/float64(k)/(2*float64(c)))
		}
	}
	return min(bound, 1)
}

// settled reports whether a count may stop early. The mirrorings not yet tried
// can only add to it, so once p has reached PMax the window cannot be flagged
// any more.
func settled(asExtreme, of int, full bool) bool {
	return !full && float64(asExtreme)/float64(of) >= PMax
}

// countExact tries every way of mirroring the intervals and counts those at
// least as lopsided as the data, which gives p exactly. It is used when there
// are few enough intervals, and that is the regime where exactness matters: a
// pattern is first caught at twelve intervals, where p is 2/4096, and an
// estimate lands on either side of PMax by luck.
//
// In Gray-code order each way differs from the last in one interval, so a step
// costs one row rather than all of them. sum holds the data's own profile on
// entry and is used as scratch.
func countExact(tilts [][]int8, sum []int, observed int, full bool) (asExtreme, of int, complete bool) {
	of, asExtreme = 1<<len(tilts), 1 // the data as observed is one of the ways
	sign := make([]int, len(tilts))
	for j := range sign {
		sign[j] = 1
	}
	g := 1
	for ; g < of && !settled(asExtreme, of, full); g++ {
		j := bits.TrailingZeros(uint(g))
		sign[j] = -sign[j]
		for i, t := range tilts[j] {
			sum[i] += 2 * sign[j] * int(t)
		}
		if lopsidedness(sum) >= observed {
			asExtreme++
		}
	}
	return asExtreme, of, g == of
}

// countRandom estimates the same count from random mirrorings, for a window
// with too many intervals to try them all. The generator starts from the same
// state on every call, so a window is not flagged on one evaluation and passed
// on the next.
//
// A round is one random word, bit j saying whether interval j is mirrored. For
// each pair of positions the intervals that tilt one way and those that tilt
// the other are kept as two bit sets, so the pair's sum under a mirroring is
// its plain sum minus twice (the mirrored ones that tilt one way less the
// mirrored ones that tilt the other): two ANDs and two bit counts, whatever
// the number of intervals. Adding the rows up one by one cost fifteen times as
// much, and this runs on every lossy interval the master writes for as long as
// a series stays flagged.
func countRandom(tilts [][]int8, sum []int, observed int, full bool) (asExtreme, of int, complete bool) {
	// One word of mirrorings: the window never holds more intervals than that.
	if len(tilts) > 64 {
		panic("losspattern: a window of more than 64 intervals")
	}
	up, down := make([]uint64, len(sum)), make([]uint64, len(sum))
	for j, row := range tilts {
		for i, t := range row {
			switch t {
			case 1:
				up[i] |= 1 << j
			case -1:
				down[i] |= 1 << j
			}
		}
	}
	// Only the pairs that tilt in some interval: the others are zero under
	// every mirroring, and with a thousand probes they are nearly all of them.
	n := 0
	for i := range up {
		if up[i]|down[i] != 0 {
			up[n], down[n] = up[i], down[i]
			n++
		}
	}
	up, down = up[:n], down[:n]
	plain := make([]int, n)
	for i := range plain {
		plain[i] = bits.OnesCount64(up[i]) - bits.OnesCount64(down[i])
	}

	rng := rand.New(rand.NewPCG(1, 2))
	of, asExtreme = rounds+1, 1 // counting the data as observed, as the exact count does
	done := 0
	for ; done < rounds && !settled(asExtreme, of, full); done++ {
		mirrored := rng.Uint64()
		s := 0
		for i := range plain {
			d := plain[i] - 2*(bits.OnesCount64(up[i]&mirrored)-bits.OnesCount64(down[i]&mirrored))
			s += d * d
		}
		if s >= observed {
			asExtreme++
		}
	}
	return asExtreme, of, done == rounds
}

// lopsidedness is how far a loss profile is from its own mirror image: the sum
// over mirrored pairs of positions of the squared difference in their counts.
func lopsidedness(sum []int) int {
	s := 0
	for _, d := range sum {
		s += d * d
	}
	return s
}

// Positions are the send positions, from 1 and ascending, that lost clearly more
// than their share: counts is per position how many intervals lost it, lost the
// total of lost probes. Empty when no position stands out.
func Positions(counts []int, lost int) []int {
	if len(counts) == 0 || lost == 0 {
		return nil
	}
	// Twice an even share is where "concentrated" stops being a judgement call
	// on a profile the test has already found lopsided.
	bar := 2 * float64(lost) / float64(len(counts))
	var at []int
	for i, c := range counts {
		if float64(c) >= bar {
			at = append(at, i+1)
		}
	}
	return at
}

// Where names those positions the way an operator counts them, with runs
// written as ranges: "16–20", or "4, 8, 12, 16, 20". Empty when no position
// stands out.
func (v Verdict) Where() string {
	at := Positions(v.Counts[:v.Probes], v.Lost)
	var parts []string
	for i := 0; i < len(at); i++ {
		j := i
		for j+1 < len(at) && at[j+1] == at[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d–%d", at[i], at[j]))
		} else {
			parts = append(parts, fmt.Sprint(at[i]))
		}
		i = j
	}
	return strings.Join(parts, ", ")
}
