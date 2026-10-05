package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
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
//
// What it shows is the seed scaling with the rows per target rather than with
// the rows: a few targets with many rows each, which is what a long-running
// installation looks like. It says nothing about many targets, where the seed
// does a seek per target and the advantage shrinks. The timed seed includes its
// own autocommit INSERT and DELETE, so a very slow disk could fail it with a
// message about reading the history; best-of-three makes that unlikely.
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

	ruleSources := map[string]string{
		"alert_rules":     `INSERT INTO alert_rules (id, target_id, name, metric, op, threshold) VALUES (900, 1, 'n', 'loss', '>', 1)`,
		"alert_state":     `INSERT INTO alert_state (rule_id, target_id, agent_id) VALUES (900, 1, 0)`,
		"alert_events":    `INSERT INTO alert_events (ts, rule_id, target_id, agent_id, firing, rule_name, describes, value) VALUES (1, 900, 1, 0, 1, 'r', 'd', 0)`,
		"alert_baselines": `INSERT INTO alert_baselines (rule_id, target_id, agent_id, from_ts, to_ts, intervals, samples, captured_at) VALUES (900, 1, 0, 1, 2, 1, X'', 1)`,
		"silences":        `INSERT INTO silences (rule_id, starts_at, ends_at, created_at) VALUES (900, 1, 2, 1)`,
	}
	for name, stmt := range ruleSources {
		t.Run("rule id in "+name, func(t *testing.T) {
			s := v21DB(t, stmt)
			if id, err := allocRuleID(t.Context(), s.db); err != nil || id <= 900 {
				t.Errorf("first rule id = %d, %v: an id of 900 is kept in %s", id, err, name)
			}
		})
	}
}

