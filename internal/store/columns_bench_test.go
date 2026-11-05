package store

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// BenchmarkQueryColumns reads a whole series pooled into 1000 columns, the
// shape of a plot showing a long window. By default the series are small, so
// the benchmark runs in CI as a smoke test. With SMOKENG_BENCH_DIR set, it
// builds (once, then reuses) databases of the sizes long ranges are about:
//
//	SMOKENG_BENCH_DIR=/var/tmp/b go test ./internal/store -run '^$' -bench QueryColumns -benchtime 3x
func BenchmarkQueryColumns(b *testing.B) {
	type size struct {
		name     string
		interval int64
		rows     int
	}
	sizes := []size{{"1d@60s", 60, 1440}}
	dir := os.Getenv("SMOKENG_BENCH_DIR")
	if dir != "" {
		sizes = []size{
			{"30d@60s", 60, 30 * 1440},
			{"1y@60s", 60, 365 * 1440},
			{"30d@1s", 1, 30 * 86400},
		}
	} else {
		dir = b.TempDir()
	}
	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			path := filepath.Join(dir, sz.name+".db")
			_, statErr := os.Stat(path)
			s, err := Open(path)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if statErr != nil {
				benchFill(b, s, sz.interval, sz.rows)
			}
			to := sz.interval * int64(sz.rows)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				raw, cols, err := s.QueryColumns(b.Context(), 1, 0, 0, to, 1000)
				if err != nil || raw != nil || len(cols) != 1000 {
					b.Fatalf("raw %d, cols %d, %v", len(raw), len(cols), err)
				}
			}
			b.ReportMetric(float64(sz.rows)*float64(b.N)/b.Elapsed().Seconds(), "rows/s")
		})
	}
}

// benchFill writes rows intervals of 20 pings, latencies around 10 ms.
func benchFill(b *testing.B, s *SQLite, interval int64, rows int) {
	b.Helper()
	rng := rand.New(rand.NewPCG(5, 6))
	const batch = 10_000
	for at := 0; at < rows; at += batch {
		var ms []Measurement
		for i := at; i < min(at+batch, rows); i++ {
			samples := make([]uint32, 20)
			for j := range samples {
				samples[j] = uint32(9000 + rng.IntN(3000))
			}
			slices.Sort(samples)
			ms = append(ms, Measurement{TargetID: 1, TS: int64(i) * interval, Sent: 20, Received: 20, Samples: samples})
		}
		if err := s.WriteMeasurements(b.Context(), ms); err != nil {
			b.Fatal(fmt.Errorf("fill at row %d: %w", at, err))
		}
	}
}
