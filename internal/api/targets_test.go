package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"

	"smokeng/internal/store"
)

func newTestServer(t *testing.T) (http.Handler, *store.SQLite) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, Options{}, fstest.MapFS{}), st
}

func do(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// setting digs out one {local, effective, source} object from a target body.
func setting(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	settings, ok := body["settings"].(map[string]any)
	if !ok {
		t.Fatalf("no settings in %v", body)
	}
	v, ok := settings[key].(map[string]any)
	if !ok {
		t.Fatalf("no setting %q in %v", key, settings)
	}
	return v
}

func TestCreateInheritsAndOverrides(t *testing.T) {
	h, _ := newTestServer(t)

	code, group := do(t, h, "POST", "/api/v1/targets", map[string]any{
		"parent_id": 1, "name": "Production",
		"settings": map[string]any{"pings_per_interval": 40},
	})
	if code != http.StatusCreated {
		t.Fatalf("create group: %d %v", code, group)
	}
	groupID := int64(group["id"].(float64))

	code, leaf := do(t, h, "POST", "/api/v1/targets", map[string]any{
		"parent_id": groupID, "name": "cf-v4",
		"host": "1.1.1.1", "address_family": "v4",
	})
	if code != http.StatusCreated {
		t.Fatalf("create leaf: %d %v", code, leaf)
	}
	leafID := int64(leaf["id"].(float64))
	if leaf["path"] != "/Production/cf-v4" {
		t.Errorf("path = %v", leaf["path"])
	}

	// Inherited from the group, and the provenance says so.
	pings := setting(t, leaf, "pings_per_interval")
	if pings["effective"].(float64) != 40 {
		t.Errorf("effective = %v, want 40", pings["effective"])
	}
	if pings["local"] != nil {
		t.Errorf("local = %v, want null", pings["local"])
	}
	src, ok := pings["source"].(map[string]any)
	if !ok || src["path"] != "/Production" {
		t.Errorf("source = %v, want the Production group", pings["source"])
	}

	// Override locally: source flips to "local".
	code, updated := do(t, h, "PATCH", "/api/v1/targets/"+itoa(leafID), map[string]any{
		"settings": map[string]any{"pings_per_interval": 5},
	})
	if code != http.StatusOK {
		t.Fatalf("override: %d %v", code, updated)
	}
	pings = setting(t, updated, "pings_per_interval")
	if pings["effective"].(float64) != 5 || pings["local"].(float64) != 5 || pings["source"] != "local" {
		t.Errorf("after override: %v", pings)
	}

	// Clearing to null reverts to inheritance — the "revert" button.
	code, reverted := do(t, h, "PATCH", "/api/v1/targets/"+itoa(leafID), map[string]any{
		"settings": map[string]any{"pings_per_interval": nil},
	})
	if code != http.StatusOK {
		t.Fatalf("revert: %d %v", code, reverted)
	}
	pings = setting(t, reverted, "pings_per_interval")
	if pings["effective"].(float64) != 40 || pings["local"] != nil {
		t.Errorf("after revert: %v", pings)
	}
	if src, ok := pings["source"].(map[string]any); !ok || src["path"] != "/Production" {
		t.Errorf("after revert source = %v", pings["source"])
	}
}

func TestValidationRejects(t *testing.T) {
	h, _ := newTestServer(t)
	do(t, h, "POST", "/api/v1/targets", map[string]any{"parent_id": 1, "name": "G"})

	cases := map[string]map[string]any{
		"host without family": {"parent_id": 1, "name": "a", "host": "1.1.1.1"},
		"bad family":          {"parent_id": 1, "name": "b", "host": "1.1.1.1", "address_family": "any"},
		"bad probe mode":      {"parent_id": 1, "name": "c", "settings": map[string]any{"probe_mode": "warp"}},
		"bad dscp":            {"parent_id": 1, "name": "d", "settings": map[string]any{"dscp": 99}},
		"zero interval":       {"parent_id": 1, "name": "e", "settings": map[string]any{"interval_s": 0}},
		"slash in name":       {"parent_id": 1, "name": "a/b"},
		"duplicate sibling":   {"parent_id": 1, "name": "G"},
		"unknown setting":     {"parent_id": 1, "name": "f", "settings": map[string]any{"nope": 1}},
		"missing parent":      {"name": "g"},
		"unknown parent":      {"parent_id": 9999, "name": "h"},
		// 20 pings 1s apart cannot fit inside a 10s interval.
		"burst overruns interval": {"parent_id": 1, "name": "i", "host": "1.1.1.1", "address_family": "v4",
			"settings": map[string]any{"interval_s": 10, "pings_per_interval": 20, "burst_gap_ms": 1000}},
	}
	for name, body := range cases {
		code, resp := do(t, h, "POST", "/api/v1/targets", body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400 (%v)", name, code, resp)
		}
	}
}

func TestDeleteRules(t *testing.T) {
	h, _ := newTestServer(t)
	_, group := do(t, h, "POST", "/api/v1/targets", map[string]any{"parent_id": 1, "name": "G"})
	groupID := int64(group["id"].(float64))
	_, child := do(t, h, "POST", "/api/v1/targets", map[string]any{
		"parent_id": groupID, "name": "c", "host": "1.1.1.1", "address_family": "v4",
	})
	childID := int64(child["id"].(float64))

	if code, _ := do(t, h, "DELETE", "/api/v1/targets/1", nil); code != http.StatusBadRequest {
		t.Errorf("deleting the root returned %d, want 400", code)
	}
	if code, _ := do(t, h, "DELETE", "/api/v1/targets/"+itoa(groupID), nil); code != http.StatusBadRequest {
		t.Errorf("deleting a group with children returned %d, want 400", code)
	}
	code, resp := do(t, h, "DELETE", "/api/v1/targets/"+itoa(groupID)+"?recursive=true", nil)
	if code != http.StatusOK {
		t.Fatalf("recursive delete: %d %v", code, resp)
	}
	deleted, _ := resp["deleted"].([]any)
	if len(deleted) != 2 || int64(deleted[0].(float64)) != childID {
		t.Errorf("deleted = %v, want the child before its parent", deleted)
	}
	if code, _ := do(t, h, "DELETE", "/api/v1/targets/"+itoa(groupID), nil); code != http.StatusNotFound {
		t.Errorf("deleting a gone target returned %d, want 404", code)
	}
}

// Measurements outlive the target row they came from; deleting a target must
// never silently destroy history.
func TestDeleteKeepsMeasurements(t *testing.T) {
	h, st := newTestServer(t)
	_, leaf := do(t, h, "POST", "/api/v1/targets", map[string]any{
		"parent_id": 1, "name": "c", "host": "1.1.1.1", "address_family": "v4",
	})
	id := int64(leaf["id"].(float64))
	m := store.Measurement{TargetID: id, TS: 1_756_400_000, Sent: 3, Received: 2,
		Samples: []uint32{1000, 2000}}
	if err := st.WriteMeasurements(t.Context(), []store.Measurement{m}); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, h, "DELETE", "/api/v1/targets/"+itoa(id), nil); code != http.StatusOK {
		t.Fatal("delete failed")
	}
	got, err := st.QueryRange(t.Context(), id, store.LocalAgentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("measurements after target delete = %d, want 1 (history is kept)", len(got))
	}
}

// The tree changes under the caller's feet, so responses must not be cached.
// Without this the admin UI shows state that no longer exists — a browser is
// free to serve a heuristically cached copy of a response with no directives.
func TestResponsesAreNotCacheable(t *testing.T) {
	h, _ := newTestServer(t)
	for _, path := range []string{"/api/v1/targets", "/api/v1/measurements?target_id=1"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
