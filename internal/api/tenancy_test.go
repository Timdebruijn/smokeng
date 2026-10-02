package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// tenants is two customers under a shared parent, which is what a real
// installation looks like and what the older two-customer fixture is not: its
// grants sit directly under the root, so there is no ancestor above a grant for
// anything to leak. Here /Klanten is above both, and no scoped caller may ever
// learn its name.
//
//	/Klanten/GemeenteA/gw      (grant root: GemeenteA)
//	/Klanten/GemeenteB/gw      (measured by the agent "ams-b", which A has no use for)
//
// Each customer has a golden shape rule on its own node with a captured
// reference, and the references are told apart by value.
type tenancy struct {
	st                 *store.SQLite
	mgr                *alert.Manager
	klanten            int64
	groupA, hostA      int64
	groupB, hostB      int64
	ruleA, ruleB       int64
	agentB             int64
	now                int64
	baselineA, baseliB []uint32
}

func tenants(t *testing.T) tenancy {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tenants.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	root := int64(1)

	mk := func(parent int64, name string, host *string, set func(*tree.Target)) int64 {
		n := tree.Target{ParentID: &parent, Name: name, Enabled: true}
		if host != nil {
			n.Host, n.AddressFamily = host, ptr("v4")
		}
		if set != nil {
			set(&n)
		}
		if err := st.UpsertTarget(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	f := tenancy{st: st, now: time.Now().Unix(),
		baselineA: []uint32{777, 888}, baseliB: []uint32{11111, 22222, 33333}}
	f.klanten = mk(root, "Klanten", nil, nil)
	// A setting defined on the grant root, so that what a node inherits names
	// the grant root as its source, and the source's path runs through /Klanten.
	f.groupA = mk(f.klanten, "GemeenteA", nil, func(n *tree.Target) { n.Settings.IntervalS = ptr(30) })
	f.hostA = mk(f.groupA, "gw", ptr("198.51.100.1"), nil)
	f.groupB = mk(f.klanten, "GemeenteB", nil, nil)

	pub := make([]byte, 32)
	pub[0] = 7
	rec, err := st.AddAgent(ctx, "ams-b", pub)
	if err != nil {
		t.Fatal(err)
	}
	f.agentB = rec.ID
	f.hostB = mk(f.groupB, "gw", ptr("198.51.100.2"), func(n *tree.Target) { n.Settings.Agents = ptr("ams-b") })

	golden := func(node int64) int64 {
		r := alert.Rule{TargetID: node, Name: "shape", Metric: alert.MetricShape, Op: alert.OpGreater,
			Threshold: 3, For: 3, ClearFor: 3, Enabled: true, Mode: alert.ModeAuto, Baseline: alert.BaselineGolden}
		if err := st.UpsertAlertRule(ctx, &r); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	f.ruleA, f.ruleB = golden(f.groupA), golden(f.groupB)

	f.mgr = alert.NewManager(st, nil)
	if err := f.mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rule, target, agent int64
		samples             []uint32
	}{{f.ruleA, f.hostA, 0, f.baselineA}, {f.ruleB, f.hostB, f.agentB, f.baseliB}} {
		if err := f.mgr.CaptureBaseline(ctx, alert.Baselined{
			RuleID: c.rule, TargetID: c.target, AgentID: c.agent, FromTS: f.now - 3600, ToTS: f.now,
			Intervals: 1, Samples: c.samples, CapturedAt: f.now, CapturedBy: "someone@" + "example.org",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Measurements for both gateways, in a window a capture can read.
	rows := func(target, agent int64, v uint32) []store.Measurement {
		var ms []store.Measurement
		for i := range 5 {
			ms = append(ms, store.Measurement{TargetID: target, AgentID: agent, TS: f.now - 600 + int64(i)*60,
				Sent: 1, Received: 1, Samples: []uint32{v}})
		}
		return ms
	}
	if err := st.WriteMeasurements(ctx, append(rows(f.hostA, 0, 100), rows(f.hostB, f.agentB, 99999)...)); err != nil {
		t.Fatal(err)
	}
	return f
}

// server signs the caller in as a member of group, holding role on node and
// nothing else (--default-role none).
func (f tenancy) server(t *testing.T, group string, node int64, role string) http.Handler {
	t.Helper()
	g := store.Grant{Group: group, TargetID: node, Role: role}
	if err := f.st.UpsertGrant(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	sess := &auth.Session{Subject: "u", Role: auth.RoleViewer, Groups: []string{group}, Expires: 1 << 40}
	return New(f.st, Options{Auth: &fakeAuth{session: sess}, DefaultRole: auth.RoleNone, Alerts: f.mgr}, fstest.MapFS{})
}

func call(t *testing.T, h http.Handler, method, path string, body any) (int, string) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func num(n int64) string { return strings.TrimSpace(strings.Trim(string(mustJSON(n)), "\n")) }

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// A golden reference belongs to the target it was captured from. The endpoint
// authorised the target named in the query and returned the rule's reference
// without asking whose it was, so any caller who could see one target could
// read every other tenant's baseline by naming the rule.
func TestShapeReferenceDoesNotServeAnotherTenantsBaseline(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "viewer")

	code, body := call(t, h, "GET", "/api/v1/shape-reference?rule_id="+num(f.ruleB)+"&target_id="+num(f.hostA)+"&agent_id=0", nil)
	if strings.Contains(body, "11111") || strings.Contains(body, "22222") {
		t.Fatalf("another tenant's golden reference was served (%d): %s", code, body)
	}
	if code == http.StatusOK {
		t.Errorf("a rule that is not theirs answered 200: %s", body)
	}

	// The control: their own rule, their own target, still works.
	code, body = call(t, h, "GET", "/api/v1/shape-reference?rule_id="+num(f.ruleA)+"&target_id="+num(f.hostA)+"&agent_id=0", nil)
	if code != http.StatusOK || !strings.Contains(body, "777") {
		t.Errorf("their own golden reference no longer reads: %d %s", code, body)
	}
}

// Whether a rule exists, and what kind it is, is the same disclosure as its
// samples. A rule that does not exist and a rule on another customer's node
// get the same answer, and so does a real rule of theirs that does not reach
// the target they named.
func TestShapeReferenceAnswersUnknownAndForeignRulesAlike(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "viewer")
	var first string
	for name, q := range map[string]string{
		"nonexistent":      "rule_id=99999&target_id=" + num(f.hostA),
		"another tenant's": "rule_id=" + num(f.ruleB) + "&target_id=" + num(f.hostA),
	} {
		code, body := call(t, h, "GET", "/api/v1/shape-reference?"+q, nil)
		if code != http.StatusNotFound {
			t.Errorf("%s: %d %s, want 404", name, code, body)
		}
		if first == "" {
			first = body
		} else if body != first {
			t.Errorf("%s answers differently from the other:\n%s\n%s", name, body, first)
		}
	}

	// A target they cannot see is refused as a target, whatever the rule is:
	// that answer says nothing about the rule, and is what every endpoint says
	// about a node outside the scope.
	code, body := call(t, h, "GET", "/api/v1/shape-reference?rule_id="+num(f.ruleA)+"&target_id="+num(f.hostB), nil)
	if code != http.StatusNotFound || strings.Contains(body, "11111") {
		t.Errorf("a target outside the scope: %d %s", code, body)
	}
}

// Capturing a reference reads the window from stored measurements. The rule
// was authorised, but the target to read from came from the request body, so an
// editor of one tenant could point their own rule at another's target and read
// its samples back through shape-reference.
func TestCaptureCannotReadAnotherTenantsTarget(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "editor")

	code, body := call(t, h, "POST", "/api/v1/alert-rules/"+num(f.ruleA)+"/baseline",
		map[string]any{"target_id": f.hostB, "agent_id": f.agentB, "from": f.now - 3600, "to": f.now})
	if code == http.StatusOK {
		t.Fatalf("captured a reference from another tenant's target: %s", body)
	}
	if strings.Contains(body, "ams-b") || strings.Contains(body, "198.51.100.2") {
		t.Errorf("the refusal discloses the other tenant: %s", body)
	}
	// And nothing was stored: their rule still holds its own reference.
	_, ref := call(t, h, "GET", "/api/v1/shape-reference?rule_id="+num(f.ruleA)+"&target_id="+num(f.hostA)+"&agent_id=0", nil)
	if strings.Contains(ref, "99999") {
		t.Fatalf("their rule now holds the other tenant's samples: %s", ref)
	}

	// The control: a target under their own rule's node is fine.
	code, body = call(t, h, "POST", "/api/v1/alert-rules/"+num(f.ruleA)+"/baseline",
		map[string]any{"target_id": f.hostA, "agent_id": 0, "from": f.now - 3600, "to": f.now})
	if code != http.StatusOK {
		t.Errorf("capturing from their own target = %d: %s", code, body)
	}
}

// The capture window is the caller's, and it was not bounded: every sample in
// it is read into memory, concatenated, sorted and stored as one blob.
func TestCaptureWindowIsBounded(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "editor")
	for name, w := range map[string][2]int64{
		"from the epoch":      {1, f.now},
		"int64 extremes":      {-1 << 63, 1<<63 - 1},
		"a year":              {f.now - 365*86400, f.now},
		"negative to maximum": {-1, 1<<63 - 1},
	} {
		code, body := call(t, h, "POST", "/api/v1/alert-rules/"+num(f.ruleA)+"/baseline",
			map[string]any{"target_id": f.hostA, "agent_id": 0, "from": w[0], "to": w[1]})
		if code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, body)
		}
	}
}

