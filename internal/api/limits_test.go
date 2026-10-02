package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Settings that nothing bounded: one request could schedule tens of thousands
// of probes a second at a third party, or hold millions of sockets open on a
// timeout. They are refused where a target is written, for an editor and for an
// admin alike.
func TestWritingATargetIsHeldToTheLimits(t *testing.T) {
	f := tenants(t)
	editor := f.server(t, "team-a", f.groupA, "editor")
	admin := New(f.st, Options{}, fstest.MapFS{})

	create := func(h http.Handler, name string, extra map[string]any) (int, string) {
		body := map[string]any{"parent_id": f.groupA, "name": name, "host": "198.51.100.9", "address_family": "v4"}
		for k, v := range extra {
			body[k] = v
		}
		return call(t, h, "POST", "/api/v1/targets", body)
	}
	settings := func(k string, v any) map[string]any { return map[string]any{"settings": map[string]any{k: v}} }

	for name, c := range map[string]struct {
		extra map[string]any
		want  string
	}{
		"65535 pings":             {settings("pings_per_interval", 65535), "pings_per_interval"},
		"a flood of pings":        {map[string]any{"settings": map[string]any{"pings_per_interval": 1000, "interval_s": 1, "burst_gap_ms": 0}}, "a second"},
		"a huge packet":           {settings("packet_size", 65000), "packet_size"},
		"a day-long timeout":      {settings("timeout_ms", 600000), "interval"},
		"an interval overflow":    {settings("interval_s", 9223372037), "interval_s"},
		"a trace overflow":        {settings("trace_interval_s", 18446744074), "trace_interval_s"},
		"retention of 1s":         {settings("retention_s", 1), "retention_s"},
		"a host with a path":      {map[string]any{"host": "intranet.corp/../x"}, "host"},
		"a host with userinfo":    {map[string]any{"host": "x@internal.corp"}, "host"},
		"a newline in a host":     {map[string]any{"host": "a\nb"}, "host"},
		"a name with an override": {map[string]any{"name": "a‮b"}, "name"},
		"a name with a newline":   {map[string]any{"name": "line\nbreak"}, "name"},
	} {
		for who, h := range map[string]http.Handler{"editor": editor, "admin": admin} {
			code, body := create(h, "t-"+strings.ReplaceAll(name, " ", "-"), c.extra)
			if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
				t.Errorf("%s, %s: %d %.200s, want 400 naming %q", who, name, code, body, c.want)
			}
		}
	}
	// The control: ordinary values still create.
	if code, body := create(editor, "ordinary", settings("pings_per_interval", 50)); code != http.StatusCreated {
		t.Errorf("an ordinary target = %d %s", code, body)
	}
}

