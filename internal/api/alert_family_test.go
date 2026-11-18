package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// alertFamilyServer is a v4 target with a loss rule firing for both families
// (fed to the manager directly: no target can have two families through
// configuration yet) and a golden shape rule, with stored measurements that
// tell the families apart: v4 samples are 100 µs, v6 samples 200 µs and fewer.
func alertFamilyServer(t *testing.T) (h http.Handler, leaf int64, lossRule, shapeRule alert.Rule) {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "alertfam.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tg := tree.Target{ParentID: new(int64(1)), Name: "web", Enabled: true, Host: new("192.0.2.1"), AddressFamily: new("v4")}
	if err := st.UpsertTarget(ctx, &tg); err != nil {
		t.Fatal(err)
	}
	lossRule = alert.Rule{TargetID: tg.ID, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 1, ClearFor: 1, Enabled: true, Severity: alert.SeverityWarning}
	shapeRule = alert.Rule{TargetID: tg.ID, Name: "shape", Metric: alert.MetricShape, Op: alert.OpGreater,
		Threshold: 5, For: 1, ClearFor: 1, Enabled: true, Mode: alert.ModeTunable, Baseline: alert.BaselineGolden,
		Severity: alert.SeverityWarning}
	for _, r := range []*alert.Rule{&lossRule, &shapeRule} {
		if err := st.UpsertAlertRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix() / 60 * 60
	var ms []store.Measurement
	for i := 1; i <= 5; i++ {
		ts := now - int64(i)*60
		ms = append(ms,
			store.Measurement{TargetID: tg.ID, Family: tree.FamilyV4, TS: ts, Sent: 3, Received: 3, Samples: []uint32{100, 100, 100}},
			store.Measurement{TargetID: tg.ID, Family: tree.FamilyV6, TS: ts, Sent: 3, Received: 1, Samples: []uint32{200}})
	}
	if err := st.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	m := alert.NewManager(st, nil)
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	for _, fam := range []tree.Family{tree.FamilyV4, tree.FamilyV6} {
		m.Observe(ctx, []alert.Input{{TargetID: tg.ID, Family: fam, TS: now, Sent: 10, Received: 0,
			LossTrusted: true, RTTTrusted: true}})
	}
	return New(st, Options{Alerts: m}, fstest.MapFS{}), tg.ID, lossRule, shapeRule
}

func decode(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
}

// Protects the additive family fields of the firing list and the history, and
// that acknowledging picks the series by family, defaulting to the target's
// current one: an old client that sends none still acknowledges what it did.
func TestAlertsAndEventsNameTheirFamilyAndAckTakesOne(t *testing.T) {
	h, leaf, rule, _ := alertFamilyServer(t)

	var firing struct {
		Alerts []struct {
			Rule   string `json:"rule"`
			Family string `json:"family"`
			Acked  bool   `json:"acked"`
		} `json:"alerts"`
	}
	read := func() map[string]bool {
		t.Helper()
		code, body := call(t, h, "GET", "/api/v1/alerts", nil)
		if code != 200 {
			t.Fatalf("GET alerts: %d %s", code, body)
		}
		decode(t, body, &firing)
		acked := map[string]bool{}
		for _, a := range firing.Alerts {
			if a.Rule == "loss" {
				acked[a.Family] = a.Acked
			}
		}
		return acked
	}
	if got := read(); len(got) != 2 {
		t.Fatalf("want a loss alert per family, got %v", got)
	}

	var events struct {
		Events []struct{ Family string } `json:"events"`
	}
	_, body := call(t, h, "GET", "/api/v1/alert-events", nil)
	decode(t, body, &events)
	fams := map[string]int{}
	for _, e := range events.Events {
		fams[e.Family]++
	}
	if fams["v4"] == 0 || fams["v6"] == 0 {
		t.Errorf("events do not say both families: %v", fams)
	}

	ack := func(extra map[string]any) (int, string) {
		b := map[string]any{"rule_id": rule.ID, "target_id": leaf, "agent_id": 0}
		for k, v := range extra {
			b[k] = v
		}
		return call(t, h, "POST", "/api/v1/alerts/ack", b)
	}
	// No family: the target's current one, v4, and only that.
	if code, body := ack(nil); code != 200 {
		t.Fatalf("ack without a family: %d %s", code, body)
	}
	if got := read(); !got["v4"] || got["v6"] {
		t.Errorf("an ack without a family must acknowledge the target's family only, got %v", got)
	}
	if code, body := ack(map[string]any{"family": "v6"}); code != 200 {
		t.Fatalf("ack v6: %d %s", code, body)
	}
	if got := read(); !got["v4"] || !got["v6"] {
		t.Errorf("after acknowledging both, got %v", got)
	}
	// Nothing fires for a family that was never measured.
	if code, _ := ack(map[string]any{"family": "unknown"}); code != 404 {
		t.Errorf("ack of a family that is not firing = %d, want 404", code)
	}
	for _, bad := range []any{"v5", "", 4, true} {
		code, body := ack(map[string]any{"family": bad})
		if code != 400 || !strings.Contains(body, "v4, v6 or unknown") {
			t.Errorf("ack with family %v = %d %s, want a 400 naming the accepted values", bad, code, body)
		}
	}
}

// Protects the silence's optional family on the wire: absent means every family
// and is not returned, a family is returned as given, and a misspelt one is a
// 400 rather than a silence that covers nothing.
func TestSilenceFamilyOnTheWire(t *testing.T) {
	h, leaf, _, _ := alertFamilyServer(t)
	post := func(extra map[string]any) (int, string) {
		b := map[string]any{"target_id": leaf, "duration_s": 600, "reason": "r"}
		for k, v := range extra {
			b[k] = v
		}
		return call(t, h, "POST", "/api/v1/silences", b)
	}
	for _, f := range []map[string]any{{"family": "v6"}, {"family": nil}, nil} {
		if code, body := post(f); code != 200 {
			t.Fatalf("create %v: %d %s", f, code, body)
		}
	}
	var list struct {
		Silences []map[string]any `json:"silences"`
	}
	_, body := call(t, h, "GET", "/api/v1/silences", nil)
	decode(t, body, &list)
	var scoped, every int
	for _, s := range list.Silences {
		switch f, has := s["family"]; {
		case has && f == "v6":
			scoped++
		case !has:
			every++
		default:
			t.Errorf("unexpected family %v", f)
		}
	}
	if scoped != 1 || every != 2 {
		t.Errorf("want one silence scoped to v6 and two for every family, got %d and %d: %v", scoped, every, list.Silences)
	}
	for _, bad := range []any{"v5", 6} {
		if code, body := post(map[string]any{"family": bad}); code != 400 || !strings.Contains(body, "v4, v6 or unknown") {
			t.Errorf("create with family %v = %d %s, want a 400 naming the accepted values", bad, code, body)
		}
	}
}

// Protects the baseline's family: a capture reads the series of the family it
// is asked for (default the target's current one), stores and returns which,
// and shape-reference takes the same parameter; a bad one is a 400.
func TestBaselineCaptureAndShapeReferenceTakeAFamily(t *testing.T) {
	h, leaf, _, shape := alertFamilyServer(t)
	path := "/api/v1/alert-rules/" + num(shape.ID) + "/baseline"
	capture := func(extra map[string]any) (int, map[string]any, string) {
		b := map[string]any{"target_id": leaf, "agent_id": 0}
		for k, v := range extra {
			b[k] = v
		}
		code, body := call(t, h, "POST", path, b)
		var out map[string]any
		_ = json.Unmarshal([]byte(body), &out)
		return code, out, body
	}

	// Default: v4's five intervals of three samples.
	code, out, body := capture(nil)
	if code != 200 || out["family"] != "v4" || out["samples"] != float64(15) {
		t.Fatalf("capture without a family: %d %s, want v4 and 15 samples", code, body)
	}
	// v6's five intervals of one sample each.
	code, out, body = capture(map[string]any{"family": "v6"})
	if code != 200 || out["family"] != "v6" || out["samples"] != float64(5) {
		t.Fatalf("capture of v6: %d %s, want v6 and 5 samples", code, body)
	}
	var bl struct {
		Baselines []struct {
			RuleID int64  `json:"rule_id"`
			Family string `json:"family"`
		} `json:"baselines"`
	}
	_, body = call(t, h, "GET", "/api/v1/alert-baselines", nil)
	decode(t, body, &bl)
	if len(bl.Baselines) != 1 || bl.Baselines[0].Family != "v6" {
		t.Errorf("the stored baseline should say the family it was captured from: %s", body)
	}
	// A family that was never measured has nothing to capture.
	if code, _, _ := capture(map[string]any{"family": "unknown"}); code != 400 {
		t.Errorf("capture of a family with no measurements = %d, want 400", code)
	}
	for _, bad := range []any{"v5", 4} {
		if code, _, body := capture(map[string]any{"family": bad}); code != 400 || !strings.Contains(body, "v4, v6 or unknown") {
			t.Errorf("capture with family %v = %d %s, want a 400 naming the accepted values", bad, code, body)
		}
	}

	ref := "/api/v1/shape-reference?rule_id=" + num(shape.ID) + "&target_id=" + num(leaf) + "&agent_id=0"
	for _, c := range []struct {
		query, family string
		current       int
	}{{"", "v4", 3}, {"&family=v4", "v4", 3}, {"&family=v6", "v6", 1}} {
		code, body := call(t, h, "GET", ref+c.query, nil)
		var got struct {
			Family  string   `json:"family"`
			Current []uint32 `json:"current"`
		}
		decode(t, body, &got)
		if code != 200 || got.Family != c.family || len(got.Current) != c.current {
			t.Errorf("shape-reference%s = %d %s, want family %s read from %d samples", c.query, code, body, c.family, c.current)
		}
	}
	if code, body := call(t, h, "GET", ref+"&family=v5", nil); code != 400 || !strings.Contains(body, "v4, v6 or unknown") {
		t.Errorf("shape-reference with a bad family = %d %s", code, body)
	}
}