// Probing for rule ids: before any check of scope, a missing rule was a 404, a
// rule of another kind a 400, and only then was the caller's right to it asked.
func TestCaptureAnswersUnknownAndForeignRulesAlike(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "editor")
	req := map[string]any{"target_id": f.hostA, "agent_id": 0}
	c1, b1 := call(t, h, "POST", "/api/v1/alert-rules/99999/baseline", req)
	c2, b2 := call(t, h, "POST", "/api/v1/alert-rules/"+num(f.ruleB)+"/baseline", req)
	if c1 != http.StatusNotFound || c2 != http.StatusNotFound || b1 != b2 {
		t.Errorf("nonexistent: %d %s; another tenant's: %d %s", c1, b1, c2, b2)
	}
}

// A rule defined above both customers reaches both of them by inheritance, and
// holds one reference captured from one series. A caller who can see their own
// part of its subtree can legitimately ask what a firing alert is compared
// against, but the reference is only theirs to read if the series it came from
// is. Here it came from the other customer.
func TestSharedRulesReferenceIsOnlyShownWhereItsSourceIsVisible(t *testing.T) {
	f := tenants(t)
	ctx := context.Background()
	shared := alert.Rule{TargetID: f.klanten, Name: "shared", Metric: alert.MetricShape, Op: alert.OpGreater,
		Threshold: 3, For: 3, ClearFor: 3, Enabled: true, Mode: alert.ModeAuto, Baseline: alert.BaselineGolden}
	if err := f.st.UpsertAlertRule(ctx, &shared); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.CaptureBaseline(ctx, alert.Baselined{
		RuleID: shared.ID, TargetID: f.hostB, AgentID: f.agentB, FromTS: f.now - 3600, ToTS: f.now,
		Intervals: 1, Samples: []uint32{424242}, CapturedAt: f.now,
	}); err != nil {
		t.Fatal(err)
	}
	q := "/api/v1/shape-reference?rule_id=" + num(shared.ID) + "&target_id=%s&agent_id=0"

	a := f.server(t, "team-a", f.groupA, "viewer")
	code, body := call(t, a, "GET", strings.Replace(q, "%s", num(f.hostA), 1), nil)
	if code != http.StatusOK {
		t.Fatalf("the rule reaches their target, so the endpoint answers: %d %s", code, body)
	}
	if strings.Contains(body, "424242") {
		t.Errorf("a reference captured from another customer was shown: %s", body)
	}
	if !strings.Contains(body, `"available":false`) {
		t.Errorf("an unreadable reference should read as not available: %s", body)
	}

	// The rule reaches the other customer's target too. Naming it must not turn
	// the endpoint into a way to read that target's current interval.
	code, body = call(t, a, "GET", strings.Replace(q, "%s", num(f.hostB), 1), nil)
	if code != http.StatusNotFound || strings.Contains(body, "99999") {
		t.Errorf("a rule that reaches another customer's target served it: %d %s", code, body)
	}

	// The control: the customer the reference came from reads it.
	b := f.server(t, "team-b", f.groupB, "viewer")
	code, body = call(t, b, "GET", strings.Replace(q, "%s", num(f.hostB), 1), nil)
	if code != http.StatusOK || !strings.Contains(body, "424242") || !strings.Contains(body, `"available":true`) {
		t.Errorf("the source's own customer no longer reads it: %d %s", code, body)
	}
}

