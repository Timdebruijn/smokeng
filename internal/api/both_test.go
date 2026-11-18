package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/ingest"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Protects the rule that "both" needs a hostname, at both doors of the targets
// API: a literal address has one family, so a series of the other could only
// ever be no data. The refusal is a 400 that says why and what to do, on create
// and on patch (a v4 target that has a literal host cannot be turned into
// "both" by changing only its family).
func TestBothWithALiteralHostIsRefusedWithAHint(t *testing.T) {
	h, _ := newTestServer(t)
	for _, host := range []string{"192.0.2.1", "2001:db8::1", "fe80::1%eth0"} {
		code, body := do(t, h, "POST", "/api/v1/targets", map[string]any{
			"parent_id": 1, "name": "lit", "host": host, "address_family": "both"})
		msg, _ := body["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(msg, "hostname") || !strings.Contains(msg, "v4 or v6") {
			t.Errorf("create with literal %s = %d %v, want 400 naming the hostname rule and the alternatives", host, code, body)
		}
	}
	code, created := do(t, h, "POST", "/api/v1/targets", map[string]any{
		"parent_id": 1, "name": "v4", "host": "192.0.2.1", "address_family": "v4"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create v4 = %d %v", code, created)
	}
	path := "/api/v1/targets/" + itoa(int64(created["id"].(float64)))
	code, body := do(t, h, "PATCH", path, map[string]any{"address_family": "both"})
	if msg, _ := body["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "hostname") {
		t.Errorf("patch a literal-host target to both = %d %v, want 400 with the hint", code, body)
	}
	// With a hostname it is accepted and read back as it was written.
	code, body = do(t, h, "PATCH", path, map[string]any{"host": "web.example.net", "address_family": "both"})
	if code != http.StatusOK || body["address_family"] != "both" {
		t.Errorf("patch to a hostname and both = %d %v", code, body)
	}
}

func TestBothWithAHostnameRoundTrips(t *testing.T) {
	h, _ := newTestServer(t)
	code, created := do(t, h, "POST", "/api/v1/targets", map[string]any{
		"parent_id": 1, "name": "dual", "host": "dual.example.net", "address_family": "both"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create both = %d %v", code, created)
	}
	code, list := do(t, h, "GET", "/api/v1/targets", nil)
	found := false
	for _, n := range list["targets"].([]any) {
		if m := n.(map[string]any); m["id"] == created["id"] {
			found = true
			if m["address_family"] != "both" {
				t.Errorf("read back address_family = %v, want both", m["address_family"])
			}
		}
	}
	if code != http.StatusOK || !found {
		t.Errorf("list = %d, target found %v", code, found)
	}
}

// Protects the agents against a value they cannot read. An agent of any
// released version refuses an address_family it does not know and then drops
// its whole target sync, so a target measured over both is sent as v4, the one
// family an agent measures of it.
func TestAgentAssignmentSendsABothTargetAsV4(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	tg := tree.Target{ID: mine, ParentID: new(int64(1)), Name: "mine", Enabled: true,
		Host: new("mine.example.net"), AddressFamily: new("both"), Settings: tree.Settings{Agents: new("ams")}}
	if err := st.UpsertTarget(t.Context(), &tg); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/agent/targets", nil)
	if err := ingest.Sign(req, agentID, key, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body struct {
		Targets []map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Targets) != 1 {
		t.Fatalf("assignments: %v, %s", err, rec.Body)
	}
	if got := body.Targets[0]["address_family"]; got != "v4" {
		t.Errorf("address_family = %v, want v4", got)
	}
}
