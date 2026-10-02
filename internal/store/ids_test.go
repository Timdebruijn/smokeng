package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/tree"
)

func newTarget(t *testing.T, s *SQLite, name string) int64 {
	t.Helper()
	root := int64(1)
	tg := tree.Target{ParentID: &root, Name: name, Enabled: true}
	if err := s.UpsertTarget(t.Context(), &tg); err != nil {
		t.Fatal(err)
	}
	return tg.ID
}

// A deleted target's id must not come back. Everything keyed by target id is
// kept when a target is deleted, so a reused id hands the new target the old
// one's measurements and traceroute hops.
func TestTargetIDsAreNeverReused(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	a, b, c := newTarget(t, s, "a"), newTarget(t, s, "b"), newTarget(t, s, "c")
	if err := s.DeleteTarget(ctx, c); err != nil { // the highest id
		t.Fatal(err)
	}
	d := newTarget(t, s, "d")
	if d <= c {
		t.Fatalf("a new target got id %d after id %d was deleted", d, c)
	}
	// Deleting everything still does not rewind it.
	for _, id := range []int64{a, b, d} {
		if err := s.DeleteTarget(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if e := newTarget(t, s, "e"); e <= d {
		t.Fatalf("after deleting every target, the next got id %d (highest ever was %d)", e, d)
	}
}

// The consequence that matters: the deleted target's history is not the new
// target's.
func TestANewTargetDoesNotInheritADeletedTargetsHistory(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	old := newTarget(t, s, "customer-a-core")
	if err := s.WriteMeasurements(ctx, []Measurement{
		{TargetID: old, AgentID: 0, TS: 1000, Sent: 1, Received: 1, Samples: []uint32{10}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTarget(ctx, old); err != nil {
		t.Fatal(err)
	}
	fresh := newTarget(t, s, "customer-b-core")
	got, err := s.QueryRange(ctx, fresh, 0, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a new target (id %d) read %d measurement(s) of deleted target %d", fresh, len(got), old)
	}
}

// An id inserted explicitly above the counter must not be handed out and then
// overwritten: the upsert is ON CONFLICT DO UPDATE, so an id that is taken
// would replace that target instead of creating one.
func TestAllocationSkipsAnIdThatIsTaken(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	next, err := allocTargetID(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	root := int64(1)
	squatter := tree.Target{ID: next + 1, ParentID: &root, Name: "squatter", Enabled: true}
	if err := s.UpsertTarget(ctx, &squatter); err != nil {
		t.Fatal(err)
	}
	fresh := newTarget(t, s, "fresh")
	if fresh == squatter.ID {
		t.Fatalf("the new target took id %d, which was already a target", fresh)
	}
	all, err := s.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range all {
		if tg.ID == squatter.ID && tg.Name != "squatter" {
			t.Fatalf("target %d was overwritten and is now %q", tg.ID, tg.Name)
		}
	}
}

// The same for agents, by both ways of enrolling one. A removed agent's
// measurements, paths and alert state are kept.
func TestAgentIDsAreNeverReused(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	first, err := s.AddAgent(ctx, "a", testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	last, err := s.AddAgent(ctx, "b", testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAgent(ctx, last.ID); err != nil {
		t.Fatal(err)
	}
	// Manual enrolment.
	viaAdd, err := s.AddAgent(ctx, "c", testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if viaAdd.ID <= last.ID {
		t.Fatalf("AddAgent gave id %d after id %d was removed", viaAdd.ID, last.ID)
	}
	if err := s.RemoveAgent(ctx, viaAdd.ID); err != nil {
		t.Fatal(err)
	}
	// Token enrolment, which allocates inside its own transaction.
	now := time.Unix(1_800_000_000, 0)
	tok, err := s.MintEnrolmentToken(ctx, "d", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	viaToken, err := s.RedeemEnrolmentToken(ctx, tok.Plaintext, testKey(t), now)
	if err != nil {
		t.Fatal(err)
	}
	if viaToken.ID <= viaAdd.ID {
		t.Fatalf("token enrolment gave id %d after id %d was removed", viaToken.ID, viaAdd.ID)
	}
	if first.ID == viaToken.ID {
		t.Fatalf("an id was handed out twice: %d", first.ID)
	}
}

// Ids that left their table but not the database still count. History, paths
// and alert state were kept when their target or agent was deleted, so a
// counter that began at the highest id still present would hand one of those
// back the first time it was asked, in a database that had already had the
// problem.
func TestMigrationSeedsAboveIdsOnlyHistoryStillHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v21.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 21 { // everything up to v21
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples) VALUES (50, 9, 1000, 1, 0, 0, X'')`,
		// The largest agent id sits under neither the first nor the last target,
		// so a skip-scan that stopped early or took the wrong row would miss it.
		`INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples) VALUES (2, 20, 1000, 1, 0, 0, X'')`,
		`INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples) VALUES (5, 3, 1000, 1, 0, 0, X'')`,
		`INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples) VALUES (9, 11, 1000, 1, 0, 0, X'')`,
		`INSERT INTO measurement_series (target_id, agent_id, ts, series, samples) VALUES (60, 12, 1000, 'ipdv_send', X'')`,
		`INSERT INTO paths (target_id, agent_id, ts, hops) VALUES (70, 15, 1000, '10.0.0.1')`,
		`INSERT INTO resolutions (target_id, ts, address) VALUES (80, 1000, '10.0.0.2')`,
		`PRAGMA user_version = 21`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v21 database: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	tid, err := allocTargetID(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	if tid <= 80 {
		t.Errorf("first target id after migration = %d, but ids up to 80 are in use by history", tid)
	}
	aid, err := allocAgentID(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	if aid <= 20 {
		t.Errorf("first agent id after migration = %d, but ids up to 20 are in use by history", aid)
	}
}

// A fresh database has the root target (1) and the built-in local agent (0).
func TestFreshDatabaseAllocatesAfterTheBuiltIns(t *testing.T) {
	s := openTemp(t)
	if id, err := allocTargetID(t.Context(), s.db); err != nil || id != 2 {
		t.Errorf("first target id = %d, %v; want 2", id, err)
	}
	if id, err := allocAgentID(t.Context(), s.db); err != nil || id != 1 {
		t.Errorf("first agent id = %d, %v; want 1", id, err)
	}
}

// The agent-id seed has to find the largest agent id in measurements without
// reading measurements. It is a WITHOUT ROWID table, so its primary key is the
// table and a scan reads every blob: harmless on a young database, minutes on
// the one this is built to become.
//
// EXPLAIN QUERY PLAN cannot see this: SQLite reports a full scan of such a
// table as SEARCH ... USING PRIMARY KEY, the same words as a seek. So this
// measures it, as a ratio against the naive query on the same rows, which holds
// on a slow machine and a fast one alike.
func TestAgentIDSeedReadsFarLessThanTheHistory(t *testing.T) {
	s := openTemp(t)
	if _, err := s.db.Exec(`
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 200000)
		INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples)
		SELECT (i % 4) + 1, 0, i, 1, 0, 0, X'' FROM n`); err != nil {
		t.Fatal(err)
	}
	var seed string
	for _, st := range strings.Split(seedIDsMigration(), ";\n") {
		if strings.Contains(st, "INSERT INTO agent_ids") {
			seed = st
		}
	}
	if seed == "" {
		t.Fatal("seed statement not found")
	}
	best := func(f func()) time.Duration {
		d := time.Hour
		for range 3 {
			start := time.Now()
			f()
			if e := time.Since(start); e < d {
				d = e
			}
		}
		return d
	}
	naive := best(func() {
		var m int64
		if err := s.db.QueryRow("SELECT MAX(agent_id) FROM measurements").Scan(&m); err != nil {
			t.Fatal(err)
		}
	})
	seeded := best(func() {
		if _, err := s.db.Exec(seed); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("DELETE FROM agent_ids"); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("naive MAX(agent_id): %v, seed: %v", naive, seeded)
	if seeded*10 > naive {
		t.Errorf("the agent-id seed took %v against %v for a pass over the history: it is reading the history", seeded, naive)
	}
}

// v21DB builds a database as version 21 left it, with the given rows added,
// then migrates it by opening it.
func v21DB(t *testing.T, stmts ...string) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v21.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 21 {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	for _, q := range append(stmts, `PRAGMA user_version = 21`) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v21 database: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Each place an id can be left behind has to be read by the seed. A source
// left out is only visible when it alone holds the highest id, so each one is
// tried alone: a seed that forgot silently-kept alert state or an old token
// would otherwise pass.
func TestMigrationSeedReadsEveryPlaceAnIdIsKept(t *testing.T) {
	targetSources := map[string]string{
		"targets":            `INSERT INTO targets (id, parent_id, name) VALUES (700, 1, 'x')`,
		"measurements":       `INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples) VALUES (700, 0, 1, 1, 0, 0, X'')`,
		"measurement_series": `INSERT INTO measurement_series (target_id, agent_id, ts, series, samples) VALUES (700, 0, 1, 's', X'')`,
		"resolutions":        `INSERT INTO resolutions (target_id, ts, address) VALUES (700, 1, 'a')`,
		"paths":              `INSERT INTO paths (target_id, agent_id, ts, hops) VALUES (700, 0, 1, 'h')`,
		"alert_state":        `INSERT INTO alert_state (rule_id, target_id, agent_id) VALUES (1, 700, 0)`,
		"alert_events":       `INSERT INTO alert_events (ts, rule_id, target_id, agent_id, firing, rule_name, describes, value) VALUES (1, 1, 700, 0, 1, 'r', 'd', 0)`,
		"alert_baselines":    `INSERT INTO alert_baselines (rule_id, target_id, agent_id, from_ts, to_ts, intervals, samples, captured_at) VALUES (1, 700, 0, 1, 2, 1, X'', 1)`,
		"alert_rules":        `INSERT INTO alert_rules (id, target_id, name, metric, op, threshold) VALUES (1, 700, 'n', 'loss', '>', 1)`,
		"silences":           `INSERT INTO silences (target_id, starts_at, ends_at, created_at) VALUES (700, 1, 2, 1)`,
		"grants":             `INSERT INTO grants (group_name, target_id, role) VALUES ('g', 700, 'viewer')`,
	}
	for name, stmt := range targetSources {
		t.Run("target id in "+name, func(t *testing.T) {
			s := v21DB(t, stmt)
			if id, err := allocTargetID(t.Context(), s.db); err != nil || id <= 700 {
				t.Errorf("first target id = %d, %v: an id of 700 is kept in %s", id, err, name)
			}
		})
	}

	agentSources := map[string]string{
		"agents":             `INSERT INTO agents (id, name) VALUES (800, 'x')`,
		"measurements":       `INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples) VALUES (1, 800, 1, 1, 0, 0, X'')`,
		"measurement_series": `INSERT INTO measurement_series (target_id, agent_id, ts, series, samples) VALUES (1, 800, 1, 's', X'')`,
		"paths":              `INSERT INTO paths (target_id, agent_id, ts, hops) VALUES (1, 800, 1, 'h')`,
		"alert_state":        `INSERT INTO alert_state (rule_id, target_id, agent_id) VALUES (1, 1, 800)`,
		"alert_events":       `INSERT INTO alert_events (ts, rule_id, target_id, agent_id, firing, rule_name, describes, value) VALUES (1, 1, 1, 800, 1, 'r', 'd', 0)`,
		"alert_baselines":    `INSERT INTO alert_baselines (rule_id, target_id, agent_id, from_ts, to_ts, intervals, samples, captured_at) VALUES (1, 1, 800, 1, 2, 1, X'', 1)`,
		"silences":           `INSERT INTO silences (target_id, agent_id, starts_at, ends_at, created_at) VALUES (1, 800, 1, 2, 1)`,
		"enrolment_tokens":   `INSERT INTO enrolment_tokens (token_hash, name, created_at, expires_at, agent_id) VALUES (X'01', 'n', 1, 2, 800)`,
	}
	for name, stmt := range agentSources {
		t.Run("agent id in "+name, func(t *testing.T) {
			s := v21DB(t, stmt)
			if id, err := allocAgentID(t.Context(), s.db); err != nil || id <= 800 {
				t.Errorf("first agent id = %d, %v: an id of 800 is kept in %s", id, err, name)
			}
		})
	}
}
