// This is an external test package so it can use the real store: store
// depends on alert for its rule types, so an in-package test importing store
// would be a cycle.
package alert_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

func ptr[T any](v T) *T { return &v }

// capture is a webhook that records what it was sent.
type capture struct {
	mu     sync.Mutex
	bodies [][]map[string]any
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed []map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.bodies = append(c.bodies, parsed)
		c.mu.Unlock()
	}
}

func (c *capture) all() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, b := range c.bodies {
		out = append(out, b...)
	}
	return out
}

// setup builds a store with one leaf target under a group, and returns the
// leaf's id so rules can be hung on either node.
func setup(t *testing.T) (*store.SQLite, int64, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "alert.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	group := tree.Target{ParentID: ptr(int64(1)), Name: "Production", Enabled: true,
		Settings: tree.Settings{IntervalS: ptr(60)}}
	if err := st.UpsertTarget(ctx, &group); err != nil {
		t.Fatal(err)
	}
	leaf := tree.Target{ParentID: &group.ID, Name: "gw", Enabled: true,
		Host: ptr("10.0.0.1"), AddressFamily: ptr("v4")}
	if err := st.UpsertTarget(ctx, &leaf); err != nil {
		t.Fatal(err)
	}
	return st, group.ID, leaf.ID
}

func input(targetID int64, step, sent, received int) alert.Input {
	samples := make([]uint32, received)
	for i := range samples {
		samples[i] = 5000
	}
	return alert.Input{
		TargetID: targetID, AgentID: store.LocalAgentID,
		TS: int64(1_756_400_000 + step*60), Sent: sent, Received: received,
		Samples: samples, LossTrusted: true, RTTTrusted: true,
	}
}

// The whole path: a rule defined on an ancestor applies to the leaf, fires
// only after the hysteresis window, and reaches the webhook in a shape
// Alertmanager accepts.
func TestFiresThroughToWebhook(t *testing.T) {
	ctx := context.Background()
	st, groupID, leafID := setup(t)

	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	// Defined on the group, so it must reach the leaf by inheritance.
	rule := alert.Rule{
		TargetID: groupID, Name: "packet loss", Metric: alert.MetricLoss,
		Op: alert.OpGreater, Threshold: 20, For: 2, ClearFor: 2, Enabled: true,
	}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}

	m := alert.NewManager(st, &alert.Webhook{URL: srv.URL})
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	m.Observe(ctx, []alert.Input{input(leafID, 0, 10, 5)})
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("notified after one bad interval: %v", got)
	}
	m.Observe(ctx, []alert.Input{input(leafID, 1, 10, 5)})

	sent := cap.all()
	if len(sent) != 1 {
		t.Fatalf("got %d notifications, want 1 after the window closed", len(sent))
	}
	labels, _ := sent[0]["labels"].(map[string]any)
	if labels["alertname"] != "packet loss" || labels["target"] != "/Production/gw" {
		t.Errorf("labels = %v", labels)
	}
	if _, hasEnd := sent[0]["endsAt"]; hasEnd {
		t.Error("a firing alert carries an endsAt, so Alertmanager would expire it early")
	}
	if sent[0]["startsAt"] == nil {
		t.Error("no startsAt on a firing alert")
	}

	if firing := m.Firing(); len(firing) != 1 || firing[0].TargetPath != "/Production/gw" {
		t.Errorf("Firing() = %+v", firing)
	}

	// Recovery: two good intervals, then a resolved notification with an end.
	m.Observe(ctx, []alert.Input{input(leafID, 2, 10, 10)})
	m.Observe(ctx, []alert.Input{input(leafID, 3, 10, 10)})
	all := cap.all()
	if len(all) != 2 {
		t.Fatalf("got %d notifications, want 2 (fire and resolve)", len(all))
	}
	if all[1]["endsAt"] == nil {
		t.Error("a resolved alert must carry endsAt")
	}
	if len(m.Firing()) != 0 {
		t.Error("still firing after resolve")
	}
}

