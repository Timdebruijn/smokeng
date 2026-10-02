package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

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
