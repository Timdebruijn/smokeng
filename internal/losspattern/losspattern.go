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
// Two weaker nulls were measured first and rejected. Shuffling the positions
// within an interval treats losses as independent, and real loss is bursty:
// under a Gilbert-Elliott channel it flagged 1.3% to 4.3% of windows at a
// nominal 0.1%. Rotating each interval's pattern keeps a burst a burst but not
// where interval edges cut it, and still flagged up to 0.6%.
//
// What the mirror cannot see is a pattern that is its own mirror image, such
// as the first and the last probe lost equally often.
package losspattern

import (
	"fmt"
	"math/bits"
	"math/rand/v2"
	"strings"
)

const (
	// Window is how many intervals the test looks back over, counting only
	// those that recorded which probes were lost.
	Window = 60
	// MaxAgeS bounds how far back those intervals may lie. Rate limiting that
	// hits one interval in ten takes hours to fill a window; a day is long
	// enough for that and short enough that the path is still the same path.
	MaxAgeS = 86400
	// MinLost is the least number of lost probes worth testing.
	MinLost = 10
	// MinIntervals is the least number of lopsided intervals that can reach
	// PMax at all: if every one of them leans the same way, chance does that
	// with probability 2/2^n, which first drops below PMax at eleven.
	MinIntervals = 11
	// PMax is the p-value below which a window is flagged.
	PMax = 0.001
	// rounds is how many random mirrorings estimate the p-value when there are
	// too many intervals to try every mirroring. With 1999 the smallest p is
	// 1/2000, so a window is flagged only when not one of them was as lopsided
	// as what was observed.
	rounds = 1999
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
	// P is the estimated p-value. When AtLeast is set the test stopped as soon
	// as the outcome was settled and P is a lower bound.
	P       float64
	AtLeast bool

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
// full estimates the p-value to the end. Without it the test stops once the
// window can no longer be flagged, which for ordinary loss is after a handful
// of rounds.
//
// The same history always gives the same verdict: the random mirroring is
// seeded from the window, not from the clock.
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

	// The rounds not yet run can only add to the count, so once p has reached
	// PMax the window cannot be flagged any more and the rest need not be run.
	settled := func(asExtreme, of int) bool {
		return !full && float64(asExtreme)/float64(of) >= PMax
	}

	if m := len(tilts); m <= 20 && (1<<m)*half <= exactWork {
		// Few enough intervals to try every way of mirroring them, which gives
		// p exactly. This is the regime where it matters: a pattern is first
		// caught at eleven intervals, where p is 2/2048, and an estimate from
		// two thousand random rounds lands on either side of PMax by luck.
		//
		// In Gray-code order each way differs from the last in one interval, so
		// a step costs one row rather than all of them.
		total, asExtreme := 1<<m, 1 // the data as observed is one of the ways
		sign := make([]int, m)
		for j := range sign {
			sign[j] = 1
		}
		g := 1
		for ; g < total && !settled(asExtreme, total); g++ {
			j := bits.TrailingZeros(uint(g))
			sign[j] = -sign[j]
			for i, t := range tilts[j] {
				sum[i] += 2 * sign[j] * int(t)
			}
			if lopsidedness(sum) >= observed {
				asExtreme++
			}
		}
		v.P = float64(asExtreme) / float64(total)
		v.AtLeast = g < total
		v.Flagged = v.P < PMax
		return v
	}

	// Too many to try them all: estimate from random mirrorings. Seeded from
	// the window so a verdict can be reproduced, and so that the same intervals
	// are not flagged on one evaluation and passed on the next.
	rng := rand.New(rand.NewPCG(uint64(v.To), uint64(v.From)^uint64(v.Lost)<<32))
	asExtreme, done := 1, 0 // counting the data as observed, as the exact count does
	for done < rounds && !settled(asExtreme, rounds+1) {
		done++
		clear(sum)
		var coin uint64
		for j, row := range tilts {
			if j%64 == 0 {
				coin = rng.Uint64()
			}
			sign := 1
			if coin>>(j%64)&1 == 1 {
				sign = -1
			}
			for i, t := range row {
				sum[i] += sign * int(t)
			}
		}
		if lopsidedness(sum) >= observed {
			asExtreme++
		}
	}
	v.P = float64(asExtreme) / (rounds + 1)
	v.AtLeast = done < rounds
	v.Flagged = v.P < PMax
	return v
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

// Where names the send positions that lost clearly more than their share, the
// way an operator counts them (from 1), with runs written as ranges: "16–20",
// or "4, 8, 12, 16, 20". Empty when no position stands out.
func (v Verdict) Where() string {
	if v.Probes == 0 || v.Lost == 0 {
		return ""
	}
	// Twice an even share is where "concentrated" stops being a judgement call
	// on a profile the test has already found lopsided.
	bar := 2 * float64(v.Lost) / float64(v.Probes)
	var parts []string
	for i := 0; i < v.Probes; i++ {
		if float64(v.Counts[i]) < bar {
			continue
		}
		j := i
		for j+1 < v.Probes && float64(v.Counts[j+1]) >= bar {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d–%d", i+1, j+1))
		} else {
			parts = append(parts, fmt.Sprint(i+1))
		}
		i = j
	}
	return strings.Join(parts, ", ")
}
