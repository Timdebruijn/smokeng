package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A database from before v27 has a root and targets under it. Opening it must
// set the root to exclude and nothing else, so existing targets resolve to
// exclude by inheritance and none gains a value of its own.
func TestV27SetsOnlyTheRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v26.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 26 {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO targets (id, parent_id, name, host, address_family, pings_per_interval)
		VALUES (50, 1, 'grp', NULL, NULL, NULL), (51, 50, 'leaf', '10.0.0.1', 'v4', 7)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 26`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v26 database: %v", err)
	}
	defer s.Close()
	ts, err := s.ListTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 3 {
		t.Fatalf("%d targets, want 3", len(ts))
	}
	for _, n := range ts {
		got := n.Settings.PositionalLoss
		if n.ParentID == nil {
			if got == nil || *got != "exclude" {
				t.Errorf("root positional_loss = %v, want exclude", got)
			}
			continue
		}
		if got != nil {
			t.Errorf("%s gained positional_loss %q", n.Name, *got)
		}
		if n.Name == "leaf" && (n.Settings.PingsPerInterval == nil || *n.Settings.PingsPerInterval != 7) {
			t.Errorf("an existing target's setting changed: %v", n.Settings.PingsPerInterval)
		}
	}
}
