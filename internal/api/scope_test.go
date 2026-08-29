package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// twoCustomers builds a tree with two customers who must never learn about
// each other, and returns the store plus the node ids.
type fixture struct {
	st           *store.SQLite
	groupA, hstA int64
	groupB, hstB int64
}

func twoCustomers(t *testing.T) fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	root := int64(1)

	mk := func(parent int64, name string, host *string) int64 {
		n := tree.Target{ParentID: &parent, Name: name, Enabled: true}
		if host != nil {
			n.Host = host
			n.AddressFamily = ptr("v4")
		}
		if err := st.UpsertTarget(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	f := fixture{st: st}
	f.groupA = mk(root, "GemeenteA", nil)
	f.hstA = mk(f.groupA, "gw", ptr("198.51.100.1"))
	f.groupB = mk(root, "GemeenteB", nil)
	f.hstB = mk(f.groupB, "gw", ptr("198.51.100.2"))

	// A rule on each, so the rule list is a disclosure route of its own.
	for _, id := range []int64{f.hstA, f.hstB} {
		rule := alert.Rule{
			TargetID: id, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
			Threshold: 20, For: 3, ClearFor: 3, Enabled: true,
		}
		if err := st.UpsertAlertRule(ctx, &rule); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// scopedServer grants the group a role on a node and signs the caller in as a
// member of it, with no global role at all.
func scopedServer(t *testing.T, f fixture, group string, node int64, role string) http.Handler {
	t.Helper()
	g := store.Grant{Group: group, TargetID: node, Role: role}
	if err := f.st.UpsertGrant(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	sess := &auth.Session{
		Subject: "u", Role: auth.RoleViewer, Groups: []string{group}, Expires: 1 << 40,
	}
	return New(f.st, Options{
		Auth:        &fakeAuth{session: sess},
		DefaultRole: auth.RoleNone,
	}, fstest.MapFS{})
}

func getJSON(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// The requirement, stated as a test: one customer must not learn that the
// other is a customer. Not their nodes, not their names, not that there are
// any.
func TestScopeHidesEverythingOutsideIt(t *testing.T) {
	f := twoCustomers(t)
	h := scopedServer(t, f, "team-a", f.groupA, "viewer")

	code, body := getJSON(t, h, "/api/v1/targets")
	if code != http.StatusOK {
		t.Fatalf("GET targets = %d", code)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "GemeenteB") {
		t.Errorf("the other customer's name is in the response:\n%s", raw)
	}
	list, _ := body["targets"].([]any)
	if len(list) != 2 {
		t.Errorf("got %d targets, want only the granted subtree", len(list))
	}
	seen := map[string]bool{}
	for _, item := range list {
		m := item.(map[string]any)
		seen[m["path"].(string)] = true
	}
	// Paths are rendered relative to the grant, so the subtree reads as though
	// it were the whole installation.
	for _, want := range []string{"/GemeenteA", "/GemeenteA/gw"} {
		if !seen[want] {
			t.Errorf("path %q missing; got %v", want, seen)
		}
	}
}

// Reading someone else's measurements must not merely be refused: a refusal
// distinguishable from absence confirms the target exists.
func TestScopeAnswersOutsideTargetsAsAbsent(t *testing.T) {
	f := twoCustomers(t)
	h := scopedServer(t, f, "team-a", f.groupA, "viewer")

	for _, path := range []string{
		"/api/v1/measurements?target_id=", "/api/v1/paths?target_id=",
	} {
		mine, _ := getJSON(t, h, path+itoa(f.hstA))
		if mine == http.StatusNotFound {
			t.Errorf("%s on my own target = 404", path)
		}
		theirs, _ := getJSON(t, h, path+itoa(f.hstB))
		if theirs != http.StatusNotFound {
			t.Errorf("%s on another customer's target = %d, want 404", path, theirs)
		}
	}
}

// A rule names the node it is defined on, so an unfiltered rule list would
// enumerate the tree for anyone allowed to read any of it.
func TestScopeFiltersAlertRules(t *testing.T) {
	f := twoCustomers(t)
	h := scopedServer(t, f, "team-a", f.groupA, "viewer")

	_, body := getJSON(t, h, "/api/v1/alert-rules")
	rules, _ := body["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want only my own", len(rules))
	}
	if id := int64(rules[0].(map[string]any)["target_id"].(float64)); id != f.hstA {
		t.Errorf("rule belongs to target %d, want %d", id, f.hstA)
	}
}

// A viewer grant reads and does not write; an editor grant writes, but only
// inside its own subtree.
func TestGrantRolesBoundWrites(t *testing.T) {
	write := func(h http.Handler, method, path, body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	f := twoCustomers(t)
	viewer := scopedServer(t, f, "team-a", f.groupA, "viewer")
	if code := write(viewer, "PATCH", "/api/v1/targets/"+itoa(f.hstA), `{"title":"x"}`); code != http.StatusForbidden {
		t.Errorf("viewer grant wrote its own subtree: %d", code)
	}

	g := twoCustomers(t)
	editor := scopedServer(t, g, "team-a", g.groupA, "editor")
	if code := write(editor, "PATCH", "/api/v1/targets/"+itoa(g.hstA), `{"title":"x"}`); code != http.StatusOK {
		t.Errorf("editor grant could not write its own subtree: %d", code)
	}
	// …and the other customer is still absent rather than forbidden.
	if code := write(editor, "PATCH", "/api/v1/targets/"+itoa(g.hstB), `{"title":"x"}`); code != http.StatusNotFound {
		t.Errorf("editor wrote another customer's target: %d", code)
	}
	// A move out of the scope is a write to the destination, and is refused
	// there even though the node itself is writable.
	if code := write(editor, "PATCH", "/api/v1/targets/"+itoa(g.hstA),
		`{"parent_id":`+itoa(g.groupB)+`}`); code != http.StatusNotFound {
		t.Errorf("editor moved a target out of its scope: %d", code)
	}
	// Creating under someone else's node is refused for the same reason.
	if code := write(editor, "POST", "/api/v1/targets",
		`{"parent_id":`+itoa(g.groupB)+`,"name":"sneaky"}`); code != http.StatusNotFound {
		t.Errorf("editor created a node outside its scope: %d", code)
	}
}

// What a grant never confers, however wide it is.
func TestGrantsNeverReachGlobalAdministration(t *testing.T) {
	f := twoCustomers(t)
	h := scopedServer(t, f, "team-a", f.groupA, "editor")

	for _, path := range []string{"/metrics", "/api/v1/agent-tokens", "/api/v1/grants"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with an editor grant = %d, want 403", path, rec.Code)
		}
	}
}

// The migration rule: adding the first grant must not silently lock out
// everyone who could already read. What a caller with no grant gets is a
// setting, not a consequence.
func TestDefaultRoleGovernsUngrantedCallers(t *testing.T) {
	f := twoCustomers(t)
	sess := &auth.Session{Subject: "u", Role: auth.RoleViewer, Expires: 1 << 40}

	asViewer := New(f.st, Options{Auth: &fakeAuth{session: sess}}, fstest.MapFS{})
	code, body := getJSON(t, asViewer, "/api/v1/targets")
	list, _ := body["targets"].([]any)
	if code != http.StatusOK || len(list) < 4 {
		t.Errorf("with the default settings an authenticated user saw %d targets, want the whole tree", len(list))
	}

	asNobody := New(f.st, Options{
		Auth: &fakeAuth{session: sess}, DefaultRole: auth.RoleNone,
	}, fstest.MapFS{})
	_, body = getJSON(t, asNobody, "/api/v1/targets")
	list, _ = body["targets"].([]any)
	if len(list) != 0 {
		t.Errorf("with --default-role none an ungranted user saw %d targets, want none", len(list))
	}
}
