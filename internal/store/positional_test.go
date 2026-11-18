package store

import (
	"context"
	"database/sql"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/timdebruijn/smokeng/internal/losspattern"
)

const probes = 20

// lossy is an interval of `probes` probes that lost the given send positions,
// with the mask that says so.
func lossy(target, agent, ts int64, lostAt ...int) Measurement {
	l := maskOf(probes, lostAt...)
	recv := probes - len(lostAt)
	m := Measurement{TargetID: target, AgentID: agent, TS: ts, Sent: probes, Received: recv, Samples: make([]uint32, recv)}
	for i := range m.Samples {
		m.Samples[i] = 5000
	}
	m.LossMask = EncodeLossMask(false, l)
	return m
}

// tail is the loss a rate limiter produces: the last three probes of a burst.
func tail(target, agent, ts int64) Measurement { return lossy(target, agent, ts, 17, 18, 19) }

func positionalRows(t *testing.T, s *SQLite) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM positional_loss").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func detecting(t *testing.T) *SQLite {
	s := openTemp(t)
	s.DetectPositionalLoss()
	return s
}

// Detection is a statement about a window, so it must first fire at exactly the
// interval where the window can prove it (losspattern.MinIntervals) and stay on
// after. Each batch is the slice the prober would hand over, and the flag must
// come back on that slice for alerting and from QueryRange for readers.
func TestDetectionFlagsFromTheTwelfthTailInterval(t *testing.T) {
	s := detecting(t)
	ctx := t.Context()
	for i := range 15 {
		batch := []Measurement{tail(1, 0, int64(1000+60*i))}
		if err := s.WriteMeasurements(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if got, want := batch[0].Positional != nil, i+1 >= losspattern.MinIntervals; got != want {
			t.Errorf("interval %d: flagged in the slice = %v, want %v", i+1, got, want)
		}
	}
	got, err := s.QueryRange(ctx, 1, 0, 0, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range got {
		if flagged := m.Positional != nil; flagged != (i+1 >= losspattern.MinIntervals) {
			t.Errorf("QueryRange interval %d: flagged = %v", i+1, flagged)
		}
	}
	pl := got[14].Positional
	if pl == nil || pl.Intervals != 15 || pl.Lost != 45 || pl.P >= losspattern.PMax ||
		len(pl.Counts) != probes || pl.Counts[19] != 15 || pl.Counts[0] != 0 {
		t.Errorf("flag on the last interval = %+v", pl)
	}
	if n := positionalRows(t, s); n != 4 {
		t.Errorf("%d rows, want 4", n)
	}
}

// Off is the default, and is what an agent's buffer runs with: nothing is
// judged, nothing written, nothing set on what the caller passed.
func TestDetectionOffLeavesNoTrace(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	for i := range 15 {
		batch := []Measurement{tail(1, 0, int64(1000+60*i))}
		if err := s.WriteMeasurements(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if batch[0].Positional != nil {
			t.Fatalf("interval %d flagged with detection off", i+1)
		}
	}
	if n := positionalRows(t, s); n != 0 {
		t.Errorf("%d rows with detection off", n)
	}
	got, _ := s.QueryRange(t.Context(), 1, 0, 0, 0, 1<<40)
	for _, m := range got {
		if m.Positional != nil {
			t.Fatal("QueryRange flagged with detection off")
		}
	}
}

// An agent's backlog arrives many intervals in one call. The window of each
// must include the rows written earlier in the same transaction, or a backlog
// would be judged differently from the same intervals arriving one by one. The
// rows returned are what the master evaluates alerts on, so they carry the flag.
func TestIngestBacklogFlagsAsOneByOneWould(t *testing.T) {
	s := detecting(t)
	ctx := t.Context()
	var batch []Measurement
	for i := range 15 {
		batch = append(batch, tail(1, 4, int64(1000+60*i)))
	}
	stored, err := s.IngestMeasurements(ctx, batch)
	if err != nil || len(stored) != 15 {
		t.Fatalf("stored %d, %v", len(stored), err)
	}
	for i, m := range stored {
		if flagged := m.Positional != nil; flagged != (i+1 >= losspattern.MinIntervals) {
			t.Errorf("returned interval %d: flagged = %v", i+1, flagged)
		}
	}
	if n := positionalRows(t, s); n != 4 {
		t.Errorf("%d rows, want 4", n)
	}

	// A replay stores nothing and so must create nothing.
	again, err := s.IngestMeasurements(ctx, batch)
	if err != nil || len(again) != 0 {
		t.Fatalf("replay stored %d, %v", len(again), err)
	}
	if n := positionalRows(t, s); n != 4 {
		t.Errorf("%d rows after a replay, want 4", n)
	}
}

// Ordinary loss must not be flagged: two random probes per lossy interval has
// no preferred position. Fixed seed, so this is a fact and not a probability.
func TestScatteredLossIsNotFlagged(t *testing.T) {
	s := detecting(t)
	rng := rand.New(rand.NewPCG(7, 11))
	var batch []Measurement
	for i := range 40 {
		a := rng.IntN(probes)
		b := (a + 1 + rng.IntN(probes-1)) % probes
		batch = append(batch, lossy(1, 0, int64(1000+60*i), a, b))
	}
	if err := s.WriteMeasurements(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if n := positionalRows(t, s); n != 0 {
		t.Errorf("%d scattered intervals flagged", n)
	}
}

// Series are judged on their own masks. Interleaved in one batch, a series
// that loses its tail must still be flagged, and a scattered one beside it must
// not borrow its history (or lend its own).
func TestSeriesDoNotSeeEachOther(t *testing.T) {
	s := detecting(t)
	rng := rand.New(rand.NewPCG(3, 5))
	var batch []Measurement
	for i := range 30 {
		ts := int64(1000 + 60*i)
		a := rng.IntN(probes)
		b := (a + 1 + rng.IntN(probes-1)) % probes
		batch = append(batch, tail(1, 0, ts), lossy(2, 0, ts, a, b))
	}
	if err := s.WriteMeasurements(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	for _, m := range batch {
		if m.TargetID == 2 && m.Positional != nil {
			t.Fatalf("scattered series flagged at %d", m.TS)
		}
	}
	var tailFlags int
	for _, m := range batch {
		if m.TargetID == 1 && m.Positional != nil {
			tailFlags++
		}
	}
	if tailFlags != 30-losspattern.MinIntervals+1 {
		t.Errorf("tail series flagged %d times, want %d", tailFlags, 30-losspattern.MinIntervals+1)
	}
}

// A replaced interval is a different interval. If it no longer lost anything,
// the flag it earned must not survive it.
func TestReplacingAFlaggedIntervalDropsItsFlag(t *testing.T) {
	s := detecting(t)
	ctx := t.Context()
	var batch []Measurement
	for i := range 12 {
		batch = append(batch, tail(1, 0, int64(1000+60*i)))
	}
	if err := s.WriteMeasurements(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if n := positionalRows(t, s); n != 1 {
		t.Fatalf("%d rows before, want 1", n)
	}
	clean := rowWith(batch[11].TS, probes)
	if err := s.WriteMeasurements(ctx, []Measurement{clean}); err != nil {
		t.Fatal(err)
	}
	if n := positionalRows(t, s); n != 0 {
		t.Errorf("%d rows after replacing with a clean interval, want 0", n)
	}
}

// The window is bounded in time as well as in count: loss from two days ago is
// not evidence about now.
func TestOldIntervalsDoNotCount(t *testing.T) {
	s := detecting(t)
	var batch []Measurement
	for i := range 11 {
		batch = append(batch, tail(1, 0, int64(1000+60*i)))
	}
	batch = append(batch, tail(1, 0, 1000+2*86400))
	if err := s.WriteMeasurements(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if n := positionalRows(t, s); n != 0 {
		t.Errorf("flagged on %d intervals from before the age limit", n)
	}
}

// Pruning takes the flags with the intervals they describe, in the same
// transaction, and leaves the flags of what it keeps.
func TestPruneRemovesPositionalRowsOfWhatItPrunes(t *testing.T) {
	s := detecting(t)
	ctx := t.Context()
	var batch []Measurement
	for i := range 15 { // flagged: intervals 12..15, at 1660, 1720, 1780, 1840
		batch = append(batch, tail(1, 0, int64(1000+60*i)))
	}
	if err := s.WriteMeasurements(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneMeasurements(ctx, 1, 1750, 0); err != nil {
		t.Fatal(err)
	}
	var left []int64
	rows, err := s.db.Query("SELECT ts FROM positional_loss ORDER BY ts")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		rows.Scan(&ts)
		left = append(left, ts)
	}
	if !reflect.DeepEqual(left, []int64{1780, 1840}) {
		t.Errorf("positional rows left = %v, want [1780 1840]", left)
	}
}

// A wide plot reads pooled columns; the browser will want to know how many of a
// column's intervals were flagged, without a query per interval.
func TestQueryColumnsCountsFlaggedIntervals(t *testing.T) {
	s := detecting(t)
	ctx := t.Context()
	var batch []Measurement
	for i := range 20 { // flagged: 12th..20th, at 1660 + 60k
		batch = append(batch, tail(1, 0, int64(1000+60*i)))
	}
	if err := s.WriteMeasurements(ctx, batch); err != nil {
		t.Fatal(err)
	}
	// Two columns of ten intervals: [1000,1600) and [1600,2200).
	raw, cols, err := s.QueryColumns(ctx, 1, 0, 0, 1000, 2200, 2)
	if err != nil || raw != nil || len(cols) != 2 {
		t.Fatalf("raw=%v cols=%d err=%v", raw, len(cols), err)
	}
	if cols[0].Positional != 0 || cols[1].Positional != 9 {
		t.Errorf("flagged per column = %d, %d; want 0, 9", cols[0].Positional, cols[1].Positional)
	}
	// And in the unpooled case the intervals themselves say so.
	raw, _, err = s.QueryColumns(ctx, 1, 0, 0, 1000, 2200, 100)
	if err != nil || len(raw) != 20 || raw[19].Positional == nil || raw[0].Positional != nil {
		t.Errorf("raw read: %d rows, err %v", len(raw), err)
	}
}

// A database from before v26 has measurements, some with masks. It must open,
// gain the table and the index, and flag nothing it already holds: the flags
// belong to intervals as they arrive.
func TestV26AddsTableAndIndexAndFlagsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v25.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	for i := range 15 {
		m := tail(1, 0, int64(1000+60*i))
		if _, err := db.Exec(`INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples, loss_mask)
			VALUES (1, 0, ?, ?, ?, 0, X'01', ?)`, m.TS, m.Sent, m.Received, m.LossMask); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 25`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v25 database: %v", err)
	}
	defer s.Close()
	for _, name := range []string{"positional_loss", "measurements_masked"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d in sqlite_master, %v", name, n, err)
		}
	}
	if n := positionalRows(t, s); n != 0 {
		t.Errorf("%d rows flagged retroactively", n)
	}
	got, err := s.QueryRange(context.Background(), 1, 0, 0, 0, 1<<40)
	if err != nil || len(got) != 15 || got[14].Positional != nil || got[14].LossMask == nil {
		t.Errorf("rows after v26: %d, %v", len(got), err)
	}
}

// The detector runs once per lossy interval written, so its history read must
// be a seek. If the query stops being answered from the partial index it is a
// scan of a day of rows, and this is the test that notices.
func TestHistoryQueryUsesTheMaskedIndex(t *testing.T) {
	s := openTemp(t)
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+historyQuery, 1, 0, 0, 2000, 1000, 60)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	// The name alone proves nothing: INDEXED BY puts it in the plan whatever
	// the index looks like. What matters is that the series is sought, not
	// scanned for, and that the order comes from the index.
	p := strings.Join(plan, "; ")
	if !strings.Contains(p, "SEARCH measurements USING INDEX measurements_masked (target_id=? AND agent_id=? AND family=? AND ts>? AND ts<?)") ||
		strings.Contains(p, "TEMP B-TREE") || strings.Contains(p, "SCAN") {
		t.Errorf("plan = %s", p)
	}
}

// Protects the rule that a verdict is filed under the interval it is about. A
// mask that does not decode is not in the history, so the newest interval
// there is the one before; judging that and storing it under this timestamp
// would flag an interval on another one's evidence.
func TestAnIntervalWhoseOwnMaskIsUnreadableIsNotJudged(t *testing.T) {
	s := detecting(t)
	ctx := t.Context()
	for i := range 20 {
		if err := s.WriteMeasurements(ctx, []Measurement{tail(1, 0, int64(1000+i*60))}); err != nil {
			t.Fatal(err)
		}
	}
	before := positionalRows(t, s)
	bad := tail(1, 0, 1000+20*60)
	bad.LossMask = append(bad.LossMask, 0) // one byte too long for its counts
	ms := []Measurement{bad}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if ms[0].Positional != nil || positionalRows(t, s) != before {
		t.Errorf("an interval with an unreadable mask was flagged: %+v, %d rows before, %d after",
			ms[0].Positional, before, positionalRows(t, s))
	}
}

// Protects two things about what a caller and a reader are told. A batch that
// comes in already carrying a flag does not keep it unless this write earned
// it. And a stored flag whose counts cannot be read leaves the interval
// readable and unflagged, as a damaged optional series does, rather than
// failing the whole range.
func TestStaleAndDamagedFlagsAreNotServed(t *testing.T) {
	s := detecting(t)
	ctx := t.Context()
	stale := lossy(1, 0, 1000, 3)
	stale.Positional = &PositionalLoss{P: 0.0001, Intervals: 60, Lost: 60, Counts: make([]int, probes)}
	ms := []Measurement{stale}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if ms[0].Positional != nil {
		t.Errorf("a first interval kept the flag it came in with: %+v", ms[0].Positional)
	}

	for i := 1; i <= 20; i++ {
		if err := s.WriteMeasurements(ctx, []Measurement{tail(1, 0, int64(1000+i*60))}); err != nil {
			t.Fatal(err)
		}
	}
	last := int64(1000 + 20*60)
	if _, err := s.db.Exec("UPDATE positional_loss SET counts = x'01' WHERE ts = ?", last); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryRange(ctx, 1, 0, 0, 0, 1<<40)
	if err != nil {
		t.Fatalf("a damaged flag made the range unreadable: %v", err)
	}
	if n := len(got); n != 21 || got[n-1].Positional != nil || got[n-2].Positional == nil {
		t.Errorf("want the damaged flag dropped and its neighbour kept: last %+v, the one before %+v",
			got[n-1].Positional, got[n-2].Positional)
	}
}

// The counts are stored as bytes and must come back exactly, refusing what is
// not exactly one value per probe.
func TestPositionalCountsRoundTrip(t *testing.T) {
	in := []int{0, 1, 127, 128, 300, 60, 0}
	b := EncodePositionalCounts(in)
	out, err := DecodePositionalCounts(b, len(in))
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %v, %v", out, err)
	}
	for name, bad := range map[string][]byte{
		"trailing garbage": append(append([]byte{}, b...), 0x80),
		"extra value":      append(append([]byte{}, b...), 5),
		"truncated":        b[:len(b)-1],
		"cut mid-varint":   {0x80},
	} {
		if _, err := DecodePositionalCounts(bad, len(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An interval is judged by the window that ends in it, never by what came
// after. A batch written newest first has each row seeing only the older rows
// already in it, so none gets twelve; if a row saw later ones it would be
// flagged on evidence from its own future.
func TestAnIntervalIsJudgedByItsPastOnly(t *testing.T) {
	s := detecting(t)
	var batch []Measurement
	for i := 11; i >= 0; i-- {
		batch = append(batch, tail(1, 0, int64(1000+60*i)))
	}
	if err := s.WriteMeasurements(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if n := positionalRows(t, s); n != 0 {
		t.Errorf("%d intervals flagged on rows written after them", n)
	}
}

// What detection adds to a write, per lossy interval. "tail" is a pattern so
// strong that the detector settles it on its bound; "partly" loses the last
// three probes one time in five, three and two, which is flagged by counting
// every round and is what a partly rate-limited target costs; "scattered" is
// never flagged and stops counting early.
func BenchmarkWriteLossyMeasurements(b *testing.B) {
	for _, kind := range []string{"tail", "partly", "scattered"} {
		for _, detect := range []bool{false, true} {
			name := kind + "/detect=off"
			if detect {
				name = kind + "/detect=on"
			}
			b.Run(name, func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
				if err != nil {
					b.Fatal(err)
				}
				defer s.Close()
				if detect {
					s.DetectPositionalLoss()
				}
				rng := rand.New(rand.NewPCG(1, 1))
				ts := int64(1000)
				next := func() Measurement {
					ts += 60
					switch kind {
					case "tail":
						return tail(1, 0, ts)
					case "partly":
						for {
							var at []int
							for k, p := range []float64{0.2, 0.35, 0.5} {
								if rng.Float64() < p {
									at = append(at, probes-3+k)
								}
							}
							if len(at) > 0 {
								return lossy(1, 0, ts, at...)
							}
						}
					}
					a := rng.IntN(probes)
					return lossy(1, 0, ts, a, (a+1+rng.IntN(probes-1))%probes)
				}
				// Warm: a full window, so every measured write sees one.
				var warm []Measurement
				for range losspattern.Window {
					warm = append(warm, next())
				}
				if err := s.WriteMeasurements(b.Context(), warm); err != nil {
					b.Fatal(err)
				}
				const batch = 10
				b.ResetTimer()
				for b.Loop() {
					ms := make([]Measurement, batch)
					for i := range ms {
						ms[i] = next()
					}
					if err := s.WriteMeasurements(b.Context(), ms); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch), "ns/interval")
			})
		}
	}
}
