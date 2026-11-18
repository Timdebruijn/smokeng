package store

import (
	"database/sql"
	"maps"
	"path/filepath"
	"strings"
	"testing"
)

// Protects an installation upgrading to v30 against the way the column is
// widened: targets is referenced by cascading foreign keys (grants, silences),
// so a rebuild of it would delete them, and the alert rules that point at it
// would be left dangling. Every column of every target keeps every byte, the
// foreign keys still hold, a target can now be stored as "both", and the
// column still refuses any other value.
func TestV30WidensAddressFamilyWithoutLosingTargetsOrWhatPointsAtThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v29.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := range 29 {
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
	const v4Target, v6Target, group = 10, 11, 15
	exec(`INSERT INTO targets (id, parent_id, name, host, address_family, title, notes, hidden, interval_s, pings_per_interval, probe_type, probe_port, positional_loss)
		VALUES (?, 1, 'a', 'a.example.net', 'v4', 'A title', 'notes', 1, 30, 7, 'tcp', 443, 'flag_only')`, v4Target)
	exec(`INSERT INTO targets (id, parent_id, name, host, address_family, enabled, sort_order, dscp, tls_skip_verify) VALUES (?, 1, 'b', '2001:db8::1', 'v6', 0, 4, 46, 1)`, v6Target)
	exec(`INSERT INTO targets (id, parent_id, name) VALUES (?, 1, 'g')`, group)
	exec(`INSERT INTO grants (group_name, target_id, role) VALUES ('ops', ?, 'editor'), ('ops', ?, 'viewer'), ('dev', ?, 'viewer')`, v4Target, v6Target, group)
	exec(`INSERT INTO silences (target_id, starts_at, ends_at, reason, created_at) VALUES (?, 1, 9000, 'one target', 2), (?, 1, 9000, 'the group', 3)`, v4Target, group)
	exec(`INSERT INTO alert_rules (id, target_id, name, metric, op, threshold) VALUES (1, ?, 'r1', 'loss', '>', 10), (2, ?, 'r2', 'loss', '>', 10)`, v6Target, group)
	exec(`PRAGMA user_version = 29`)

	// Every column by name, so the dump does not depend on where it sits.
	cols := strings.Fields(strings.ReplaceAll(targetCols, ",", " "))
	rest := make([]string, len(cols))
	for i, c := range cols {
		rest[i] = "quote(" + c + ")"
	}
	dump := func(db *sql.DB) map[string]string {
		return dumpTable(t, db, "targets", "id", strings.Join(rest, " || '|' || "))
	}
	counts := func(db *sql.DB) map[string]int {
		out := map[string]int{}
		for _, tb := range []string{"grants", "silences", "alert_rules", "targets"} {
			var n int
			if err := db.QueryRow("SELECT count(*) FROM " + tb).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[tb] = n
		}
		return out
	}
	before, countsBefore := dump(db), counts(db)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v29 database: %v", err)
	}
	defer s.Close()

	if after := dump(s.db); !maps.Equal(before, after) {
		t.Errorf("target rows changed by v30:\nbefore %v\nafter  %v", before, after)
	}
	if got := counts(s.db); !maps.Equal(got, countsBefore) {
		t.Errorf("row counts after v30 = %v, want %v: a cascade took rows with it", got, countsBefore)
	}
	rows, err := s.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("foreign_key_check reports a violation after v30")
	}
	rows.Close()

	if _, err := s.db.Exec(`UPDATE targets SET address_family = 'both' WHERE id = ?`, v4Target); err != nil {
		t.Errorf("a target cannot be stored as both: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE targets SET address_family = 'v5' WHERE id = ?`, v4Target); err == nil {
		t.Error("the column accepts a value that is not v4, v6 or both")
	}
}
