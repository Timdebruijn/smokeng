// Package report turns stored measurements into the summaries a person or an
// auditor asks for — starting with availability over a period. The arithmetic
// lives here, away from storage and HTTP, so it can be reasoned about and tested
// on its own.
package report

// Point is one interval's reachability: how many probes went out and how many
// came back. It carries no timing detail — availability is about whether the
// target answered, not how fast.
type Point struct {
	TS       int64
	Sent     int
	Received int
}

// Episode is a contiguous run of down intervals: an outage, with its bounds and
// how long it lasted. This is the part an SLA report is really for.
type Episode struct {
	StartTS   int64 `json:"start_ts"`
	EndTS     int64 `json:"end_ts"`
	DurationS int64 `json:"duration_s"`
	Intervals int   `json:"intervals"`
}

// Report is availability over a window, kept honest by separating what was
// measured from what was not. Availability is computed only over intervals there
// is data for; Coverage says how much of the window that was. A 100% availability
// over 40% coverage is a different claim from 100% over 99%, and this refuses to
// conflate them.
type Report struct {
	From             int64     `json:"from"`
	To               int64     `json:"to"`
	IntervalS        int       `json:"interval_s"`
	DownThresholdPct float64   `json:"down_threshold_pct"`
	WindowS          int64     `json:"window_s"`
	UpS              int64     `json:"up_s"`
	DownS            int64     `json:"down_s"`
	UnknownS         int64     `json:"unknown_s"`
	CoveredS         int64     `json:"covered_s"`
	UpIntervals      int       `json:"up_intervals"`
	DownIntervals    int       `json:"down_intervals"`
	HasData          bool      `json:"has_data"`
	Availability     float64   `json:"availability"` // UpS / CoveredS, 0 when no data
	Coverage         float64   `json:"coverage"`     // CoveredS / WindowS
	Downtime         []Episode `json:"downtime"`
}

// Availability classifies each interval as up, down or unknown and rolls the run
// into a Report. An interval is down when its loss reaches downThresholdPct
// (100 — the default a caller should pass — means only a total blackout counts);
// up otherwise; and a gap between measurements is neither, it is time smokeng has
// no data for and will not pretend about. Points must be ordered by TS and lie
// within [from, to); intervalS is the target's effective interval, used both to
// turn an interval count into seconds and to tell a gap from an adjacency.
func Availability(points []Point, intervalS int, from, to int64, downThresholdPct float64) Report {
	r := Report{
		From: from, To: to, IntervalS: intervalS,
		DownThresholdPct: downThresholdPct,
		Downtime:         []Episode{},
	}
	if to > from {
		r.WindowS = to - from
	}
	if intervalS <= 0 {
		// Without an interval length there is no way to attribute time; report
		// the window as entirely unknown rather than guess.
		r.UnknownS = r.WindowS
		return r
	}

	// A gap is a gap when the next measurement is more than an interval and a
	// half away — half an interval of slack absorbs ordinary scheduling jitter.
	gapTol := int64(intervalS) + int64(intervalS)/2
	isDown := func(p Point) bool {
		if p.Sent <= 0 {
			return false
		}
		loss := 100 * float64(p.Sent-p.Received) / float64(p.Sent)
		return loss >= downThresholdPct
	}

	var cur *Episode
	for i, p := range points {
		if p.Sent <= 0 {
			// A measurement that sent nothing tells us nothing about reachability.
			continue
		}
		// A gap since the previous point ends any open episode: the missing
		// intervals are unknown, not part of the outage.
		if cur != nil && i > 0 && p.TS-points[i-1].TS > gapTol {
			r.Downtime = append(r.Downtime, *cur)
			cur = nil
		}
		if isDown(p) {
			r.DownIntervals++
			if cur == nil {
				cur = &Episode{StartTS: p.TS}
			}
			cur.Intervals++
			cur.EndTS = p.TS + int64(intervalS)
			cur.DurationS = int64(cur.Intervals) * int64(intervalS)
		} else {
			r.UpIntervals++
			if cur != nil {
				r.Downtime = append(r.Downtime, *cur)
				cur = nil
			}
		}
	}
	if cur != nil {
		r.Downtime = append(r.Downtime, *cur)
	}

	r.UpS = int64(r.UpIntervals) * int64(intervalS)
	r.DownS = int64(r.DownIntervals) * int64(intervalS)
	r.CoveredS = r.UpS + r.DownS
	if r.CoveredS > r.WindowS && r.WindowS > 0 {
		// Rounding at the window edges can push the attributed time just past
		// the window; do not let coverage read above 100%.
		r.CoveredS = r.WindowS
	}
	r.UnknownS = r.WindowS - r.CoveredS
	if r.UnknownS < 0 {
		r.UnknownS = 0
	}
	r.HasData = r.CoveredS > 0
	if r.HasData {
		r.Availability = float64(r.UpS) / float64(r.CoveredS)
	}
	if r.WindowS > 0 {
		r.Coverage = float64(r.CoveredS) / float64(r.WindowS)
	}
	return r
}