// Capturing a reference is a write to the node the rule is defined on. A viewer
// can see the rule and read what it compares against, and may not change it.
func TestCaptureNeedsWriteAccessToTheRule(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "viewer")
	code, body := call(t, h, "POST", "/api/v1/alert-rules/"+num(f.ruleA)+"/baseline",
		map[string]any{"target_id": f.hostA, "agent_id": 0, "from": f.now - 3600, "to": f.now})
	if code != http.StatusForbidden {
		t.Errorf("a viewer capturing a baseline = %d %s, want 403", code, body)
	}
	code, body = call(t, h, "DELETE", "/api/v1/alert-rules/"+num(f.ruleA)+"/baseline", nil)
	if code != http.StatusForbidden {
		t.Errorf("a viewer clearing a baseline = %d %s, want 403", code, body)
	}
	// And it is still there.
	_, ref := call(t, h, "GET", "/api/v1/shape-reference?rule_id="+num(f.ruleA)+"&target_id="+num(f.hostA)+"&agent_id=0", nil)
	if !strings.Contains(ref, "777") {
		t.Errorf("the reference did not survive a viewer's attempts: %s", ref)
	}
}

// What a rule is must not be told to someone who may not know it exists. The
// kind check used to come first, so a rule of the wrong kind on another
// customer's node answered 400 where a missing one answered 404.
func TestCaptureDoesNotSayWhatKindOfRuleItIsBeforeAuthorising(t *testing.T) {
	f := tenants(t)
	ctx := context.Background()
	loss := alert.Rule{TargetID: f.groupB, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 3, ClearFor: 3, Enabled: true}
	if err := f.st.UpsertAlertRule(ctx, &loss); err != nil {
		t.Fatal(err)
	}
	h := f.server(t, "team-a", f.groupA, "editor")
	req := map[string]any{"target_id": f.hostA, "agent_id": 0}
	c1, b1 := call(t, h, "POST", "/api/v1/alert-rules/99999/baseline", req)
	c2, b2 := call(t, h, "POST", "/api/v1/alert-rules/"+num(loss.ID)+"/baseline", req)
	if c1 != c2 || b1 != b2 {
		t.Errorf("a missing rule answers %d %s, another customer's rule of the wrong kind %d %s", c1, b1, c2, b2)
	}
}
