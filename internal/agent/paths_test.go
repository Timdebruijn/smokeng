package agent

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timdebruijn/smokeng/internal/api"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// An agent's path changes reach the master under its own id, through the real
// route, with the measurement batch's rules: only targets assigned to it, a bad
// row dropped and the rest kept, the first write wins. Its buffer then holds
// nothing to send but still knows the last path, so the next traceroute that
// finds the same route is not a change.
func TestAgentPathChangesReachTheMasterUnderItsID(t *testing.T) {
	ctx := context.Background()
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
	root := int64(1)
	mk := func(name, agents string) int64 {
		n := tree.Target{ParentID: &root, Name: name, Host: new("192.0.2.9"), AddressFamily: new("v4"), Enabled: true,
			Settings: tree.Settings{Agents: new(agents)}}
		if err := master.UpsertTarget(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	mine, other := mk("mine", "ams-01"), mk("other", "local")

	srv := httptest.NewServer(api.New(master, api.Options{}, fstest.MapFS{}))
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

	// What the prober records on an agent: the local id, as on a master.
	now := time.Now().Unix()
	for _, p := range []struct {
		target, ts int64
		hops       string
	}{
		{mine, now - 600, "10.0.0.1,*,192.0.2.9"},
		{mine, now - 300, "10.0.0.1,10.9.9.9,192.0.2.9"},
		{other, now - 300, "10.0.0.1,192.0.2.9"}, // not assigned to this agent
		{mine, now - 200, "10.0.0.1,not-an-address"},
		{mine, now + 3600, "10.0.0.1,192.0.2.9"}, // from the future
	} {
		if err := buf.RecordPath(ctx, p.target, store.LocalAgentID, p.ts, p.hops); err != nil {
			t.Fatal(err)
		}
	}
	a.push(ctx)

	got, err := master.PathChanges(ctx, mine, rec.ID, 0, now+7200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Hops != "10.0.0.1,*,192.0.2.9" || got[1].Hops != "10.0.0.1,10.9.9.9,192.0.2.9" {
		t.Errorf("master holds %+v for the agent's own target, want the two valid changes", got)
	}
	if got, _ := master.PathChanges(ctx, other, rec.ID, 0, now+7200); len(got) != 0 {
		t.Errorf("master stored %+v for a target not assigned to the agent", got)
	}
	if local, _ := master.PathChanges(ctx, mine, store.LocalAgentID, 0, now+7200); len(local) != 0 {
		t.Errorf("the agent's changes landed under the master's own prober: %+v", local)
	}

	if pending, _ := buf.PendingPaths(ctx, 100); len(pending) != 0 {
		t.Errorf("still pending after the master confirmed: %+v", pending)
	}
	// The last path survives, so the next run compares against it.
	if last, _ := buf.LastPath(ctx, mine, store.LocalAgentID); last != "10.0.0.1,192.0.2.9" {
		t.Errorf("LastPath = %q, want the latest recorded (the future one is the latest here)", last)
	}

	// First write wins: a replay changes nothing.
	dup, err := master.IngestPaths(ctx, rec.ID, []store.PathChange{{TargetID: mine, TS: now - 600, Hops: "1.2.3.4"}})
	if err != nil || dup != 1 {
		t.Errorf("a replayed change: %d duplicates, %v; want 1", dup, err)
	}
	if got, _ := master.PathChanges(ctx, mine, rec.ID, 0, now+7200); got[0].Hops != "10.0.0.1,*,192.0.2.9" {
		t.Errorf("a replay rewrote a reported change: %+v", got[0])
	}
}

// A master without the route (older, or downgraded after asking for paths)
// answers 404. The changes stay buffered for when it is upgraded, rather than
// being thrown away.
func TestPathChangesStayBufferedWhenTheMasterHasNoRoute(t *testing.T) {
	a, st, _ := newTestAgent(t, http.NotFoundHandler())
	ctx := context.Background()
	if err := st.RecordPath(ctx, 42, store.LocalAgentID, 1000, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	a.pushPaths(ctx)
	if pending, _ := st.PendingPaths(ctx, 10); len(pending) != 1 {
		t.Errorf("pending after a 404: %+v, want the change still there", pending)
	}
}
