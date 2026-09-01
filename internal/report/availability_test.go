package report

import "testing"

// pts builds a run of measurements one interval apart from a start, from a
// compact spec: received count per interval, -1 meaning "no measurement" (a gap).
func pts(start int64, intervalS int, sent int, received ...int) []Point {
	var out []Point
	for i, rc := range received {
		if rc < 0 {
			continue // a gap: no point at this slot
		}
		out = append(out, Point{TS: start + int64(i*intervalS), Sent: sent, Received: rc})
	}
	return out
}

func TestAvailabilityAllUp(t *testing.T) {
	p := pts(1000, 60, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10) // 10 intervals, all up
	r := Availability(p, 60, 1000, 1600, 100)
	if !r.HasData || r.Availability != 1 || r.Coverage != 1 {
		t.Fatalf("all up: availability=%v coverage=%v hasData=%v", r.Availability, r.Coverage, r.HasData)
	}
	if len(r.Downtime) != 0 {
		t.Fatalf("all up: got %d downtime episodes", len(r.Downtime))
	}
	if r.UpS != 600 || r.CoveredS != 600 || r.UnknownS != 0 {
		t.Fatalf("all up: upS=%d coveredS=%d unknownS=%d", r.UpS, r.CoveredS, r.UnknownS)
	}
}

func TestAvailabilityWithOutage(t *testing.T) {
	// Three contiguous down intervals in the middle.
	p := pts(1000, 60, 10, 10, 10, 0, 0, 0, 10, 10, 10, 10, 10)
	r := Availability(p, 60, 1000, 1600, 100)
	if r.UpIntervals != 7 || r.DownIntervals != 3 {
		t.Fatalf("counts: up=%d down=%d", r.UpIntervals, r.DownIntervals)
	}
	if r.Availability != 0.7 {
		t.Fatalf("availability=%v, want 0.7", r.Availability)
	}
	if len(r.Downtime) != 1 {
		t.Fatalf("want one episode, got %d", len(r.Downtime))
	}
	e := r.Downtime[0]
	if e.StartTS != 1120 || e.Intervals != 3 || e.DurationS != 180 || e.EndTS != 1300 {
		t.Fatalf("episode = %+v", e)
	}
}

func TestAvailabilityGapIsUnknownNotDown(t *testing.T) {
	// Two up intervals, a gap of two, then two up: coverage 4/10, availability 1.
	p := pts(1000, 60, 10, 10, 10, -1, -1, 10, 10)
	r := Availability(p, 60, 1000, 1600, 100)
	if r.DownIntervals != 0 || len(r.Downtime) != 0 {
		t.Fatalf("a gap must not read as downtime: down=%d episodes=%d", r.DownIntervals, len(r.Downtime))
	}
	if r.CoveredS != 240 || r.UnknownS != 360 {
		t.Fatalf("coveredS=%d unknownS=%d, want 240/360", r.CoveredS, r.UnknownS)
	}
	if r.Availability != 1 || r.Coverage != 0.4 {
		t.Fatalf("availability=%v coverage=%v, want 1 / 0.4", r.Availability, r.Coverage)
	}
}

func TestAvailabilityGapBreaksEpisode(t *testing.T) {
	// Down, then a gap, then down again: two outages, not one run across the gap.
	p := pts(1000, 60, 10, 0, -1, -1, 0, 10)
	r := Availability(p, 60, 1000, 1600, 100)
	if len(r.Downtime) != 2 {
		t.Fatalf("a gap between outages must split them: got %d episodes", len(r.Downtime))
	}
}

func TestAvailabilityThreshold(t *testing.T) {
	// One interval at 60% loss. Strict (100) counts it up; a 50% SLA counts it down.
	p := pts(1000, 60, 10, 10, 4, 10)
	if r := Availability(p, 60, 1000, 1180, 100); r.DownIntervals != 0 {
		t.Fatalf("threshold 100: 60%% loss should be up, got %d down", r.DownIntervals)
	}
	if r := Availability(p, 60, 1000, 1180, 50); r.DownIntervals != 1 {
		t.Fatalf("threshold 50: 60%% loss should be down, got %d down", r.DownIntervals)
	}
}

func TestAvailabilityNoData(t *testing.T) {
	r := Availability(nil, 60, 1000, 1600, 100)
	if r.HasData || r.Availability != 0 || r.CoveredS != 0 || r.UnknownS != 600 {
		t.Fatalf("empty: %+v", r)
	}
}