// Hysteresis is only worth anything if it survives a restart: an alert firing
// for an hour must not resolve and re-fire because smokeng was restarted.
func TestStateSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	st, _, leafID := setup(t)
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	rule := alert.Rule{
		TargetID: leafID, Name: "loss", Metric: alert.MetricLoss,
		Op: alert.OpGreater, Threshold: 20, For: 2, ClearFor: 2, Enabled: true,
	}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}

	m1 := alert.NewManager(st, &alert.Webhook{URL: srv.URL})
	if err := m1.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	m1.Observe(ctx, []alert.Input{input(leafID, 0, 10, 5)})
	m1.Observe(ctx, []alert.Input{input(leafID, 1, 10, 5)})
	if len(cap.all()) != 1 {
		t.Fatalf("expected one firing notification, got %d", len(cap.all()))
	}

	// A fresh manager over the same store: still firing, and it does not
	// announce the alert a second time.
	m2 := alert.NewManager(st, &alert.Webhook{URL: srv.URL})
	if err := m2.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if firing := m2.Firing(); len(firing) != 1 {
		t.Fatalf("firing after restart = %d, want 1", len(firing))
	}
	m2.Observe(ctx, []alert.Input{input(leafID, 2, 10, 5)})
	if n := len(cap.all()); n != 1 {
		t.Errorf("re-announced on restart: %d notifications, want 1", n)
	}

	// Repeat exists so Alertmanager does not expire a long-running alert.
	m2.Repeat(ctx)
	if n := len(cap.all()); n != 2 {
		t.Errorf("Repeat sent %d notifications in total, want 2", n)
	}
}

// A silence suppresses delivery and attention, but not the fact: the alert still
// fires, the transition is still logged, and lifting the silence lets it deliver.
func TestSilenceSuppressesDeliveryNotTheFact(t *testing.T) {
	ctx := context.Background()
	st, groupID, leafID := setup(t)

	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	rule := alert.Rule{TargetID: groupID, Name: "packet loss", Metric: alert.MetricLoss,
		Op: alert.OpGreater, Threshold: 20, For: 2, ClearFor: 2, Enabled: true}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, &alert.Webhook{URL: srv.URL})
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	// Silence the group; the leaf is under it, and the window is always open.
	// StartsAt/EndsAt are wall-clock, independent of the backdated inputs.
	if _, err := m.AddSilence(ctx, alert.Silence{
		TargetID: ptr(groupID), StartsAt: 1, EndsAt: 4_000_000_000, Reason: "maintenance",
	}); err != nil {
		t.Fatal(err)
	}

	m.Observe(ctx, []alert.Input{input(leafID, 0, 10, 5)})
	m.Observe(ctx, []alert.Input{input(leafID, 1, 10, 5)})

	if got := cap.all(); len(got) != 0 {
		t.Fatalf("silence did not suppress delivery: %v", got)
	}
	firing := m.Firing()
	if len(firing) != 1 || !firing[0].Silenced {
		t.Fatalf("Firing() = %+v, want one silenced alert", firing)
	}
	events, err := st.ListAlertEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].Firing {
		t.Fatalf("transition log = %+v, want the fire recorded despite the silence", events)
	}

	// Lift it: what is still firing now delivers.
	sils, _ := m.ListSilences(ctx)
	if len(sils) != 1 {
		t.Fatalf("ListSilences = %+v", sils)
	}
	if ok, err := m.RemoveSilence(ctx, sils[0].ID); err != nil || !ok {
		t.Fatalf("RemoveSilence = %v, %v", ok, err)
	}
	m.Repeat(ctx)
	if got := cap.all(); len(got) != 1 {
		t.Fatalf("after lifting the silence, got %d deliveries, want 1", len(got))
	}
}

