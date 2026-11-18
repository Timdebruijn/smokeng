package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
)

// A replay runs next to a live server. Reads must neither block on the
// server's open write transaction nor see half of it, and must see it once it
// commits.
func TestOpenReadOnlyReadsBesideAnOpenWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ro.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO measurements (target_id, agent_id, family, ts, sent, received, flags, samples)
		VALUES (1, 0, 0, 100, 5, 1, 0, X'01A09C01')`); err != nil {
		t.Fatal(err)
	}

	read := make(chan int, 1)
	go func() {
		ms, err := ro.QueryRange(ctx, 1, 0, 0, 0, 1000)
		if err != nil {
			t.Error(err)
		}
		read <- len(ms)
	}()
	select {
	case n := <-read:
		if n != 0 {
			t.Errorf("read %d rows of an uncommitted write", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a read blocked on the writer's open transaction")
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ms, err := ro.QueryRange(ctx, 1, 0, 0, 0, 1000)
	if err != nil || len(ms) != 1 {
		t.Fatalf("after commit: %d rows, err %v; want 1", len(ms), err)
	}
}

// The handle must not be able to change the database, whatever the caller
// does: that is the guarantee a command next to a server relies on.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	r := alert.Rule{TargetID: 1, Name: "x", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 1, For: 1, ClearFor: 1, Enabled: true, Severity: alert.SeverityInfo}
	if err := ro.UpsertAlertRule(context.Background(), &r); err == nil {
		t.Fatal("a write through the read-only handle succeeded")
	}
}

// A database from another version must be refused, not migrated: upgrading the
// server's schema from a read-only tool would strand the server.
func TestOpenReadOnlyRefusesOtherSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	ro, err := OpenReadOnly(path)
	if err == nil {
		ro.Close()
		t.Fatal("opened a database at another schema version")
	}
	if !strings.Contains(err.Error(), "v3") || !strings.Contains(err.Error(), "same smokeng version") {
		t.Errorf("error does not say what to do: %v", err)
	}
}

// A mistyped path must not turn into an empty database, which would replay
// as "nothing ever fired".
func TestOpenReadOnlyMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	if ro, err := OpenReadOnly(path); err == nil {
		ro.Close()
		t.Fatal("opened a missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file was created: %v", err)
	}
}
