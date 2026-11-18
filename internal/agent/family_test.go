package agent

import (
	"crypto/ed25519"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/api"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Protects the agent saying which family it measured over, end to end through
// the real routes. The master's target is v4 now; the agent's buffer holds a
// backlog from when it was v6, plus a row and a change of unknown family. The v6
// ones must land under v6, and the unknown ones under the target's current v4;
// if the agent stopped sending the family, everything would be filed under v4.
func TestAgentBacklogReachesTheMasterUnderItsFamily(t *testing.T) {
	ctx := t.Context()
	master, err := store.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	key, _, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "probe.key"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := master.AddAgent(ctx, "ams-01", key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	target := tree.Target{ParentID: new(int64(1)), Name: "web", Host: new("192.0.2.9"), AddressFamily: new("v4"),
		Enabled: true, Settings: tree.Settings{Agents: new("ams-01")}}
	if err := master.UpsertTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(master, api.Options{Alerts: alert.NewManager(master, nil)}, fstest.MapFS{}))
	t.Cleanup(srv.Close)
	buf, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { buf.Close() })
	a, err := New(Config{Master: srv.URL, AgentID: rec.ID, Insecure: true}, key, buf)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix() / 60 * 60
	row := func(ts int64, fam tree.Family) store.Measurement {
		return store.Measurement{TargetID: target.ID, AgentID: store.LocalAgentID, Family: fam, TS: ts,
			Sent: 3, Received: 3, Samples: []uint32{1000, 1200, 1500}}
	}
	if err := buf.WriteMeasurements(ctx, []store.Measurement{
		row(now-300, tree.FamilyV6), row(now-240, tree.FamilyUnknown), row(now-180, tree.FamilyV4),
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		ts   int64
		fam  tree.Family
		hops string
	}{
		{now - 300, tree.FamilyV6, "2001:db8::1,2001:db8::9"},
		{now - 240, tree.FamilyUnknown, "10.0.0.1,192.0.2.9"},
	} {
		if err := buf.RecordPath(ctx, target.ID, store.LocalAgentID, p.fam, p.ts, p.hops); err != nil {
			t.Fatal(err)
		}
	}
	a.push(ctx)

	count := func(fam tree.Family) (n int) {
		got, err := master.QueryRange(ctx, target.ID, rec.ID, fam, 0, now+60)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if v6, v4 := count(tree.FamilyV6), count(tree.FamilyV4); v6 != 1 || v4 != 2 {
		t.Errorf("master holds %d rows under v6 and %d under v4, want 1 and 2", v6, v4)
	}
	paths := func(fam tree.Family) []store.PathChange {
		got, err := master.PathChanges(ctx, target.ID, rec.ID, fam, 0, now+60)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := paths(tree.FamilyV6); len(got) != 1 || got[0].Hops != "2001:db8::1,2001:db8::9" {
		t.Errorf("v6 paths = %+v, want the v6 change only", got)
	}
	if got := paths(tree.FamilyV4); len(got) != 1 || got[0].Hops != "10.0.0.1,192.0.2.9" {
		t.Errorf("v4 paths = %+v, want the change of unknown family, filed under the current one", got)
	}
}
