package alert_test

import (
	"context"
	"maps"
	"net/http/httptest"
	"testing"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// inputOf is input() for a series of the given family. No target can have two
// families through configuration yet, so the tests feed them directly.
func inputOf(targetID int64, fam tree.Family, step, sent, received int) alert.Input {
	in := input(targetID, step, sent, received)
	in.Family = fam
	return in
}

// familyManager is a manager with a loss rule (fires after two bad intervals)
// on the leaf's group, plus the leaf's id and the rule.
func familyManager(t *testing.T, notifier alert.Notifier) (*store.SQLite, *alert.Manager, int64, alert.Rule) {
	t.Helper()
	ctx := context.Background()
	st, groupID, leafID := setup(t)
	rule := alert.Rule{
		TargetID: groupID, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 2, ClearFor: 2, Enabled: true, Severity: alert.SeverityWarning,
	}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, notifier)
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	return st, m, leafID, rule
}

// firingBy lists what is firing, by family, with whether it is acked and silenced.
func firingBy(m *alert.Manager) map[tree.Family]alert.Alert {
	out := map[tree.Family]alert.Alert{}
	for _, a := range m.Firing() {
		out[a.Family] = a
	}
	return out
}

// Protects the reason the family is in the state's key: the same rule, target
// and agent measured over two families at the same timestamps are two series.
// Keyed without the family the v6 intervals would advance (and clear) the v4
// state, and an alert on a broken v4 path would be resolved by a healthy v6 one.
func TestTwoFamiliesOfOneTargetFireAndResolveApart(t *testing.T) {
	ctx := context.Background()
	_, m, leaf, _ := familyManager(t, nil)

	for step := range 2 {
		m.Observe(ctx, []alert.Input{
			inputOf(leaf, tree.FamilyV4, step, 10, 0),  // all lost
			inputOf(leaf, tree.FamilyV6, step, 10, 10), // fine
		})
	}
	got := firingBy(m)
	if _, ok := got[tree.FamilyV4]; !ok || len(got) != 1 {
		t.Fatalf("want only v4 firing while v6 is clear, got %d alerts", len(got))
	}

	// v6 breaks as well: now both fire, as two alerts.
	for step := 2; step < 4; step++ {
		m.Observe(ctx, []alert.Input{
			inputOf(leaf, tree.FamilyV4, step, 10, 0),
			inputOf(leaf, tree.FamilyV6, step, 10, 0),
		})
	}
	if got := firingBy(m); len(got) != 2 {
		t.Fatalf("want v4 and v6 firing, got %d alerts", len(got))
	}

	// v4 recovers, v6 stays broken: only v4 resolves.
	for step := 4; step < 6; step++ {
		m.Observe(ctx, []alert.Input{
			inputOf(leaf, tree.FamilyV4, step, 10, 10),
			inputOf(leaf, tree.FamilyV6, step, 10, 0),
		})
	}
	got = firingBy(m)
	if _, ok := got[tree.FamilyV6]; !ok || len(got) != 1 {
		t.Fatalf("want only v6 still firing, got %d alerts", len(got))
	}
}

// Protects acknowledging one family's alert without acknowledging the other's,
// and that the acknowledgement of each survives a restart: a person who has
// seen the v6 outage has not seen the v4 one.
func TestAcknowledgeAndRestartArePerFamily(t *testing.T) {
	ctx := context.Background()
	st, m, leaf, rule := familyManager(t, nil)
	for step := range 2 {
		m.Observe(ctx, []alert.Input{
			inputOf(leaf, tree.FamilyV4, step, 10, 0),
			inputOf(leaf, tree.FamilyV6, step, 10, 0),
		})
	}
	if changed, err := m.Acknowledge(ctx, rule.ID, leaf, store.LocalAgentID, tree.FamilyV6, true, "tim"); err != nil || !changed {
		t.Fatalf("acknowledge v6: changed=%v err=%v", changed, err)
	}
	check := func(when string, m *alert.Manager) {
		t.Helper()
		got := firingBy(m)
		if len(got) != 2 || !got[tree.FamilyV6].Acked || got[tree.FamilyV4].Acked {
			t.Errorf("%s: want v6 acknowledged and v4 not, got v4 acked=%v v6 acked=%v (%d alerts)",
				when, got[tree.FamilyV4].Acked, got[tree.FamilyV6].Acked, len(got))
		}
	}
	check("after the acknowledge", m)

	m2 := alert.NewManager(st, nil)
	if err := m2.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	check("after a save and reload", m2)

	// A family nothing is firing for cannot be acknowledged, whatever the
	// other has.
	if changed, err := m2.Acknowledge(ctx, rule.ID, leaf, store.LocalAgentID, tree.FamilyUnknown, true, "tim"); err != nil || changed {
		t.Errorf("acknowledging a family that is not firing: changed=%v err=%v", changed, err)
	}
}

