package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/timdebruijn/smokeng/internal/tree"
)

// dumpTable returns, per row, a key and a rendering of every column but
// family, so a table can be compared before and after the rebuild without
// naming its columns twice. quote() renders blobs as hex, so a changed byte
// shows.
func dumpTable(t *testing.T, db *sql.DB, table, key, rest string) map[string]string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("SELECT %s, %s FROM %s", key, rest, table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		if _, dup := out[k]; dup {
			t.Fatalf("%s: two rows at %s", table, k)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Protects the history of an installation upgrading to v28 against being filed
// under a family it was not measured over, and against being lost or changed
// by the rebuild of five tables. A row keeps every byte; its family is the
// target's current one when nothing says otherwise, and 0 (unknown) where the
// log shows the family was switched and the row cannot be placed on either
// side of the switch, or the target is gone. A guess here would be permanent:
// nothing could later tell a guessed 6 from a measured one.
func TestV28GivesEveryRowItsFamilyAndKeepsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v27.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := range 27 {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}

	const (
		v4Target   = 10 // always v4
		v6Target   = 11 // always v6
		flipTarget = 12 // was v4, resolved a v6 address at 2000, is v6 now
		gone       = 99 // no row in targets
		remote     = 5  // an agent other than the local prober
	)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, tg := range []struct {
		id  int
		fam string
	}{{v4Target, "v4"}, {v6Target, "v6"}, {flipTarget, "v6"}} {
		exec(`INSERT INTO targets (id, parent_id, name, host, address_family) VALUES (?, 1, ?, 'h', ?)`,
			tg.id, fmt.Sprintf("t%d", tg.id), tg.fam)
	}
	exec(`INSERT INTO agents (id, name) VALUES (?, 'remote')`, remote)
	for _, r := range []struct {
		target, ts int
		addr       string
	}{
		{v4Target, 500, "192.0.2.1"}, {v6Target, 500, "2001:db8::1"},
		{flipTarget, 1000, "192.0.2.2"}, {flipTarget, 2000, "2001:db8::2"},
		{gone, 500, "192.0.2.3"},
	} {
		exec(`INSERT INTO resolutions (target_id, ts, address) VALUES (?, ?, ?)`, r.target, r.ts, r.addr)
	}

	// The same three intervals for every (target, agent), each row with bytes
	// of its own.
	want := map[string]tree.Family{} // "target/agent/ts" -> family
	wantFamily := func(target, agent, ts int) tree.Family {
		switch target {
		case v4Target:
			return tree.FamilyV4
		case v6Target:
			return tree.FamilyV6
		case flipTarget:
			if agent == 0 && ts >= 2000 {
				return tree.FamilyV6
			}
		}
		return tree.FamilyUnknown
	}
	for _, target := range []int{v4Target, v6Target, flipTarget, gone} {
		for _, agent := range []int{0, remote} {
			for _, ts := range []int{1500, 2000, 2500} {
				salt := byte(target + agent*7 + ts)
				exec(`INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples, icmp_error, submitted, send_error, loss_mask)
					VALUES (?, ?, ?, 5, 1, 1, ?, 771, ?, 2, ?)`,
					target, agent, ts, []byte{1, salt, 0x9c, 1}, ts%2, []byte{0, salt})
				exec(`INSERT INTO measurement_series (target_id, agent_id, ts, series, samples) VALUES (?, ?, ?, 'ipdv_send', ?)`,
					target, agent, ts, []byte{salt, 1, 2})
				exec(`INSERT INTO positional_loss (target_id, agent_id, ts, p, intervals, lost, counts) VALUES (?, ?, ?, 0.001, 12, 36, ?)`,
					target, agent, ts, []byte{salt, 9})
				exec(`INSERT INTO paths (target_id, agent_id, ts, hops, submitted) VALUES (?, ?, ?, ?, ?)`,
					target, agent, ts, fmt.Sprintf("10.0.0.%d,*", salt), ts%2)
				want[fmt.Sprintf("%d/%d/%d", target, agent, ts)] = wantFamily(target, agent, ts)
			}
		}
	}
	exec(`PRAGMA user_version = 27`)

	key := "target_id||'/'||agent_id||'/'||ts"
	tables := []struct {
		name, key, rest string
	}{
		{"measurements", key, "quote(sent)||quote(received)||quote(flags)||quote(samples)||quote(icmp_error)||quote(submitted)||quote(send_error)||quote(loss_mask)"},
		{"measurement_series", key + "||'/'||series", "quote(samples)"},
		{"positional_loss", key, "quote(p)||quote(intervals)||quote(lost)||quote(counts)"},
		{"paths", key, "quote(hops)||quote(submitted)"},
		{"resolutions", "target_id||'/'||ts", "quote(address)"},
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
		t.Fatalf("migrating a v27 database: %v", err)
	}
	defer s.Close()

	// The rebuild writes each table's definition out again by hand. A column
	// that lost its type, its NOT NULL or its default there would pass every
	// check on the rows above and bite on the next insert that relies on it.
	for _, tb := range tables {
		if got := columnsOf(t, s.db, tb.name); !slices.Equal(got, columnsBefore[tb.name]) {
			t.Errorf("%s: columns besides family changed:\nbefore %v\nafter  %v", tb.name, columnsBefore[tb.name], got)
		}
	}

	for _, tb := range tables {
		after := dumpTable(t, s.db, tb.name, tb.key, tb.rest)
		if !maps.Equal(before[tb.name], after) {
			t.Errorf("%s: rows or bytes changed by the rebuild:\nbefore %v\nafter  %v", tb.name, before[tb.name], after)
		}
	}

	for _, table := range []string{"measurements", "measurement_series", "positional_loss", "paths"} {
		keyed := key
		if table == "measurement_series" {
			keyed += "||'/'||series"
		}
		fams := dumpFamilies(t, s.db, table, keyed)
		for k, got := range fams {
			var target, agent, ts int
			if _, err := fmt.Sscanf(k, "%d/%d/%d", &target, &agent, &ts); err != nil {
				t.Fatal(err)
			}
			if w := wantFamily(target, agent, ts); got != w {
				t.Errorf("%s %s: family %v, want %v", table, k, got, w)
			}
		}
	}
	if got := dumpFamilies(t, s.db, "resolutions", "target_id||'/'||ts"); !maps.Equal(got, map[string]tree.Family{
		"10/500": tree.FamilyV4, "11/500": tree.FamilyV6, "12/1000": tree.FamilyV4, "12/2000": tree.FamilyV6, "99/500": tree.FamilyV4,
	}) {
		t.Errorf("resolutions families = %v", got)
	}

	var names []string
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE name IN
		('measurements_pending', 'measurements_masked', 'paths_pending', 'family_backfill') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if !slices.Equal(names, []string{"measurements_masked", "measurements_pending", "paths_pending"}) {
		t.Errorf("indexes and helper table after v28 = %v, want the three indexes and no family_backfill", names)
	}
	fk, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer fk.Close()
	if fk.Next() {
		t.Error("foreign_key_check reports a violation after v28")
	}

	// And it reads: the flipped target's local rows from the switch on.
	got, err := s.AvailabilitySeries(context.Background(), flipTarget, LocalAgentID, tree.FamilyV6, 0, 1<<40)
	if err != nil || len(got) != 2 || got[0].TS != 2000 || got[1].TS != 2500 {
		t.Errorf("flipped target, local, v6: %d rows, %v", len(got), err)
	}
}

// columnsOf lists a table's columns other than family, each with its type,
// nullability and default, sorted by name: the rebuild may place them
// differently, and nothing reads them by position.
func columnsOf(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name||' '||type||' notnull='||"notnull"||' default='||quote(dflt_value)
		FROM pragma_table_info(?) WHERE name <> 'family' ORDER BY name`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		t.Fatalf("%s: no columns", table)
	}
	return out
}

func dumpFamilies(t *testing.T, db *sql.DB, table, key string) map[string]tree.Family {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("SELECT %s, family FROM %s", key, table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]tree.Family{}
	for rows.Next() {
		var k string
		var f tree.Family
		if err := rows.Scan(&k, &f); err != nil {
			t.Fatal(err)
		}
		out[k] = f
	}
	return out
}

// Protects the reason the family is in the key: one interval of one target
// measured over both families is two rows, and neither read, nor write, nor
// the agent outbox may mix them. Keyed without family the second write
// replaced the first, and a read returned whichever the caller asked for
// along with the other's series and flag.
func TestTwoFamiliesAtOneTimestampAreTwoSeries(t *testing.T) {
	s := detecting(t)
	ctx := t.Context()
	const target, agent = 1, 4

	// v4 has the history that makes its intervals losing at fixed positions;
	// v6 has one interval, at a timestamp v4 has too, and nothing to judge.
	var v4 []Measurement
	for i := range 20 {
		m := tail(target, agent, int64(1000+i*60))
		m.Family = tree.FamilyV4
		m.Series = map[string][]int32{SeriesIPDVSend: {1, 2, 3}}
		v4 = append(v4, m)
	}
	if err := s.WriteMeasurements(ctx, v4); err != nil {
		t.Fatal(err)
	}
	const shared = 1000 + 19*60
	six := tail(target, agent, shared)
	six.Family = tree.FamilyV6
	for i := range six.Samples {
		six.Samples[i] = uint32(7 + i) // v4's are all 5000
	}
	six.Series = map[string][]int32{SeriesIPDVReceive: {9}}
	if err := s.WriteMeasurements(ctx, []Measurement{six}); err != nil {
		t.Fatal(err)
	}

	read := func(f tree.Family) Measurement {
		t.Helper()
		ms, err := s.QueryRange(ctx, target, agent, f, shared, shared+1)
		if err != nil || len(ms) != 1 {
			t.Fatalf("%v: %d rows, %v", f, len(ms), err)
		}
		return ms[0]
	}
	a, b := read(tree.FamilyV4), read(tree.FamilyV6)
	if a.Family != tree.FamilyV4 || b.Family != tree.FamilyV6 {
		t.Errorf("families read back as %v and %v", a.Family, b.Family)
	}
	if a.Positional == nil || b.Positional != nil {
		t.Errorf("positional flag: v4 %v, v6 %v; want v4 flagged by its own history and v6 not", a.Positional, b.Positional)
	}
	if _, ok := a.Series[SeriesIPDVSend]; !ok || a.Series[SeriesIPDVReceive] != nil {
		t.Errorf("v4 series = %v", a.Series)
	}
	if _, ok := b.Series[SeriesIPDVReceive]; !ok || b.Series[SeriesIPDVSend] != nil {
		t.Errorf("v6 series = %v", b.Series)
	}
	if slices.Equal(a.Samples, b.Samples) {
		t.Errorf("both families read %v", a.Samples)
	}
	if ms, err := s.QueryRange(ctx, target, agent, tree.FamilyUnknown, 0, 1<<40); err != nil || len(ms) != 0 {
		t.Errorf("unknown family read %d rows, %v", len(ms), err)
	}

	// The ingest path keeps the first write per family: v6 at a timestamp v4
	// already has is new, not a duplicate; v6 again is.
	ing := rowAt(5000, 5, []uint32{1}, nil)
	ing.Family = tree.FamilyV4
	ing6 := ing
	ing6.Family = tree.FamilyV6
	if st, err := s.IngestMeasurements(ctx, []Measurement{ing, ing6, ing6}); err != nil || len(st) != 2 {
		t.Errorf("ingest of v4, v6, v6 stored %d rows, %v; want 2", len(st), err)
	}

	// Outbox: both are pending, and confirming one leaves the other, with its
	// series, in place.
	pending, err := s.PendingMeasurements(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var sixPending *Measurement
	for i := range pending {
		if pending[i].TS == shared && pending[i].Family == tree.FamilyV6 {
			sixPending = &pending[i]
		}
	}
	if sixPending == nil || sixPending.Series[SeriesIPDVReceive] == nil || sixPending.Series[SeriesIPDVSend] != nil {
		t.Fatalf("pending v6 row at %d = %+v", shared, sixPending)
	}
	var fours int
	for _, m := range pending {
		if m.TS == shared && m.Family == tree.FamilyV4 {
			fours++
			if m.Series[SeriesIPDVSend] == nil || m.Series[SeriesIPDVReceive] != nil {
				t.Errorf("pending v4 row carries series %v", m.Series)
			}
		}
	}
	if fours != 1 {
		t.Errorf("%d pending v4 rows at %d, want 1", fours, shared)
	}
	if err := s.DropSubmitted(ctx, []Measurement{*sixPending}); err != nil {
		t.Fatal(err)
	}
	if a2 := read(tree.FamilyV4); a2.Series[SeriesIPDVSend] == nil || a2.Positional == nil {
		t.Errorf("dropping the v6 row damaged v4: %+v", a2)
	}
	if ms, _ := s.QueryRange(ctx, target, agent, tree.FamilyV6, shared, shared+1); len(ms) != 0 {
		t.Errorf("the dropped v6 row is still there: %+v", ms)
	}

	// Replacing v4 leaves v6 alone: write v6 back, then rewrite v4.
	if err := s.WriteMeasurements(ctx, []Measurement{six}); err != nil {
		t.Fatal(err)
	}
	re := tail(target, agent, shared)
	re.Family = tree.FamilyV4
	if err := s.WriteMeasurements(ctx, []Measurement{re}); err != nil {
		t.Fatal(err)
	}
	if b2 := read(tree.FamilyV6); b2.Series[SeriesIPDVReceive] == nil || len(b2.Samples) != len(six.Samples) {
		t.Errorf("replacing the v4 row damaged v6: %+v", b2)
	}
	if a2 := read(tree.FamilyV4); a2.Series != nil {
		t.Errorf("the rewritten v4 row kept its old series %v", a2.Series)
	}
}
