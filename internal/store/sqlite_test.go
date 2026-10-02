package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/timdebruijn/smokeng/internal/tree"
)

func ptr[T any](v T) *T { return &v }

// An existing database must be upgraded in place, with its measurements
// intact: history is the whole point of this project and a migration that
// loses it, or refuses to run, is worse than no migration.
func TestMigrationFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")

	// Build a database as version 1 knew it.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples)
		VALUES (1, 0, 1756400000, 5, 1, 0, X'01A09C01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening a v1 database: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}

	// The pre-existing row survives, and reads back through the new column.
	got, err := s.QueryRange(ctx, 1, LocalAgentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("measurements after migration = %d, want 1", len(got))
	}
	if got[0].Sent != 5 || got[0].ICMPErr != nil {
		t.Errorf("migrated row = %+v, want sent=5 and no ICMP error", got[0])
	}

	// Reopening an already-migrated database must be a no-op, not an error.
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a migrated database: %v", err)
	}
	again.Close()
}

func TestSQLiteRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A fresh database seeds a complete root and passes tree validation.
	targets, err := s.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ID != 1 || targets[0].ParentID != nil {
		t.Fatalf("fresh db targets = %+v, want only the seeded root", targets)
	}
	if _, err := tree.New(targets); err != nil {
		t.Fatalf("seeded root fails tree validation: %v", err)
	}

	child := tree.Target{
		ParentID:      ptr(int64(1)),
		Name:          "cloudflare-v4",
		Host:          ptr("1.1.1.1"),
		AddressFamily: ptr("v4"),
		Enabled:       true,
		Settings:      tree.Settings{PingsPerInterval: ptr(40)},
	}
	if err := s.UpsertTarget(ctx, &child); err != nil {
		t.Fatal(err)
	}
	if child.ID == 0 {
		t.Fatal("UpsertTarget did not assign an id")
	}

	m := Measurement{
		TargetID: child.ID,
		AgentID:  LocalAgentID,
		TS:       1_756_400_000,
		Sent:     20,
		Received: 3,
		Flags:    FlagUserspaceTX,
		Samples:  []uint32{20000, 20150, 21000},
	}
	if err := s.WriteMeasurements(ctx, []Measurement{m}); err != nil {
		t.Fatal(err)
	}
	// Idempotency: rewriting the same row must not duplicate or error.
	if err := s.WriteMeasurements(ctx, []Measurement{m}); err != nil {
		t.Fatal(err)
	}

	got, err := s.QueryRange(ctx, child.ID, LocalAgentID, m.TS, m.TS+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("QueryRange returned %d rows, want 1", len(got))
	}
	if got[0].Sent != 20 || got[0].Received != 3 || got[0].Flags != FlagUserspaceTX ||
		!slices.Equal(got[0].Samples, m.Samples) {
		t.Fatalf("round trip = %+v, want %+v", got[0], m)
	}

	// The received == len(samples) invariant is enforced on write.
	bad := m
	bad.Received = 5
	if err := s.WriteMeasurements(ctx, []Measurement{bad}); err == nil {
		t.Fatal("expected error for received != len(samples)")
	}
}

func TestPruneMeasurements(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "prune.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(name string) int64 {
		tg := tree.Target{
			ParentID: ptr(int64(1)), Name: name, Host: ptr("1.1.1.1"),
			AddressFamily: ptr("v4"), Enabled: true,
		}
		if err := s.UpsertTarget(ctx, &tg); err != nil {
			t.Fatal(err)
		}
		return tg.ID
	}
	kept := mk("kept")   // has retention applied
	other := mk("other") // must be untouched: pruning is scoped to one target

	const base = 1_000_000
	const step = 100
	const n = 10
	write := func(target int64, ts int64) {
		if err := s.WriteMeasurements(ctx, []Measurement{{
			TargetID: target, AgentID: LocalAgentID, TS: ts,
			Sent: 1, Received: 1, Samples: []uint32{1000},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		write(kept, base+int64(i)*step)
		write(other, base+int64(i)*step)
	}

	// Cutoff halfway: rows at base..base+400 (5) are older, base+500..base+900 (5)
	// stay. A slice smaller than the span forces several passes, exercising the
	// chunking loop rather than a single delete.
	cutoff := int64(base + 500)
	deleted, err := s.PruneMeasurements(ctx, kept, cutoff, 150)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 5 {
		t.Fatalf("pruned %d rows, want 5", deleted)
	}

	remaining, err := s.QueryRange(ctx, kept, LocalAgentID, 0, base+n*step)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 5 {
		t.Fatalf("kept target has %d rows after prune, want 5", len(remaining))
	}
	for _, r := range remaining {
		if r.TS < cutoff {
			t.Fatalf("row at %d survived a cutoff of %d", r.TS, cutoff)
		}
	}

	// The other target is whole: nobody set retention on it.
	untouched, err := s.QueryRange(ctx, other, LocalAgentID, 0, base+n*step)
	if err != nil {
		t.Fatal(err)
	}
	if len(untouched) != n {
		t.Fatalf("other target has %d rows, want %d — prune leaked across targets", len(untouched), n)
	}

	// A cutoff older than everything keeps the target forever: nothing to do.
	deleted, err = s.PruneMeasurements(ctx, kept, base-1, 150)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("prune below all data deleted %d rows, want 0", deleted)
	}
}

// The pool has to stay bounded under concurrent readers. Without a limit
// database/sql opens a connection per waiting caller, which is how a slow spell
// turns into an unbounded number of SQLite connections instead of a queue.
func TestPoolIsBounded(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const limit = 8
	if got := s.db.Stats().MaxOpenConnections; got != limit {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, limit)
	}

	// Far more callers than the limit, each holding a read open long enough to
	// overlap the others.
	var wg sync.WaitGroup
	peak := make(chan int, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int
			if err := s.db.QueryRow("select count(*) from targets").Scan(&n); err != nil {
				t.Error(err)
				return
			}
			peak <- s.db.Stats().OpenConnections
		}()
	}
	wg.Wait()
	close(peak)
	for got := range peak {
		if got > limit {
			t.Fatalf("pool grew to %d connections, limit is %d", got, limit)
		}
	}

	// The idle half of the limit, separately: hold the full limit at once, then
	// release it. Without SetMaxIdleConns the pool keeps database/sql's default
	// of two and closes the other six, so every reader after a burst pays to
	// reopen a connection that was just thrown away.
	ctx := context.Background()
	conns := make([]*sql.Conn, 0, limit)
	for range limit {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.db.Stats().Idle; got != limit {
		t.Fatalf("after releasing %d connections, Idle = %d, want %d", limit, got, limit)
	}
}
