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

// firingOverride makes the real manager report a chosen set of firing alerts,
// which is the only way to put one in a test without driving a rule through
// its evaluation.
type firingOverride struct {
	*alert.Manager
	fire []alert.Alert
}

func (o firingOverride) Firing() []alert.Alert { return o.fire }

// The requirement of DESIGN.md §7.4 stated once for every read route rather
// than once per handler: nothing above the grant root, beside it, or measuring
// something elsewhere may appear in any response a scoped caller can get. That
// includes the name of an ancestor, which is not a node they have a grant on
// and so is exactly what a per-handler visibility check does not think to
// check.
//
// The route-coverage test checks that every route is classified. This checks
// that the classification is true.
func TestNoReadRouteNamesAnythingOutsideTheScope(t *testing.T) {
	f := tenants(t)
	ctx := context.Background()

	// Firing alerts for both customers, with the full path the manager records.
	ruleA, ruleB := alert.Rule{ID: f.ruleA, TargetID: f.groupA, Name: "shape"}, alert.Rule{ID: f.ruleB, TargetID: f.groupB, Name: "shape"}
	fire := []alert.Alert{
		{Rule: &ruleA, TargetID: f.hostA, TargetPath: "/Klanten/GemeenteA/gw", TargetHost: "198.51.100.1", AgentName: "local", Firing: true, Since: time.Now()},
		{Rule: &ruleB, TargetID: f.hostB, TargetPath: "/Klanten/GemeenteB/gw", TargetHost: "198.51.100.2", AgentID: f.agentB, AgentName: "ams-b", Firing: true, Since: time.Now()},
	}
	// A silence on each customer.
	for _, node := range []int64{f.groupA, f.groupB} {
		node := node
		now := time.Now().Unix()
		if _, err := f.mgr.AddSilence(ctx, alert.Silence{TargetID: &node, StartsAt: now, EndsAt: now + 3600,
			Reason: "maintenance", CreatedBy: "ops", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	forbidden := []string{"Klanten", "GemeenteB", "ams-b", "198.51.100.2"}
	routes := []string{
		"/api/v1/targets",
		"/api/v1/availability?target_id=" + num(f.hostA),
		"/api/v1/availability?target_id=" + num(f.hostA) + "&agent_id=" + num(f.agentB),
		"/api/v1/alert-rules",
		"/api/v1/alerts",
		"/api/v1/alert-baselines",
		"/api/v1/shape-reference?rule_id=" + num(f.ruleA) + "&target_id=" + num(f.hostA) + "&agent_id=0",
		"/api/v1/silences",
		"/api/v1/alert-events",
		"/api/v1/agents",
		"/api/v1/paths?target_id=" + num(f.hostA),
	}
	for _, role := range []string{"viewer", "editor"} {
		g := store.Grant{Group: "team-a-" + role, TargetID: f.groupA, Role: role}
		if err := f.st.UpsertGrant(ctx, &g); err != nil {
			t.Fatal(err)
		}
		sess := &auth.Session{Subject: "u", Role: auth.RoleViewer, Groups: []string{g.Group}, Expires: 1 << 40}
		h := New(f.st, Options{Auth: &fakeAuth{session: sess}, DefaultRole: auth.RoleNone,
			Alerts: firingOverride{Manager: f.mgr, fire: fire}}, fstest.MapFS{})
		for _, route := range routes {
			code, body := call(t, h, "GET", route, nil)
			if code != http.StatusOK {
				t.Errorf("%s %s: %d %s", role, route, code, body)
				continue
			}
			for _, bad := range forbidden {
				if strings.Contains(body, bad) {
					t.Errorf("%s GET %s names %q, which is outside the scope:\n%s", role, route, bad, body)
				}
			}
		}
	}
}

// An editor may name only the agents in their scope, the same set the picker
// offers. Anything else is the same answer whether the agent exists or not, and
// never a list: the old error named every enrolled agent to whoever got one
// wrong, and accepted any that was right, which pointed a target at another
// customer's vantage point.
func TestEditorMayOnlyNameAgentsInTheirScope(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "editor")
	patch := func(agents string) (int, string) {
		return call(t, h, "PATCH", "/api/v1/targets/"+num(f.hostA), map[string]any{"settings": map[string]any{"agents": agents}})
	}

	codeUnknown, bodyUnknown := patch("no-such-agent")
	codeForeign, bodyForeign := patch("ams-b")
	if codeUnknown != http.StatusBadRequest || codeForeign != http.StatusBadRequest {
		t.Fatalf("unknown agent: %d %s; another customer's agent: %d %s", codeUnknown, bodyUnknown, codeForeign, bodyForeign)
	}
	for _, body := range []string{bodyUnknown, bodyForeign} {
		if strings.Contains(body, "enrolled agents are") || (strings.Contains(body, "local") && strings.Contains(body, "ams-b")) {
			t.Errorf("the refusal lists agents: %s", body)
		}
	}
	// An agent that exists and one that does not read the same except for the
	// name the caller typed themselves.
	if strings.Replace(bodyForeign, "ams-b", "X", 1) != strings.Replace(bodyUnknown, "no-such-agent", "X", 1) {
		t.Errorf("an existing agent is refused differently from a missing one:\n%s\n%s", bodyForeign, bodyUnknown)
	}

	// The control: an agent that already measures something they can see.
	if code, body := patch("local"); code != http.StatusOK {
		t.Errorf("naming an agent in their scope = %d %s", code, body)
	}

	// And an admin is still told what exists when they get it wrong.
	admin := New(f.st, Options{}, fstest.MapFS{})
	code, body := call(t, admin, "PATCH", "/api/v1/targets/"+num(f.hostA), map[string]any{"settings": map[string]any{"agents": "no-such-agent"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "enrolled agents are") {
		t.Errorf("an admin's refusal no longer lists the enrolled agents: %d %s", code, body)
	}
	if code, body := call(t, admin, "PATCH", "/api/v1/targets/"+num(f.hostA), map[string]any{"settings": map[string]any{"agents": "ams-b"}}); code != http.StatusOK {
		t.Errorf("an admin naming any enrolled agent = %d %s", code, body)
	}
}

// The check applies to what a request changes. A node can carry an agent its
// editor is not offered, because an admin gave it one that no visible target
// uses; changing the title of that node must not be refused for it.
func TestEditorCanEditANodeWhoseAgentIsNotTheirs(t *testing.T) {
	f := tenants(t)
	ctx := context.Background()
	all, err := f.st.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		switch all[i].ID {
		case f.groupA:
			all[i].Settings.Agents = ptr("ams-b") // set by an admin, used by no visible target
		case f.hostA:
			all[i].Settings.Agents = ptr("local")
		default:
			continue
		}
		if err := f.st.UpsertTarget(ctx, &all[i]); err != nil {
			t.Fatal(err)
		}
	}
	h := f.server(t, "team-a", f.groupA, "editor")
	if code, body := call(t, h, "PATCH", "/api/v1/targets/"+num(f.groupA), map[string]any{"title": "Gemeente A"}); code != http.StatusOK {
		t.Errorf("changing a title was refused because of an agent it did not touch: %d %s", code, body)
	}
	// Touching the agents field is a different matter.
	if code, _ := call(t, h, "PATCH", "/api/v1/targets/"+num(f.groupA), map[string]any{"settings": map[string]any{"agents": "ams-b"}}); code != http.StatusBadRequest {
		t.Errorf("naming an agent that is not theirs, on a node that happens to carry it, = %d, want 400", code)
	}
}

// Creating a target goes through the same check as changing one.
func TestEditorCannotCreateATargetOnAnAgentOutsideTheirScope(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "editor")
	create := func(agents string) int {
		code, _ := call(t, h, "POST", "/api/v1/targets", map[string]any{
			"parent_id": f.groupA, "name": "new-" + agents, "host": "198.51.100.9", "address_family": "v4",
			"settings": map[string]any{"agents": agents},
		})
		return code
	}
	if code := create("ams-b"); code != http.StatusBadRequest {
		t.Errorf("creating a target on another customer's agent = %d, want 400", code)
	}
	if code := create("local"); code != http.StatusCreated {
		t.Errorf("creating a target on an agent in their scope = %d, want 201", code)
	}
}

// A firing alert can outlive its target: the manager keeps the state until the
// next reload. With the target gone there is no path to render as the caller
// sees it, and the path the manager recorded runs from the real root. Falling
// back to it would put /Klanten in the answer exactly when nothing else could.
func TestFiringAlertForAVanishedTargetDoesNotFallBackToTheRecordedPath(t *testing.T) {
	f := tenants(t)
	rule := alert.Rule{ID: f.ruleA, TargetID: f.groupA, Name: "shape"}
	gone := alert.Alert{Rule: &rule, TargetID: 99999, TargetPath: "/Klanten/GemeenteA/gone", Firing: true, Since: time.Now()}
	g := store.Grant{Group: "team-a", TargetID: f.groupA, Role: "viewer"}
	if err := f.st.UpsertGrant(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	sess := &auth.Session{Subject: "u", Role: auth.RoleViewer, Groups: []string{"team-a"}, Expires: 1 << 40}
	h := New(f.st, Options{Auth: &fakeAuth{session: sess}, DefaultRole: auth.RoleNone,
		Alerts: firingOverride{Manager: f.mgr, fire: []alert.Alert{gone}}}, fstest.MapFS{})
	code, body := call(t, h, "GET", "/api/v1/alerts", nil)
	if code != http.StatusOK || strings.Contains(body, "Klanten") {
		t.Errorf("%d %s", code, body)
	}

	// An admin may see all of it, so for them the recorded path is a fine fallback.
	admin := New(f.st, Options{Alerts: firingOverride{Manager: f.mgr, fire: []alert.Alert{gone}}}, fstest.MapFS{})
	if _, body := call(t, admin, "GET", "/api/v1/alerts", nil); !strings.Contains(body, "/Klanten/GemeenteA/gone") {
		t.Errorf("an admin lost the recorded path of an alert whose target is gone: %s", body)
	}
}

// The strongest statement of "no oracle" there is: for every route that takes
// an id, a foreign id has to be answered exactly as an id that does not exist,
// status and body. A caller can then learn nothing about the other tenant by
// trying numbers, which is how the rule endpoints leaked: a missing rule said
// "not found" and another customer's said something else.
//
// The route-coverage test checks that routes are classified, not that they
// enforce anything, and a per-handler test only covers the handlers someone
// remembered. This puts a foreign id into every id-bearing field of every
// scoped route that has one, in the path, the query and the body.
func TestAForeignIdIsAnsweredExactlyLikeAMissingOne(t *testing.T) {
	f := tenants(t)
	ctx := context.Background()
	now := time.Now().Unix()
	sB, err := f.mgr.AddSilence(ctx, alert.Silence{TargetID: &f.groupB, StartsAt: now, EndsAt: now + 3600, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	const missing = int64(99999)

	type route struct {
		name           string
		method, path   string
		body           func(id int64) any
		foreign, other int64 // the other tenant's id, and a nonexistent one
		// ownObject marks a route whose subject is the caller's own object and
		// whose foreign id is only where it is being moved to. A viewer is
		// refused for the first, before the second is looked at, and that is
		// 403: it is about something they can see.
		ownObject bool
	}
	targetPath := func(format string) func(int64) string {
		return func(id int64) string { return strings.Replace(format, "%d", num(id), 1) }
	}
	_ = targetPath
	routes := []route{
		{"measurements", "GET", "/api/v1/measurements?target_id=%d&agent_id=0&from=0&to=100", nil, f.hostB, missing, false},
		{"paths", "GET", "/api/v1/paths?target_id=%d", nil, f.hostB, missing, false},
		{"availability", "GET", "/api/v1/availability?target_id=%d", nil, f.hostB, missing, false},
		{"shape-reference target", "GET", "/api/v1/shape-reference?rule_id=" + num(f.ruleA) + "&target_id=%d", nil, f.hostB, missing, false},
		{"patch target", "PATCH", "/api/v1/targets/%d", func(int64) any { return map[string]any{"title": "x"} }, f.hostB, missing, false},
		{"delete target", "DELETE", "/api/v1/targets/%d", nil, f.hostB, missing, false},
		{"create under", "POST", "/api/v1/targets", func(id int64) any {
			return map[string]any{"parent_id": id, "name": "n", "host": "198.51.100.9", "address_family": "v4"}
		}, f.groupB, missing, false},
		{"move into", "PATCH", "/api/v1/targets/" + num(f.hostA), func(id int64) any { return map[string]any{"parent_id": id} }, f.groupB, missing, true},
		{"rule on", "POST", "/api/v1/alert-rules", func(id int64) any {
			return map[string]any{"target_id": id, "name": "r", "metric": "loss", "op": ">", "threshold": 1}
		}, f.groupB, missing, false},
		{"move rule to", "PATCH", "/api/v1/alert-rules/" + num(f.ruleA), func(id int64) any { return map[string]any{"target_id": id} }, f.groupB, missing, true},
		{"patch rule", "PATCH", "/api/v1/alert-rules/%d", func(int64) any { return map[string]any{"threshold": 5} }, f.ruleB, missing, false},
		{"delete rule", "DELETE", "/api/v1/alert-rules/%d", nil, f.ruleB, missing, false},
		{"capture", "POST", "/api/v1/alert-rules/%d/baseline", func(int64) any { return map[string]any{"target_id": f.hostA, "agent_id": 0} }, f.ruleB, missing, false},
		{"clear baseline", "DELETE", "/api/v1/alert-rules/%d/baseline", nil, f.ruleB, missing, false},
		{"ack", "POST", "/api/v1/alerts/ack", func(id int64) any { return map[string]any{"rule_id": f.ruleA, "target_id": id, "agent_id": 0} }, f.hostB, missing, false},
		{"silence on", "POST", "/api/v1/silences", func(id int64) any { return map[string]any{"target_id": id, "duration_s": 60} }, f.groupB, missing, false},
		{"delete silence", "DELETE", "/api/v1/silences/%d", nil, sB.ID, missing, false},
	}
	for _, role := range []string{"viewer", "editor"} {
		h := f.server(t, "team-a-"+role, f.groupA, role)
		for _, rt := range routes {
			ask := func(id int64) (int, string) {
				var body any
				if rt.body != nil {
					body = rt.body(id)
				}
				return call(t, h, rt.method, strings.Replace(rt.path, "%d", num(id), 1), body)
			}
			cf, bf := ask(rt.foreign)
			cm, bm := ask(rt.other)
			if cf != cm || bf != bm {
				t.Errorf("%s %s: a foreign id answers %d %s, a missing one %d %s", role, rt.name, cf, bf, cm, bm)
			}
			// Absent, not forbidden. Equal to a missing id's answer is not enough on
			// its own: a status that is wrong for both (a 403 for each) says the
			// same thing about each, and confirms that something is there to forbid.
			want := http.StatusNotFound
			if rt.ownObject && role == "viewer" {
				want = http.StatusForbidden
			}
			if cf != want {
				t.Errorf("%s %s: a foreign id answered %d %s, want %d", role, rt.name, cf, bf, want)
			}
		}
	}

	// Nothing of the other customer's changed in all that.
	all, err := f.st.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seenB bool
	for _, n := range all {
		if n.ID == f.hostB {
			seenB = true
			if n.Title != nil {
				t.Errorf("the other customer's target was retitled to %q", *n.Title)
			}
		}
	}
	if !seenB {
		t.Error("the other customer's target is gone")
	}
}

// Every scoped caller is shown the silences that cover the whole tree, by
// design, so one they try to delete is refused rather than answered as absent:
// "no such silence" would be untrue of something on their own screen.
func TestAGlobalSilenceIsRefusedNotHidden(t *testing.T) {
	f := tenants(t)
	now := time.Now().Unix()
	global, err := f.mgr.AddSilence(context.Background(), alert.Silence{StartsAt: now, EndsAt: now + 3600, CreatedAt: now, Reason: "all hands"})
	if err != nil {
		t.Fatal(err)
	}
	h := f.server(t, "team-a", f.groupA, "editor")
	if _, body := call(t, h, "GET", "/api/v1/silences", nil); !strings.Contains(body, "all hands") {
		t.Fatalf("a scoped caller is no longer shown the global silence: %s", body)
	}
	if code, body := call(t, h, "DELETE", "/api/v1/silences/"+num(global.ID), nil); code != http.StatusForbidden {
		t.Errorf("deleting a global silence = %d %s, want 403", code, body)
	}
}
