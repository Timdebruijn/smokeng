// Package shape measures how an RTT distribution differs from another, and how
// bimodal one is. These are the two questions a threshold on a single number —
// median, p95, loss — cannot answer, and the reason smokeng keeps the whole
// distribution: a path that splits into two latencies, or whose bulk holds while
// its tail grows, changes shape without necessarily crossing any scalar line.
//
// Everything here is pure and scale-and-location aware only where it should be:
// Wasserstein distance is in the samples' own units (microseconds), and the
// bimodality coefficient is invariant to both shift and scale, as a shape
// measure must be.
package shape

import (
	"math"
	"sort"
)

// Wasserstein1 is the 1-D earth-mover distance between two empirical
// distributions: the area between their CDFs, which is the average distance the
// probability mass of one must travel to become the other. It is returned in the
// samples' units (microseconds), so a result reads directly as "the distribution
// moved by this much". Unlike a KS statistic it is sensitive to the tail, and
// unlike a bin-based divergence it needs no binning and no matching sample
// counts.
//
// Zero samples on either side is undefined; the caller gets 0 and should treat
// the comparison as having no data rather than as "no change".
func Wasserstein1(a, b []uint32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	A := sortedFloats(a)
	B := sortedFloats(b)
	na, nb := float64(len(A)), float64(len(B))

	var w, prevX, cdfA, cdfB float64
	i, j := 0, 0
	started := false
	for i < len(A) || j < len(B) {
		var x float64
		if j >= len(B) || (i < len(A) && A[i] <= B[j]) {
			x = A[i]
		} else {
			x = B[j]
		}
		if started {
			// The CDFs are constant across [prevX, x); accumulate the gap there.
			w += math.Abs(cdfA-cdfB) * (x - prevX)
		}
		for i < len(A) && A[i] == x {
			i++
		}
		for j < len(B) && B[j] == x {
			j++
		}
		cdfA, cdfB = float64(i)/na, float64(j)/nb
		prevX = x
		started = true
	}
	return w
}

// BimodalityCoefficient is Sarle's coefficient: (skewness² + 1) over the
// kurtosis with a small-sample correction. It runs from 0 to 1; a uniform
// distribution sits at about 0.555, and values above that are the signature of
// two clusters — the classic shape of load-balancing across two paths of
// different length, or of a failover that is flapping. ok is false when there
// are too few samples, or they are all identical, to say anything.
func BimodalityCoefficient(x []uint32) (float64, bool) {
	n := len(x)
	if n < 4 {
		return 0, false
	}
	N := float64(n)
	var mean float64
	for _, v := range x {
		mean += float64(v)
	}
	mean /= N

	var m2, m3, m4 float64
	for _, v := range x {
		d := float64(v) - mean
		d2 := d * d
		m2 += d2
		m3 += d2 * d
		m4 += d2 * d2
	}
	m2 /= N
	m3 /= N
	m4 /= N
	if m2 == 0 {
		return 0, false // no spread: skewness and kurtosis are undefined
	}

	skew := m3 / math.Pow(m2, 1.5)
	exKurt := m4/(m2*m2) - 3 // excess kurtosis (Fisher)

	denom := exKurt + 3*(N-1)*(N-1)/((N-2)*(N-3))
	if denom == 0 {
		return 0, false
	}
	return (skew*skew + 1) / denom, true
}

func sortedFloats(x []uint32) []float64 {
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = float64(v)
	}
	sort.Float64s(out)
	return out
}
