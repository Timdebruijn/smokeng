package store

import (
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Protects an installation upgrading to v29 against losing alert state, history
// or silences to the rebuild of alert_state, against an acknowledged firing
// alert coming back as a fresh one, and against history being filed under a
// family it was not measured over. A state is about now and gets the target's
// current family; an event and a baseline are history and get the family v28
// gave a measurement of that agent at that time; a silence keeps covering every
// family. Every other column keeps every byte.
func TestV29GivesAlertsTheirFamilyAndKeepsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v28.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := range 28 {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	const (
		v4Target   = 10 // always v4
		v6Target   = 11 // always v6
		flipTarget = 12 // was v4, resolved a v6 address at 2000, is v6 now
		group      = 15 // no host and no family
		gone       = 99 // no row in targets
		remote     = 5  // an agent other than the local prober
	)
	for _, tg := range []struct {
		id  int
		fam string
	}{{v4Target, "v4"}, {v6Target, "v6"}, {flipTarget, "v6"}} {
		exec(`INSERT INTO targets (id, parent_id, name, host, address_family) VALUES (?, 1, ?, 'h', ?)`,
			tg.id, fmt.Sprintf("t%d", tg.id), tg.fam)
	}
	exec(`INSERT INTO targets (id, parent_id, name) VALUES (?, 1, 'g')`, group)
	exec(`INSERT INTO agents (id, name) VALUES (?, 'remote')`, remote)
	for _, r := range []struct {
		target, ts int
		addr       string
	}{{flipTarget, 1000, "192.0.2.2"}, {flipTarget, 2000, "2001:db8::2"}} {
		fam := 4
		if r.addr[0] == '2' {
			fam = 6
		}
		exec(`INSERT INTO resolutions (target_id, family, ts, address) VALUES (?, ?, ?, ?)`, r.target, fam, r.ts, r.addr)
	}
	exec(`PRAGMA user_version = 28`)

	// Rule 1 carries the states and events; rules 2 to 5 carry one baseline each
	// (a baseline is one per rule).
	for id := 1; id <= 5; id++ {
		exec(`INSERT INTO alert_rules (id, target_id, name, metric, op, threshold) VALUES (?, 1, ?, 'loss', '>', 10)`,
			id, fmt.Sprintf("r%d", id))
	}

	// Alert state: the family is the target's current one, whatever the agent.
	type stateRow struct {
		target, agent int
		want          tree.Family
	}
	states := []stateRow{
		{v4Target, 0, tree.FamilyV4}, {v6Target, 0, tree.FamilyV6},
		{flipTarget, 0, tree.FamilyV6}, {flipTarget, remote, tree.FamilyV6},
		{group, 0, tree.FamilyUnknown}, {gone, 0, tree.FamilyUnknown},
	}
	for i, st := range states {
		// The first is firing and acknowledged, the second firing and not, the
		// rest clear with a streak: all three shapes a state can have.
		switch {
		case i == 0:
			exec(`INSERT INTO alert_state (rule_id, target_id, agent_id, firing, since, streak, last_ts, value, acked_since, acked_at, acked_by)
				VALUES (1, ?, ?, 1, 1500, 0, 2500, 42.5, 1500, 1600, 'tim@example.org')`, st.target, st.agent)
		case i == 1:
			exec(`INSERT INTO alert_state (rule_id, target_id, agent_id, firing, since, streak, last_ts, value)
				VALUES (1, ?, ?, 1, 1700, 0, 2500, 55)`, st.target, st.agent)
		default:
			exec(`INSERT INTO alert_state (rule_id, target_id, agent_id, firing, streak, last_ts, value)
				VALUES (1, ?, ?, 0, 2, 2500, 1.25)`, st.target, st.agent)
		}
	}

	// Events: history at a timestamp, by the rule v28 gave measurements. The
	// switch of target 12 is at 2000: the local prober's rows from then on are
	// v6, earlier ones and every remote one are unknown.
	type eventRow struct {
		target, agent, ts int
		want              tree.Family
	}
	events := []eventRow{
		{v4Target, 0, 1500, tree.FamilyV4}, {v4Target, remote, 1500, tree.FamilyV4},
		{v6Target, 0, 1500, tree.FamilyV6},
		{flipTarget, 0, 1500, tree.FamilyUnknown}, {flipTarget, 0, 2500, tree.FamilyV6},
		{flipTarget, remote, 2500, tree.FamilyUnknown},
		{group, 0, 1500, tree.FamilyUnknown}, {gone, 0, 1500, tree.FamilyUnknown},
	}
	for _, e := range events {
		exec(`INSERT INTO alert_events (ts, rule_id, target_id, agent_id, firing, rule_name, describes, value)
			VALUES (?, 1, ?, ?, 1, 'r1', 'loss > 10% for 3 intervals', 33.5)`, e.ts, e.target, e.agent)
	}

	// Baselines are judged at from_ts, rule by rule.
	baselines := []struct {
		rule, target, agent, from int
		want                      tree.Family
	}{
		{2, v4Target, 0, 1500, tree.FamilyV4},
		{3, flipTarget, 0, 1500, tree.FamilyUnknown}, // before the switch
		{4, flipTarget, 0, 2500, tree.FamilyV6},
		{5, gone, 0, 1500, tree.FamilyUnknown},
	}
	for _, b := range baselines {
		exec(`INSERT INTO alert_baselines (rule_id, target_id, agent_id, from_ts, to_ts, intervals, samples, captured_at, captured_by)
			VALUES (?, ?, ?, ?, ?, 3, ?, 9000, 'tim@example.org')`,
			b.rule, b.target, b.agent, b.from, b.from+3000, []byte{1, byte(b.rule), 0x9c})
	}

	exec(`INSERT INTO silences (target_id, agent_id, rule_id, starts_at, ends_at, reason, created_by, created_at) VALUES (NULL, NULL, NULL, 1, 9000, 'all', 'a', 1)`)
	exec(`INSERT INTO silences (target_id, agent_id, rule_id, starts_at, ends_at, reason, created_by, created_at) VALUES (?, 0, NULL, 1, 9000, 'one target', 'b', 2)`, v4Target)
	exec(`INSERT INTO silences (target_id, agent_id, rule_id, starts_at, ends_at, reason, created_by, created_at) VALUES (NULL, NULL, 1, 5000, 9000, 'one rule', 'c', 3)`)

	tables := []struct {
		name, key, rest string
	}{
		{"alert_state", "rule_id||'/'||target_id||'/'||agent_id",
			"quote(firing)||quote(since)||quote(streak)||quote(last_ts)||quote(value)||quote(acked_since)||quote(acked_at)||quote(acked_by)"},
		{"alert_events", "id",
			"quote(ts)||quote(rule_id)||quote(target_id)||quote(agent_id)||quote(firing)||quote(rule_name)||quote(describes)||quote(value)"},
		{"alert_baselines", "rule_id",
			"quote(target_id)||quote(agent_id)||quote(from_ts)||quote(to_ts)||quote(intervals)||quote(samples)||quote(captured_at)||quote(captured_by)"},
		{"silences", "id",
			"quote(target_id)||quote(agent_id)||quote(rule_id)||quote(starts_at)||quote(ends_at)||quote(reason)||quote(created_by)||quote(created_at)"},
	}
	before := map[string]map[string]string{}
	columnsBefore := map[string][]string{}
	for _, tb := range tables {
		before[tb.name] = dumpTable(t, db, tb.name, tb.key, tb.rest)
		if len(before[tb.name]) == 0 {
			t.Fatalf("%s: nothing to migrate", tb.name)
		}
		columnsBefore[tb.name] = columnsOf(t, db, tb.name)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v28 database: %v", err)
	}
	defer s.Close()

	// A column that lost its type, its NOT NULL or its default in the rebuilt
	// alert_state would pass the row checks below and bite on the next insert.
	for _, tb := range tables {
		if got := columnsOf(t, s.db, tb.name); !slices.Equal(got, columnsBefore[tb.name]) {
			t.Errorf("%s: columns besides family changed:\nbefore %v\nafter  %v", tb.name, columnsBefore[tb.name], got)
		}
		if after := dumpTable(t, s.db, tb.name, tb.key, tb.rest); !maps.Equal(before[tb.name], after) {
			t.Errorf("%s: rows or bytes changed by v29:\nbefore %v\nafter  %v", tb.name, before[tb.name], after)
		}
	}

	wantStates := map[string]tree.Family{}
	for _, st := range states {
		wantStates[fmt.Sprintf("1/%d/%d", st.target, st.agent)] = st.want
	}
	if got := dumpFamilies(t, s.db, "alert_state", tables[0].key); !maps.Equal(got, wantStates) {
		t.Errorf("alert_state families = %v, want %v", got, wantStates)
	}

	// The acknowledged firing alert is still firing and still acknowledged, for
	// the family it had: an ack that stopped matching its episode would shout
	// again on the first evaluation after the upgrade.
	loaded, err := s.ListAlertStates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, st := range loaded {
		if st.TargetID == v4Target {
			found = true
			if !st.Firing || !st.Acked() || st.Family != tree.FamilyV4 || st.AckedBy != "tim@example.org" || st.Value != 42.5 {
				t.Errorf("the acknowledged firing state came back as %+v", st)
			}
		}
	}
	if !found {
		t.Error("the acknowledged firing state is gone")
	}

	wantEvents := map[string]tree.Family{}
	for _, e := range events {
		wantEvents[fmt.Sprintf("%d/%d/%d", e.target, e.agent, e.ts)] = e.want
	}
	gotEvents := map[string]tree.Family{}
	evs, err := s.ListAlertEvents(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		gotEvents[fmt.Sprintf("%d/%d/%d", e.TargetID, e.AgentID, e.TS)] = e.Family
	}
	if !maps.Equal(gotEvents, wantEvents) {
		t.Errorf("alert_events families = %v, want %v", gotEvents, wantEvents)
	}

	wantBaselines := map[string]tree.Family{}
	for _, b := range baselines {
		wantBaselines[fmt.Sprint(b.rule)] = b.want
	}
	if got := dumpFamilies(t, s.db, "alert_baselines", "rule_id"); !maps.Equal(got, wantBaselines) {
		t.Errorf("alert_baselines families = %v, want %v", got, wantBaselines)
	}

	sils, err := s.ListSilences(t.Context())
	if err != nil || len(sils) != 3 {
		t.Fatalf("silences after v29: %d, %v", len(sils), err)
	}
	for _, sl := range sils {
		if sl.Family != nil {
			t.Errorf("silence %q got family %v: an existing silence must keep covering every family", sl.Reason, *sl.Family)
		}
	}

	// The key now holds the family, and the foreign key to the rules is kept.
	if _, err := s.db.Exec(`INSERT INTO alert_state (rule_id, target_id, agent_id, family) VALUES (1, ?, 0, 6)`, v4Target); err != nil {
		t.Errorf("a second family of the same (rule, target, agent) is refused: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO alert_state (rule_id, target_id, agent_id, family) VALUES (1, ?, 0, 6)`, v4Target); err == nil {
		t.Error("the same series twice was accepted: family is not part of the primary key together with the rest")
	}
	if _, err := s.db.Exec(`INSERT INTO alert_state (rule_id, target_id, agent_id, family) VALUES (777, 1, 0, 4)`); err == nil {
		t.Error("alert_state kept no foreign key to alert_rules")
	}
	var leftover int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('family_backfill', 'alert_state_new')`).Scan(&leftover); err != nil || leftover != 0 {
		t.Errorf("helper tables left behind: %d, %v", leftover, err)
	}
	fk, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer fk.Close()
	if fk.Next() {
		t.Error("foreign_key_check reports a violation after v29")
	}
}

// Protects the store round trip of the new columns: a family written is the
// family read, a silence without one stays without one, and the states of the
// two families of one (rule, target, agent) are rows of their own.
func TestAlertFamilyRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	rule := alert.Rule{TargetID: 1, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 10, For: 3, ClearFor: 3, Enabled: true}
	if err := s.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}

	v6 := tree.FamilyV6
	scoped := alert.Silence{Family: &v6, StartsAt: 1, EndsAt: 9, CreatedAt: 1}
	every := alert.Silence{StartsAt: 1, EndsAt: 9, CreatedAt: 1}
	for _, sil := range []*alert.Silence{&scoped, &every} {
		if err := s.CreateSilence(ctx, sil); err != nil {
			t.Fatal(err)
		}
	}
	sils, err := s.ListSilences(ctx)
	if err != nil || len(sils) != 2 {
		t.Fatalf("silences: %d, %v", len(sils), err)
	}
	for _, g := range sils {
		switch {
		case g.ID == scoped.ID && (g.Family == nil || *g.Family != tree.FamilyV6):
			t.Errorf("a silence scoped to v6 came back as %v", g.Family)
		case g.ID == every.ID && g.Family != nil:
			t.Errorf("a silence for every family came back scoped to %v", *g.Family)
		}
	}

	if err := s.SaveAlertStates(ctx, []alert.State{
		{RuleID: rule.ID, TargetID: 7, Family: tree.FamilyV4, Firing: true, Since: 100, LastTS: 200},
		{RuleID: rule.ID, TargetID: 7, Family: tree.FamilyV6, Streak: 1, LastTS: 200},
	}); err != nil {
		t.Fatal(err)
	}
	states, err := s.ListAlertStates(ctx)
	if err != nil || len(states) != 2 {
		t.Fatalf("states: %d, %v: the two families must not replace each other", len(states), err)
	}
	for _, st := range states {
		if (st.Family == tree.FamilyV4) != st.Firing {
			t.Errorf("state %+v: the firing flag followed the wrong family", st)
		}
	}

	if err := s.RecordAlertEvents(ctx, []alert.Event{{TS: 5, RuleID: rule.ID, TargetID: 7, Family: tree.FamilyV6,
		Firing: true, RuleName: "loss", Describes: "d"}}); err != nil {
		t.Fatal(err)
	}
	if evs, err := s.ListAlertEvents(ctx, 10); err != nil || len(evs) != 1 || evs[0].Family != tree.FamilyV6 {
		t.Errorf("event family: %+v, %v", evs, err)
	}
	if err := s.SaveAlertBaseline(ctx, &alert.Baselined{RuleID: rule.ID, TargetID: 7, Family: tree.FamilyV6,
		FromTS: 1, ToTS: 2, Intervals: 1, Samples: []uint32{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if bs, err := s.ListAlertBaselines(ctx); err != nil || len(bs) != 1 || bs[0].Family != tree.FamilyV6 {
		t.Errorf("baseline family: %+v, %v", bs, err)
	}
}