// A silence names a rule by id and has no foreign key to it. With ids reused, a
// silence made for a rule that was then deleted muted whichever rule was created
// next, which may be another customer's.
func TestRuleIDsAreNeverReused(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	mk := func(name string) int64 {
		r := alert.Rule{TargetID: 1, Name: name, Metric: alert.MetricLoss, Op: alert.OpGreater,
			Threshold: 20, For: 3, ClearFor: 3, Enabled: true}
		if err := s.UpsertAlertRule(ctx, &r); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	first, second := mk("a"), mk("b")
	if err := s.DeleteAlertRule(ctx, second); err != nil { // the highest id
		t.Fatal(err)
	}
	third := mk("c")
	if third <= second {
		t.Fatalf("a new rule got id %d after id %d was deleted", third, second)
	}
	if err := s.DeleteAlertRule(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAlertRule(ctx, third); err != nil {
		t.Fatal(err)
	}
	if fourth := mk("d"); fourth <= third {
		t.Fatalf("after deleting every rule, the next got id %d (highest ever was %d)", fourth, third)
	}
}

// An id inserted explicitly above the counter is skipped, not overwritten.
func TestRuleAllocationSkipsAnIdThatIsTaken(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	next, err := allocRuleID(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	squatter := alert.Rule{ID: next + 1, TargetID: 1, Name: "squatter", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 3, ClearFor: 3, Enabled: true}
	if err := s.UpsertAlertRule(ctx, &squatter); err != nil {
		t.Fatal(err)
	}
	fresh := alert.Rule{TargetID: 1, Name: "fresh", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 3, ClearFor: 3, Enabled: true}
	if err := s.UpsertAlertRule(ctx, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.ID == squatter.ID {
		t.Fatalf("the new rule took id %d, which was already a rule", fresh.ID)
	}
	rules, err := s.ListAlertRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.ID == squatter.ID && r.Name != "squatter" {
			t.Fatalf("rule %d was overwritten and is now %q", r.ID, r.Name)
		}
	}
}

// Enrolling agents concurrently must not fail with SQLITE_BUSY. A transaction
// that reads first and writes later, begun the default deferred way, cannot
// upgrade its read lock in WAL mode when another writer has committed in
// between, and the busy timeout does not apply to that: the statement fails at
// once with BUSY_SNAPSHOT. RedeemEnrolmentToken reads the token and then
// allocates and inserts, which is exactly that shape, and an enrolment that
// fails this way loses a single-use token's only chance.
func TestConcurrentEnrolmentDoesNotFailWithBusy(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	const n = 64
	now := time.Unix(1_800_000_000, 0)
	toks := make([]string, n)
	for i := range n {
		tok, err := s.MintEnrolmentToken(ctx, "agent-"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+time.Duration(i).String(), time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		toks[i] = tok.Plaintext
	}
	errs := make(chan error, n)
	ids := make(chan int64, n)
	start := make(chan struct{})
	for i := range n {
		go func() {
			<-start
			a, err := s.RedeemEnrolmentToken(ctx, toks[i], testKey(t), now)
			if err != nil {
				errs <- err
				return
			}
			ids <- a.ID
			errs <- nil
		}()
	}
	close(start)
	failed := 0
	for range n {
		if err := <-errs; err != nil {
			failed++
			if failed <= 3 {
				t.Errorf("enrolment failed: %v", err)
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent enrolments failed", failed, n)
	}
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("agent id %d was handed out twice", id)
		}
		seen[id] = true
	}
}

// A target names its agents in one space-separated string, so an agent name with
// a space is two names the moment it is written there. RenameAgent to "b c"
// rewrote a target's list from "ams-01" into two agents, neither of which exists.
func TestAgentNamesCannotContainWhatSplitsOrHidesThem(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	now := time.Unix(1_800_000_000, 0)
	agent, err := s.AddAgent(ctx, "ams-01", testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("n", 65)
	for name, bad := range map[string]string{
		"a space":              "b c",
		"a tab":                "b\tc",
		"a newline":            "b\nc",
		"a bell":               "b\x07c",
		"an escape":            "b\x1bc",
		"a delete":             "b\x7fc",
		"a non-breaking space": "b c",
		"a line separator":     "b c",
		"an override":          "b‮c",
		"a zero-width joiner":  "b‍c",
		"too long":             long,
		"not UTF-8":            "b\xffc",
		"local":                "local",
		"nothing":              "",
	} {
		if _, err := s.AddAgent(ctx, bad, testKey(t)); err == nil {
			t.Errorf("AddAgent accepted a name with %s", name)
		}
		if _, err := s.MintEnrolmentToken(ctx, bad, time.Hour, now); err == nil && name != "a space" {
			// MintEnrolmentToken trims, so only the edges are removed: a name with
			// whitespace in the middle must still be refused, which the loop covers.
			t.Errorf("MintEnrolmentToken accepted a name with %s", name)
		}
		if _, err := s.RenameAgent(ctx, agent.ID, bad); err == nil {
			t.Errorf("RenameAgent accepted a name with %s", name)
		}
	}
	// Ordinary names, including ones real agents have.
	for _, good := range []string{"ams-01", "site_b", "dc1.example.org", "Rotterdam-Zuid", strings.Repeat("n", 64)} {
		if _, err := s.RenameAgent(ctx, agent.ID, good); err != nil {
			t.Errorf("RenameAgent refused %q: %v", good, err)
		}
	}
}

// A path with a question mark or a hash was cut there as a URI. "a?b.db" opened
// a database called "a", with none of the pragmas, and "a#b.db" the same: the
// right data in the wrong file, without a word.
func TestAPathWithQuestionMarkOrHashOpensThatFile(t *testing.T) {
	for _, name := range []string{"what?.db", "tag#1.db", "both?#.db", "100%.db", "with space.db", "x%41y.db"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("the database is not at %q: %v", path, err)
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), name) {
					t.Errorf("a stray file %q was created instead", e.Name())
				}
			}
			var mode string
			if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
				t.Errorf("journal_mode = %q (%v): the pragmas in the DSN did not apply", mode, err)
			}
			var fk int
			s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk)
			if fk != 1 {
				t.Errorf("foreign_keys = %d: the pragmas in the DSN did not apply", fk)
			}
		})
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// The database holds the key that signs session cookies, so a copy of it, or a
// read of it by another local user, is a way to be any user. A new one is created
// private whatever the umask is, and the sidecar files SQLite writes beside it
// take its mode.
func TestANewDatabaseIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES ('x', 'y')`); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if m := mode(t, p); m&0o077 != 0 {
			t.Errorf("%s is %v, readable beyond its owner", filepath.Base(p), m)
		}
	}
}

// One that exists is not made less accessible to the group an operator chose,
// but is closed to everyone else, and the operator is told.
func TestAnExistingDatabaseLosesWorldAccessAndNothingElse(t *testing.T) {
	for _, c := range []struct{ from, want os.FileMode }{
		{0o644, 0o640},
		{0o666, 0o660},
		{0o640, 0o640},
		{0o600, 0o600},
		{0o604, 0o600},
		{0o602, 0o600},
		{0o601, 0o600},
	} {
		path := filepath.Join(t.TempDir(), "old.db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		// Leftovers of a crashed run: sidecars at the old mode, empty.
		for _, p := range []string{path + "-wal", path + "-shm"} {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Chmod(p, c.from); err != nil {
				t.Fatal(err)
			}
		}
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('x', 'y')`); err != nil {
			t.Fatal(err)
		}
		if got := mode(t, path); got != c.want {
			t.Errorf("a %v database became %v, want %v", c.from, got, c.want)
		}
		for _, p := range []string{path + "-wal", path + "-shm"} {
			if _, err := os.Stat(p); err == nil && mode(t, p)&0o007 != 0 {
				t.Errorf("a %v database left %s world-accessible: %v", c.from, filepath.Base(p), mode(t, p))
			}
		}
		s.Close()
	}
}
