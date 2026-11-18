package agent

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/api"
	"github.com/timdebruijn/smokeng/internal/probe"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// bothMaster is a master with a "both" target assigned to the agent ams-01 and
// a loss rule on the root, served through the real routes. wrap, when set,
// sits in front of the API (to play an older agent or master).
type bothMaster struct {
	st     *store.SQLite
	mgr    *alert.Manager
	srv    *httptest.Server
	target int64
	agent  *Agent
	buf    *store.SQLite
	rec    store.AgentRecord
}

func newBothMaster(t *testing.T, wrap func(http.Handler) http.Handler) *bothMaster {
	t.Helper()
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
	target := tree.Target{ParentID: new(int64(1)), Name: "dual", Host: new("dual.example.net"),
		AddressFamily: new("both"), Enabled: true, Settings: tree.Settings{Agents: new("ams-01")}}
	if err := master.UpsertTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	rule := alert.Rule{TargetID: 1, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 2, ClearFor: 2, Enabled: true, Severity: alert.SeverityWarning}
	if err := master.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	mgr := alert.NewManager(master, nil)
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	var h http.Handler = api.New(master, api.Options{Alerts: mgr}, fstest.MapFS{})
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
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
	return &bothMaster{st: master, mgr: mgr, srv: srv, target: target.ID, agent: a, buf: buf, rec: rec}
}

// series is the families the agent's prober would measure the target over.
func (m *bothMaster) series(t *testing.T) map[string]bool {
	t.Helper()
	specs, err := probe.Specs(t.Context(), m.buf)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for k := range specs {
		if k.TargetID == m.target {
			out[k.Family] = true
		}
	}
	return out
}

func wantSeries(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("the agent measures %v, want %v", got, want)
		return
	}
	for _, f := range want {
		if !got[f] {
			t.Errorf("the agent measures %v, want %v", got, want)
		}
	}
}

// Protects the whole path of a "both" target through a new agent, over the
// real routes: the agent says it can (the master sends both), mirrors it, its
// prober holds two series, and a row and a path change recorded locally under
// each family reach the master under that family. v6 is lossy and v4 is not,
// so the rule on the master must fire for v6 alone: if the agent filed both
// under one family, or the master pooled them, that would not hold.
func TestAnAgentMeasuresABothTargetOverBothFamilies(t *testing.T) {
	ctx := t.Context()
	m := newBothMaster(t, nil)
	if err := m.agent.pull(ctx); err != nil {
		t.Fatalf("pull: %v", err)
	}
	wantSeries(t, m.series(t), "v4", "v6")

	now := time.Now().Unix() / 60 * 60
	row := func(ts int64, fam tree.Family, received int) store.Measurement {
		return store.Measurement{TargetID: m.target, AgentID: store.LocalAgentID, Family: fam, TS: ts,
			Sent: 5, Received: received, Samples: make([]uint32, received)}
	}
	var rows []store.Measurement
	for i := range 2 {
		ts := now - int64(120-60*i)
		rows = append(rows, row(ts, tree.FamilyV4, 5), row(ts, tree.FamilyV6, 1))
	}
	if err := m.buf.WriteMeasurements(ctx, rows); err != nil {
		t.Fatal(err)
	}
	want := map[tree.Family]string{tree.FamilyV4: "10.0.0.1,192.0.2.9", tree.FamilyV6: "2001:db8::1,2001:db8::9"}
	for fam, hops := range want {
		if err := m.buf.RecordPath(ctx, m.target, store.LocalAgentID, fam, now-60, hops); err != nil {
			t.Fatal(err)
		}
	}
	m.agent.push(ctx)

	for _, fam := range []tree.Family{tree.FamilyV4, tree.FamilyV6} {
		got, err := m.st.QueryRange(ctx, m.target, m.rec.ID, fam, 0, now+60)
		if err != nil || len(got) != 2 {
			t.Errorf("master holds %d rows under %v (%v), want 2", len(got), fam, err)
		}
	}
	for fam, hops := range want {
		got, err := m.st.PathChanges(ctx, m.target, m.rec.ID, fam, 0, now+60)
		if err != nil || len(got) != 1 || got[0].Hops != hops {
			t.Errorf("%v paths = %+v (%v), want one change %q", fam, got, err, hops)
		}
	}
	firing := m.mgr.Firing()
	if len(firing) != 1 || firing[0].Family != tree.FamilyV6 {
		t.Errorf("firing = %+v, want the loss rule on v6 alone", firing)
	}
}

// Protects an older agent, simulated by a pull that does not carry the
// X-Agent-Features header (it is not signed, so removing it in front of the
// master is what an older agent's request looks like): the master sends v4,
// the mirror succeeds, and the agent measures one series. If the master sent
// both regardless, the older agent's sync would fail on a value it refuses.
func TestAnAgentThatDoesNotSayItCanMeasuresABothTargetOverV4(t *testing.T) {
	ctx := t.Context()
	m := newBothMaster(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("X-Agent-Features")
			h.ServeHTTP(w, r)
		})
	})
	if err := m.agent.pull(ctx); err != nil {
		t.Fatalf("pull: %v", err)
	}
	targets, err := m.buf.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		if tg.ID == m.target && (tg.AddressFamily == nil || *tg.AddressFamily != "v4") {
			t.Errorf("mirrored address_family = %v, want v4", tg.AddressFamily)
		}
	}
	wantSeries(t, m.series(t), "v4")
}

// Protects a new agent under an older master, simulated by a master that
// answers v4 although the agent sent the header (an older master ignores it).
// The agent mirrors what it is sent and measures one series; it must not
// widen the target to both on its own.
func TestANewAgentUnderAnOlderMasterMeasuresOneSeries(t *testing.T) {
	var sawHeader string
	a, st, _ := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("X-Agent-Features")
		fmt.Fprint(w, `{"targets":[{"target_id":42,"path":"/dual","host":"dual.example.net","address_family":"v4",`+
			`"interval_s":60,"pings":20,"probe_mode":"burst","burst_gap_ms":10,"timeout_ms":1000,"packet_size":56,"dscp":0,`+
			`"probe_type":"icmp","probe_port":0,"dns_query":"","dns_rr_type":"","http_path":"","tls_skip_verify":false,`+
			`"trace_interval_s":300}]}`)
	}))
	if err := a.pull(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sawHeader != "both" {
		t.Errorf("X-Agent-Features = %q, want both", sawHeader)
	}
	specs, err := probe.Specs(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := specs[probe.SpecKey{TargetID: 42, Family: "v4"}]; !ok || len(specs) != 1 {
		t.Errorf("the agent measures %d series (%v), want v4 alone", len(specs), specs)
	}
}
