package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// v24 on a buffer from before agents sent paths: the history an agent traced at
// its own default is not shipped, but the latest path per target is, or it
// would hold back the current route — the next traceroute compares with it and
// finds no change.
func TestV24SendsOnlyTheLatestPathPerTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 23 { // everything up to v23
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO paths (target_id, agent_id, ts, hops) VALUES (5, 0, 1000, 'a'), (5, 0, 2000, 'b'), (6, 0, 1500, 'c')`,
		`PRAGMA user_version = 23`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v23 database: %v", err)
	}
	defer s.Close()
	got, err := s.PendingPaths(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].TargetID != 6 || got[1].TargetID != 5 || got[1].Hops != "b" {
		t.Errorf("pending after v24 = %+v, want the latest of each target: 6 at 1500, 5 at 2000", got)
	}
}