// A shape rule fires when the distribution shifts away from its rolling
// baseline, and stays quiet while the series is stable — the whole point being
// to catch a change no single percentile threshold would.
func TestShapeRuleFiresOnShift(t *testing.T) {
	ctx := context.Background()
	st, groupID, leafID := setup(t)

	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	rule := alert.Rule{
		TargetID: groupID, Name: "shape shift", Metric: alert.MetricShape,
		Op: alert.OpGreater, Threshold: 10, For: 2, ClearFor: 2, Enabled: true,
		Mode: alert.ModeTunable, Baseline: alert.BaselineRolling,
	}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, &alert.Webhook{URL: srv.URL})
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	mk := func(step, rttUs int) alert.Input {
		s := make([]uint32, 20)
		for i := range s {
			s[i] = uint32(rttUs)
		}
		return alert.Input{
			TargetID: leafID, AgentID: store.LocalAgentID,
			TS: int64(1_756_400_000 + step*60), Sent: 20, Received: 20,
			Samples: s, LossTrusted: true, RTTTrusted: true,
		}
	}

	// Warm up and then hold steady: no shift, nothing fires.
	step := 0
	for ; step < 14; step++ {
		m.Observe(ctx, []alert.Input{mk(step, 5000)})
	}
	if n := len(cap.all()); n != 0 {
		t.Fatalf("shape rule fired on a stable series: %d notifications", n)
	}

	// Shift the distribution up. After the hysteresis window it fires.
	for ; step < 18; step++ {
		m.Observe(ctx, []alert.Input{mk(step, 60000)})
	}
	if len(cap.all()) == 0 {
		t.Fatal("shape rule did not fire on a distribution shift")
	}
	if f := m.Firing(); len(f) != 1 || f[0].Rule.Metric != alert.MetricShape {
		t.Fatalf("Firing() = %+v, want one firing shape rule", f)
	}
}

// A silence must not overreach: one scoped to an unrelated target, and one whose
// window has passed, both leave the leaf's alert to deliver normally.
func TestSilenceScopeAndWindowDoNotOverreach(t *testing.T) {
	ctx := context.Background()
	st, groupID, leafID := setup(t)

	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	rule := alert.Rule{TargetID: groupID, Name: "packet loss", Metric: alert.MetricLoss,
		Op: alert.OpGreater, Threshold: 20, For: 2, ClearFor: 2, Enabled: true}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, &alert.Webhook{URL: srv.URL})
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	// A real but unrelated target (a sibling of the group under the root), so a
	// silence on it does not cover this leaf.
	other := tree.Target{ParentID: ptr(int64(1)), Name: "other", Enabled: true,
		Host: ptr("10.0.0.9"), AddressFamily: ptr("v4")}
	if err := st.UpsertTarget(ctx, &other); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	// One silence for the unrelated target, one for the right target but a window
	// that closed in 1970. Neither should cover this leaf now.
	if _, err := m.AddSilence(ctx, alert.Silence{TargetID: ptr(other.ID), StartsAt: 1, EndsAt: 4_000_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddSilence(ctx, alert.Silence{TargetID: ptr(groupID), StartsAt: 1, EndsAt: 2}); err != nil {
		t.Fatal(err)
	}

	m.Observe(ctx, []alert.Input{input(leafID, 0, 10, 5)})
	m.Observe(ctx, []alert.Input{input(leafID, 1, 10, 5)})

	if got := cap.all(); len(got) != 1 {
		t.Fatalf("an out-of-scope or expired silence suppressed delivery: got %d, want 1", len(got))
	}
	if f := m.Firing(); len(f) != 1 || f[0].Silenced {
		t.Fatalf("Firing() = %+v, want one alert that is not silenced", f)
	}
}

// Rules replace rather than accumulate down the tree, consistently with every
// other inheritable setting: a node that defines rules defines the whole set.
func TestChildRulesReplaceInherited(t *testing.T) {
	ctx := context.Background()
	st, groupID, leafID := setup(t)
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	// The group would fire on any loss at all; the leaf only above 50%.
	groupRule := alert.Rule{
		TargetID: groupID, Name: "any loss", Metric: alert.MetricLoss,
		Op: alert.OpGreater, Threshold: 0, For: 1, ClearFor: 1, Enabled: true,
	}
	leafRule := alert.Rule{
		TargetID: leafID, Name: "heavy loss", Metric: alert.MetricLoss,
		Op: alert.OpGreater, Threshold: 50, For: 1, ClearFor: 1, Enabled: true,
	}
	for _, r := range []*alert.Rule{&groupRule, &leafRule} {
		if err := st.UpsertAlertRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	m := alert.NewManager(st, &alert.Webhook{URL: srv.URL})
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	// 20% loss: the group's rule would fire, the leaf's must not — and the
	// leaf's is the only one that applies.
	m.Observe(ctx, []alert.Input{input(leafID, 0, 10, 8)})
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("an overridden ancestor rule still fired: %v", got)
	}
	m.Observe(ctx, []alert.Input{input(leafID, 1, 10, 2)})
	got := cap.all()
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1", len(got))
	}
	if labels, _ := got[0]["labels"].(map[string]any); labels["alertname"] != "heavy loss" {
		t.Errorf("fired rule = %v, want the leaf's own", labels)
	}
}

