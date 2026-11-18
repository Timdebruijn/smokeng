package api

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Protects the interval count of a captured baseline: an interval that sent
// nothing (no data) holds no samples, so it is not one the baseline was taken
// from, and the count must not include it.
func TestBaselineCaptureCountsOnlyIntervalsThatSent(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "baseline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tg := tree.Target{ParentID: new(int64(1)), Name: "web", Enabled: true, Host: new("192.0.2.1"), AddressFamily: new("v4")}
	if err := st.UpsertTarget(ctx, &tg); err != nil {
		t.Fatal(err)
	}
	rule := alert.Rule{TargetID: tg.ID, Name: "shape", Metric: alert.MetricShape, Op: alert.OpGreater,
		Threshold: 5, For: 1, ClearFor: 1, Enabled: true, Mode: alert.ModeTunable, Baseline: alert.BaselineGolden,
		Severity: alert.SeverityWarning}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix() / 60 * 60
	noData := store.SendReasonNoAddress
	var ms []store.Measurement
	for i := 1; i <= 5; i++ {
		ms = append(ms, store.Measurement{TargetID: tg.ID, Family: tree.FamilyV4, TS: now - int64(i)*60,
			Sent: 3, Received: 3, Samples: []uint32{100, 100, 100}})
	}
	ms = append(ms,
		store.Measurement{TargetID: tg.ID, Family: tree.FamilyV4, TS: now - 6*60, Flags: store.FlagSendFailed, SendErr: &noData},
		store.Measurement{TargetID: tg.ID, Family: tree.FamilyV4, TS: now - 7*60, Flags: store.FlagSendFailed, SendErr: &noData})
	if err := st.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, nil)
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	h := New(st, Options{Alerts: m}, fstest.MapFS{})

	code, body := call(t, h, "POST", "/api/v1/alert-rules/"+num(rule.ID)+"/baseline",
		map[string]any{"target_id": tg.ID, "agent_id": 0})
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	if code != 200 || out["intervals"] != float64(5) || out["samples"] != float64(15) {
		t.Fatalf("capture = %d %s, want 5 intervals and 15 samples", code, body)
	}
}