// The limits apply to what a request changes. A node that predates one is not
// refused an unrelated edit for it, or tightening a limit would be an outage for
// whoever had configured past the new line.
func TestEditingANodeThatPredatesALimitIsNotRefusedForIt(t *testing.T) {
	f := tenants(t)
	ctx := context.Background()
	all, err := f.st.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if all[i].ID == f.hostA {
			all[i].Settings.PacketSize = ptr(20000) // past MaxPacketSize, stored before the limit existed
			all[i].Settings.PingsPerInterval = ptr(5)
			if err := f.st.UpsertTarget(ctx, &all[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := f.server(t, "team-a", f.groupA, "editor")
	patch := func(body map[string]any) (int, string) {
		return call(t, h, "PATCH", "/api/v1/targets/"+num(f.hostA), body)
	}
	if code, body := patch(map[string]any{"title": "retitled"}); code != http.StatusOK {
		t.Errorf("retitling a node with a legacy packet_size = %d %.200s", code, body)
	}
	if code, _ := patch(map[string]any{"settings": map[string]any{"packet_size": 30000}}); code != http.StatusBadRequest {
		t.Errorf("changing a legacy packet_size to another value over the limit = %d, want 400", code)
	}
	if code, body := patch(map[string]any{"settings": map[string]any{"packet_size": 1400}}); code != http.StatusOK {
		t.Errorf("bringing it inside the limit = %d %.200s", code, body)
	}
}

// A chain of nodes costs depth squared on every request that resolves one, so
// depth is bounded where it is created.
func TestTheTreeCannotBeNestedWithoutLimit(t *testing.T) {
	f := tenants(t)
	admin := New(f.st, Options{}, fstest.MapFS{})
	parent := f.groupA // /Klanten/GemeenteA is already at depth 2
	var last int
	for d := 3; d <= tree.MaxDepth+3; d++ {
		code, body := call(t, admin, "POST", "/api/v1/targets", map[string]any{"parent_id": parent, "name": "n" + num(int64(d))})
		last = code
		if code != http.StatusCreated {
			if d != tree.MaxDepth+1 {
				t.Fatalf("the refusal came at depth %d, want %d: %d %.200s", d, tree.MaxDepth+1, code, body)
			}
			if !strings.Contains(body, "levels deep") {
				t.Errorf("the refusal does not say why: %s", body)
			}
			return
		}
		// Fetch the new node's id as the next parent.
		_, listing := call(t, admin, "GET", "/api/v1/targets", nil)
		parent = idOfName(t, listing, "n"+num(int64(d)))
	}
	t.Fatalf("a chain %d deep was created (last status %d)", tree.MaxDepth+3, last)
}

func idOfName(t *testing.T, listing, name string) int64 {
	t.Helper()
	i := strings.Index(listing, `"name":"`+name+`"`)
	if i < 0 {
		t.Fatalf("%s not in the listing", name)
	}
	j := strings.LastIndex(listing[:i], `"id":`)
	var id int64
	if _, err := fmtSscan(listing[j+5:], &id); err != nil {
		t.Fatal(err)
	}
	return id
}

func fmtSscan(s string, id *int64) (int, error) {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		*id = *id*10 + int64(s[n]-'0')
		n++
	}
	return n, nil
}

// Each of these settings is valid on its own. What makes the leaf beneath the
// group a flood is the combination, which only exists after inheritance, so only
// a check on the resolved tree can refuse it.
func TestAGroupSettingThatPushesALeafOverALimitIsRefused(t *testing.T) {
	f := tenants(t)
	h := f.server(t, "team-a", f.groupA, "editor")
	code, body := call(t, h, "PATCH", "/api/v1/targets/"+num(f.groupA), map[string]any{
		"settings": map[string]any{"pings_per_interval": 1000, "interval_s": 1, "burst_gap_ms": 0},
	})
	if code != http.StatusBadRequest || !strings.Contains(body, "a second") {
		t.Errorf("a group setting that makes a leaf send 1000 pings a second = %d %.200s, want 400", code, body)
	}
	// The leaf itself is unchanged.
	_, listing := call(t, h, "GET", "/api/v1/targets", nil)
	if strings.Contains(listing, `"pings_per_interval":{"effective":1000`) {
		t.Errorf("the refused setting was stored: %.300s", listing)
	}
}

// Moving A under B and B under A are each fine on their own and a cycle
// together. Each request validated against the tree as it had read it, both
// passed, and both wrote, after which tree.New failed on every request until
// the database was edited by hand. The writes are serialised now, so the second
// validates against the result of the first: exactly one of the two succeeds.
//
// Many rounds, because a race is not guaranteed to show; with the writes
// unserialised this fails on the first rounds in practice.
func TestTwoMovesThatWouldMakeACycleCannotBothSucceed(t *testing.T) {
	f := tenants(t)
	admin := New(f.st, Options{}, fstest.MapFS{})
	mk := func(name string) int64 {
		code, body := call(t, admin, "POST", "/api/v1/targets", map[string]any{"parent_id": f.groupA, "name": name})
		if code != http.StatusCreated {
			t.Fatalf("%d %s", code, body)
		}
		return idOfName(t, func() string { _, l := call(t, admin, "GET", "/api/v1/targets", nil); return l }(), name)
	}
	for round := range 25 {
		a, b := mk("a"+num(int64(round))), mk("b"+num(int64(round)))
		start := make(chan struct{})
		res := make(chan int, 2)
		move := func(node, under int64) {
			<-start
			code, _ := call(t, admin, "PATCH", "/api/v1/targets/"+num(node), map[string]any{"parent_id": under})
			res <- code
		}
		go move(a, b)
		go move(b, a)
		close(start)
		ok := 0
		for range 2 {
			if <-res == http.StatusOK {
				ok++
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d of the two moves succeeded, want exactly one", round, ok)
		}
		// And the tree is still one the whole installation can read.
		all, err := f.st.ListTargets(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tree.New(all); err != nil {
			t.Fatalf("round %d left a tree that does not load: %v", round, err)
		}
	}
}

// Rules are defined on a node and go with it. A recursive delete of a subtree
// holding a rule used to fail part-way: children gone, the node refused by the
// foreign key, the handler answering 500, and nothing to say what was left.
func TestDeletingASubtreeWithRulesIsAllOrNothing(t *testing.T) {
	f := tenants(t) // GemeenteA carries a rule, and so does GemeenteB
	admin := New(f.st, Options{}, fstest.MapFS{})
	code, body := call(t, admin, "DELETE", "/api/v1/targets/"+num(f.groupA)+"?recursive=true", nil)
	if code != http.StatusOK {
		t.Fatalf("deleting a subtree with a rule = %d %s", code, body)
	}
	all, err := f.st.ListTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range all {
		if n.ID == f.groupA || n.ID == f.hostA {
			t.Errorf("target %d (%s) survived the delete", n.ID, n.Name)
		}
	}
	rules, err := f.st.ListAlertRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.TargetID == f.groupA {
			t.Errorf("rule %d outlived the node it was defined on", r.ID)
		}
	}
	// The other customer is untouched.
	var keptB bool
	for _, n := range all {
		if n.ID == f.hostB {
			keptB = true
		}
	}
	if !keptB {
		t.Error("the other customer's target went with it")
	}
}

// gatedStore holds a tree write until released, so a test can let something else
// change the tree between a request starting and its transaction beginning.
//
// It embeds the concrete store and not the Store interface. New asks the store
// whether it can also list grants, enrol agents and so on, by type assertion; an
// embedded interface hides those, the server then knows no grants at all, and
// every scoped caller is refused for that reason whatever else is true. An
// earlier version of the test below did exactly that and could not fail.
type gatedStore struct {
	*store.SQLite
	entered chan struct{}
	gate    chan struct{}
}

func (g gatedStore) ChangeTargets(ctx context.Context, fn func([]tree.Target) (store.TargetChange, error)) error {
	close(g.entered)
	<-g.gate
	return g.SQLite.ChangeTargets(ctx, fn)
}

// A request is authorised against the tree it is applied to, inside the
// transaction, not against one read before it. An editor's right to write a node
// is a fact about where the node sits when the write happens: here an admin moves
// it out of their grant after the request has started and before it is applied.
func TestAMoveIsAuthorisedAgainstTheTreeItIsAppliedTo(t *testing.T) {
	f := tenants(t)
	g := gatedStore{SQLite: f.st, entered: make(chan struct{}), gate: make(chan struct{})}
	g0 := store.Grant{Group: "team-a", TargetID: f.groupA, Role: "editor"}
	if err := f.st.UpsertGrant(context.Background(), &g0); err != nil {
		t.Fatal(err)
	}
	sess := &auth.Session{Subject: "u", Role: auth.RoleViewer, Groups: []string{"team-a"}, Expires: 1 << 40}
	editor := New(g, Options{Auth: &fakeAuth{session: sess}, DefaultRole: auth.RoleNone}, fstest.MapFS{})

	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() {
		code, body := call(t, editor, "PATCH", "/api/v1/targets/"+num(f.hostA), map[string]any{"title": "mine still?"})
		done <- result{code, body}
	}()
	<-g.entered // the request is in flight and has not touched the tree yet

	// Meanwhile an admin moves the node into the other customer's subtree.
	if err := f.st.ChangeTargets(context.Background(), func(cur []tree.Target) (store.TargetChange, error) {
		for i := range cur {
			if cur[i].ID == f.hostA {
				moved := cur[i]
				moved.ParentID = &f.groupB
				moved.Name = "gw-from-a"
				return store.TargetChange{Upsert: []*tree.Target{&moved}}, nil
			}
		}
		return store.TargetChange{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	close(g.gate)

	r := <-done
	t.Logf("the in-flight edit was answered %d %.160s", r.code, r.body)
	if r.code != http.StatusNotFound {
		t.Errorf("an edit to a node that left their grant while the request was in flight = %d %.200s, want 404", r.code, r.body)
	}
	all, _ := f.st.ListTargets(context.Background())
	for _, n := range all {
		if n.ID == f.hostA && n.Title != nil && *n.Title == "mine still?" {
			t.Error("the edit was applied to a node outside their grant")
		}
	}
}

// The control for the test above: through the same gated store, with nothing
// moved in between, the editor's edit goes through. Without it, a server that
// refused every scoped caller would pass the test above for the wrong reason.
func TestTheGatedServerAnswersAnEditorNormally(t *testing.T) {
	f := tenants(t)
	g := gatedStore{SQLite: f.st, entered: make(chan struct{}), gate: make(chan struct{})}
	grant := store.Grant{Group: "team-a", TargetID: f.groupA, Role: "editor"}
	if err := f.st.UpsertGrant(context.Background(), &grant); err != nil {
		t.Fatal(err)
	}
	sess := &auth.Session{Subject: "u", Role: auth.RoleViewer, Groups: []string{"team-a"}, Expires: 1 << 40}
	editor := New(g, Options{Auth: &fakeAuth{session: sess}, DefaultRole: auth.RoleNone}, fstest.MapFS{})
	done := make(chan int, 1)
	go func() {
		code, _ := call(t, editor, "PATCH", "/api/v1/targets/"+num(f.hostA), map[string]any{"title": "still mine"})
		done <- code
	}()
	<-g.entered
	close(g.gate)
	if code := <-done; code != http.StatusOK {
		t.Errorf("an edit within their grant, through the gated store = %d, want 200", code)
	}
}