// Acknowledging a firing alert mutes it without resolving it, ties to the
// current episode, and is dropped when that episode ends — so a fresh fire of
// the same rule demands attention again rather than inheriting the old ack.
func TestAcknowledgeTiesToEpisode(t *testing.T) {
	ctx := context.Background()
	st, groupID, leafID := setup(t)

	rule := alert.Rule{
		TargetID: groupID, Name: "loss", Metric: alert.MetricLoss,
		Op: alert.OpGreater, Threshold: 20, For: 1, ClearFor: 1, Enabled: true,
	}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, nil)
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	// Fire.
	m.Observe(ctx, []alert.Input{input(leafID, 0, 10, 0)})
	firing := m.Firing()
	if len(firing) != 1 || firing[0].Acked {
		t.Fatalf("want one firing, unacked alert, got %+v", firing)
	}

	// Acknowledge it.
	changed, err := m.Acknowledge(ctx, rule.ID, leafID, store.LocalAgentID, true, "tim@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Acknowledge reported no firing alert to change")
	}
	firing = m.Firing()
	if len(firing) != 1 || !firing[0].Acked || firing[0].AckedBy != "tim@example.org" {
		t.Fatalf("alert should be firing and acknowledged by tim, got %+v", firing)
	}

	// It survives a restart, because both the firing state and the ack are
	// persisted and the ack still matches its episode.
	m2 := alert.NewManager(st, nil)
	if err := m2.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if f := m2.Firing(); len(f) != 1 || !f[0].Acked {
		t.Fatalf("ack did not survive a restart: %+v", f)
	}

	// Resolve, then re-fire: the new episode is not acknowledged.
	m2.Observe(ctx, []alert.Input{input(leafID, 1, 10, 10)}) // healthy → resolves
	if f := m2.Firing(); len(f) != 0 {
		t.Fatalf("alert should have resolved, got %+v", f)
	}
	m2.Observe(ctx, []alert.Input{input(leafID, 2, 10, 0)}) // lossy again → re-fires
	f := m2.Firing()
	if len(f) != 1 {
		t.Fatalf("alert should have re-fired, got %+v", f)
	}
	if f[0].Acked {
		t.Fatal("the re-fired episode inherited the old acknowledgement; each episode must be " +
			"acknowledged on its own")
	}
}

// Unacknowledging clears the mark, and acknowledging something not firing is a
// no-op the caller can distinguish.
func TestAcknowledgeUnackAndMiss(t *testing.T) {
	ctx := context.Background()
	st, groupID, leafID := setup(t)
	rule := alert.Rule{
		TargetID: groupID, Name: "loss", Metric: alert.MetricLoss,
		Op: alert.OpGreater, Threshold: 20, For: 1, ClearFor: 1, Enabled: true,
	}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, nil)
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	// Nothing firing yet: an ack changes nothing and says so.
	if changed, err := m.Acknowledge(ctx, rule.ID, leafID, store.LocalAgentID, true, "x"); err != nil || changed {
		t.Fatalf("ack of a non-firing alert should be a no-op, got changed=%v err=%v", changed, err)
	}

	m.Observe(ctx, []alert.Input{input(leafID, 0, 10, 0)})
	if _, err := m.Acknowledge(ctx, rule.ID, leafID, store.LocalAgentID, true, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acknowledge(ctx, rule.ID, leafID, store.LocalAgentID, false, ""); err != nil {
		t.Fatal(err)
	}
	if f := m.Firing(); len(f) != 1 || f[0].Acked {
		t.Fatalf("unack should have cleared the mark, got %+v", f)
	}
}