// Protects the family scope of a silence: one scoped to v6 mutes the v6 alert
// and neither shows the v4 one as silenced nor holds its notification back; one
// without a family covers both.
func TestASilenceScopedToAFamilyMutesOnlyThatFamily(t *testing.T) {
	ctx := context.Background()
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	_, m, leaf, _ := familyManager(t, &alert.Webhook{URL: srv.URL})

	v6 := tree.FamilyV6
	sil, err := m.AddSilence(ctx, alert.Silence{Family: &v6, StartsAt: 1, EndsAt: 1 << 40})
	if err != nil {
		t.Fatal(err)
	}
	for step := range 2 {
		m.Observe(ctx, []alert.Input{
			inputOf(leaf, tree.FamilyV4, step, 10, 0),
			inputOf(leaf, tree.FamilyV6, step, 10, 0),
		})
	}
	got := firingBy(m)
	if len(got) != 2 || got[tree.FamilyV4].Silenced || !got[tree.FamilyV6].Silenced {
		t.Errorf("want only v6 silenced, got v4=%v v6=%v", got[tree.FamilyV4].Silenced, got[tree.FamilyV6].Silenced)
	}
	sent := cap.all()
	if len(sent) != 1 || sent[0]["labels"].(map[string]any)["family"] != "v4" {
		t.Errorf("want one notification, for v4, got %v", sent)
	}

	// Without a family the same silence covers both, as a silence always did.
	if ok, err := m.RemoveSilence(ctx, sil.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := m.AddSilence(ctx, alert.Silence{StartsAt: 1, EndsAt: 1 << 40}); err != nil {
		t.Fatal(err)
	}
	got = firingBy(m)
	if !got[tree.FamilyV4].Silenced || !got[tree.FamilyV6].Silenced {
		t.Errorf("a silence without a family should cover both, got v4=%v v6=%v", got[tree.FamilyV4].Silenced, got[tree.FamilyV6].Silenced)
	}
}

// Protects the webhook against dropping or changing a label Alertmanager routes
// on while adding the family: every label that was there is exactly as it was,
// and there is one more.
func TestWebhookCarriesTheFamilyAndKeepsEveryOtherLabel(t *testing.T) {
	ctx := context.Background()
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	_, m, leaf, _ := familyManager(t, &alert.Webhook{URL: srv.URL})

	for step := range 2 {
		m.Observe(ctx, []alert.Input{inputOf(leaf, tree.FamilyV6, step, 10, 0)})
	}
	sent := cap.all()
	if len(sent) != 1 {
		t.Fatalf("got %d notifications, want 1", len(sent))
	}
	labels := map[string]string{}
	for k, v := range sent[0]["labels"].(map[string]any) {
		labels[k] = v.(string)
	}
	want := map[string]string{
		"alertname": "loss", "target": "/Production/gw", "host": "10.0.0.1", "agent": "local",
		"metric": "loss", "severity": "warning",
		"family": "v6",
	}
	if !maps.Equal(labels, want) {
		t.Errorf("labels = %v\nwant     %v", labels, want)
	}
}

// Protects the transition log and the golden reference from losing the family:
// an event says which family fired, and a capture is stored with the family it
// was read from.
func TestEventsAndBaselinesCarryTheFamily(t *testing.T) {
	ctx := context.Background()
	st, m, leaf, rule := familyManager(t, nil)
	for step := range 2 {
		m.Observe(ctx, []alert.Input{inputOf(leaf, tree.FamilyV6, step, 10, 0)})
	}
	evs, err := st.ListAlertEvents(ctx, 10)
	if err != nil || len(evs) != 1 || evs[0].Family != tree.FamilyV6 {
		t.Fatalf("events = %+v, %v", evs, err)
	}
	if err := m.CaptureBaseline(ctx, alert.Baselined{RuleID: rule.ID, TargetID: leaf, Family: tree.FamilyV6,
		Samples: []uint32{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	if bs := m.Baselines(); len(bs) != 1 || bs[0].Family != tree.FamilyV6 {
		t.Errorf("baselines = %+v", bs)
	}
}

// Protects the shape memory from being pooled across families: a rolling
// reference is built from one series' own history, so intervals of v4 must not
// become the baseline v6 is judged against.
func TestShapeMemoryIsPerFamily(t *testing.T) {
	ctx := context.Background()
	st, groupID, leaf := setup(t)
	rule := alert.Rule{
		TargetID: groupID, Name: "shape", Metric: alert.MetricShape, Op: alert.OpGreater,
		Threshold: 5, For: 2, ClearFor: 2, Enabled: true, Mode: alert.ModeTunable,
		Baseline: alert.BaselineRolling, Severity: alert.SeverityWarning,
	}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, nil)
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	for step := range 3 {
		m.Observe(ctx, []alert.Input{inputOf(leaf, tree.FamilyV4, step, 10, 10)})
	}
	if ref := m.ShapeReferenceDetail(rule.ID, leaf, store.LocalAgentID, tree.FamilyV4); !ref.OK {
		t.Error("v4 has been measured and has no rolling reference")
	}
	if ref := m.ShapeReferenceDetail(rule.ID, leaf, store.LocalAgentID, tree.FamilyV6); ref.OK {
		t.Error("v6 was never measured and has a rolling reference: the two families share a shape memory")
	}
}

// Protects against an alert that can never clear. A target's family is changed
// while a rule is firing on it: the old family gets no more intervals, and
// keyed on the family nothing would ever advance that state again, so it would
// fire, and be repeated to the webhook, until the rule was deleted. The reload
// that sees the change ends it, in memory and in the store, so that a restart
// does not bring it back; the new family then starts clean.
func TestChangingATargetsFamilyEndsWhatWasFiringOnTheOldOne(t *testing.T) {
	ctx := context.Background()
	st, m, leaf, rule := familyManager(t, nil)
	for step := range 2 {
		m.Observe(ctx, []alert.Input{inputOf(leaf, tree.FamilyV4, step, 10, 0)})
	}
	if got := firingBy(m); len(got) != 1 {
		t.Fatalf("setup: %d alerts firing on v4, want 1", len(got))
	}

	targets, err := st.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range targets {
		if targets[i].ID == leaf {
			targets[i].Host, targets[i].AddressFamily = ptr("2001:db8::1"), ptr("v6")
			if err := st.UpsertTarget(ctx, &targets[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.Firing(); len(got) != 0 {
		t.Errorf("after the family changed, still firing: %+v", got)
	}
	// The prober's old loop is stopped only after the reload and can still
	// deliver an interval of the old family. It must not start a state that
	// nothing could clear: two of them would fire this rule.
	for step := 2; step < 4; step++ {
		m.Observe(ctx, []alert.Input{inputOf(leaf, tree.FamilyV4, step, 10, 0)})
	}
	if got := m.Firing(); len(got) != 0 {
		t.Errorf("late intervals of the old family fired: %+v", got)
	}
	// An Observe that took its snapshot before the reload saves it afterwards,
	// which puts the firing row back in the store. The next reload ends it again.
	stale := alert.State{RuleID: rule.ID, TargetID: leaf, AgentID: store.LocalAgentID, Family: tree.FamilyV4,
		Firing: true, Since: 1_756_400_060, LastTS: 1_756_400_060, Value: 100}
	if err := st.SaveAlertStates(ctx, []alert.State{stale}); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.Firing(); len(got) != 0 {
		t.Errorf("a firing row put back in the store came back to life: %+v", got)
	}
	states, err := st.ListAlertStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.Firing {
			t.Errorf("the store still holds a firing state: %+v", s)
		}
	}
	// A restart reads the store, and must not find it firing there either.
	m2 := alert.NewManager(st, nil)
	if err := m2.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m2.Firing(); len(got) != 0 {
		t.Errorf("after a restart the ended alert is back: %+v", got)
	}
	// The new family is a series of its own and fires on its own evidence.
	m.Observe(ctx, []alert.Input{inputOf(leaf, tree.FamilyV6, 4, 10, 0)})
	if got := m.Firing(); len(got) != 0 {
		t.Errorf("one bad v6 interval fired a rule that needs two: %+v", got)
	}
	m.Observe(ctx, []alert.Input{inputOf(leaf, tree.FamilyV6, 5, 10, 0)})
	if got := firingBy(m); len(got) != 1 || got[tree.FamilyV6].Family != tree.FamilyV6 {
		t.Errorf("two bad v6 intervals: firing %+v, want the v6 alert alone", got)
	}
}
