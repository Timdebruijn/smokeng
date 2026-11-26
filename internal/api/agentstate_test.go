package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/tree"
)

func TestAgentStateThresholds(t *testing.T) {
	const every, now = 75, 10_000
	for _, c := range []struct {
		lastSeen int64
		want     string
	}{
		{0, "never"},
		{now - 2*every, "reporting"},
		{now - 2*every - 1, "late"},
		{now - 10*every, "late"},
		{now - 10*every - 1, "silent"},
	} {
		if got := agentState(c.lastSeen, every, now); got != c.want {
			t.Errorf("last seen %ds ago: %q, want %q", now-c.lastSeen, got, c.want)
		}
	}
}

// The listing says how often an agent should be heard from (its shortest
// target interval plus the push cadence) and whether it has been, and says
// nothing for an agent whose silence means nothing: one that measures no
// target, one that is disabled, and the local prober.
func TestAgentsListingSaysWhichAgentsHaveGoneQuiet(t *testing.T) {
	h, st := newTestServer(t)
	ctx := context.Background()
	add := func(name string) int64 {
		pub := make([]byte, 32)
		pub[0] = byte(len(name))
		pub[1] = name[0]
		rec, err := st.AddAgent(ctx, name, pub)
		if err != nil {
			t.Fatal(err)
		}
		return rec.ID
	}
	now := time.Now().Unix()
	quiet, late, off := add("quiet"), add("late"), add("off")
	add("never")
	add("idle")
	if err := st.TouchAgent(ctx, quiet, now-30, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchAgent(ctx, late, now-200, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchAgent(ctx, off, now-1_000_000, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAgentEnabled(ctx, off, false); err != nil {
		t.Fatal(err)
	}
	root := int64(1)
	// A group that measures nothing itself, with a short interval and agents its
	// children inherit: "inherit" and the local prober through b, which sets its
	// own interval, and "deep" through c, which inherits the group's.
	g := tree.Target{ParentID: &root, Name: "g", Enabled: true,
		Settings: tree.Settings{IntervalS: new(30), Agents: new("inherit local")}}
	if err := st.UpsertTarget(ctx, &g); err != nil {
		t.Fatal(err)
	}
	add("inherit")
	add("deep")
	add("portless")
	for _, n := range []tree.Target{
		{ParentID: &g.ID, Name: "b", Host: new("192.0.2.4"), AddressFamily: new("v4"), Enabled: true,
			Settings: tree.Settings{IntervalS: new(120)}},
		{ParentID: &g.ID, Name: "c", Host: new("192.0.2.5"), AddressFamily: new("v4"), Enabled: true,
			Settings: tree.Settings{Agents: new("deep")}},
		// tcp without a port is skipped by the prober, so it is nothing to wait for.
		{ParentID: &root, Name: "d", Host: new("192.0.2.6"), AddressFamily: new("v4"), Enabled: true,
			Settings: tree.Settings{IntervalS: new(10), ProbeType: new("tcp"), Agents: new("portless")}},
		// 60 s here and 300 s on the other: the shorter one sets the cadence.
		{ParentID: &root, Name: "a", Host: new("192.0.2.1"), AddressFamily: new("v4"), Enabled: true,
			Settings: tree.Settings{IntervalS: new(60), Agents: new("quiet late never off")}},
		{ParentID: &root, Name: "b", Host: new("192.0.2.2"), AddressFamily: new("v4"), Enabled: true,
			Settings: tree.Settings{IntervalS: new(300), Agents: new("quiet")}},
		// A disabled target does not make "idle" an agent anyone waits for.
		{ParentID: &root, Name: "c", Host: new("192.0.2.3"), AddressFamily: new("v4"), Enabled: false,
			Settings: tree.Settings{Agents: new("idle")}},
	} {
		if err := st.UpsertTarget(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}

	code, body := do(t, h, "GET", "/api/v1/agents", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/agents = %d", code)
	}
	got := map[string]map[string]any{}
	for _, a := range body["agents"].([]any) {
		m := a.(map[string]any)
		got[m["name"].(string)] = m
	}
	for name, want := range map[string]string{"quiet": "reporting", "late": "late", "never": "never"} {
		if got[name]["state"] != want || got[name]["report_every_s"] != float64(75) {
			t.Errorf("%s: state %v every %v, want %s every 75", name, got[name]["state"], got[name]["report_every_s"], want)
		}
	}
	// Inherited agents and intervals count; the group itself does not.
	for name, want := range map[string]float64{"inherit": 135, "deep": 45} {
		if got[name]["report_every_s"] != want {
			t.Errorf("%s: every %v, want %v", name, got[name]["report_every_s"], want)
		}
	}
	for _, name := range []string{"idle", "off", "local", "portless"} {
		if _, ok := got[name]["state"]; ok {
			t.Errorf("%s has a state, %v; its silence means nothing", name, got[name]["state"])
		}
	}
}

// A scoped caller's cadence counts only the targets they can see: an agent
// that measures a ten-second target in another customer's subtree must not
// show them a cadence that gives that target away.
func TestAgentCadenceCountsOnlyVisibleTargets(t *testing.T) {
	f := tenants(t)
	ctx := context.Background()
	targets, err := f.st.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range targets {
		switch targets[i].ID {
		case f.hostA:
			targets[i].Settings.Agents, targets[i].Settings.IntervalS = new("ams-b"), new(300)
		case f.hostB:
			targets[i].Settings.IntervalS = new(10)
		default:
			continue
		}
		if err := f.st.UpsertTarget(ctx, &targets[i]); err != nil {
			t.Fatal(err)
		}
	}
	h := f.server(t, "team-a", f.groupA, "viewer")
	code, body := call(t, h, "GET", "/api/v1/agents", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/agents = %d %s", code, body)
	}
	if !strings.Contains(body, `"report_every_s":315`) || strings.Contains(body, `"report_every_s":25`) {
		t.Errorf("scoped cadence leaks or is missing: %s", body)
	}
}
