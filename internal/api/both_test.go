package api

import (
	"crypto/ed25519"
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

// assignedFamilies asks for the agent's targets, sending the given
// X-Agent-Features headers (none when empty), and returns each target's
// address_family by id.
func assignedFamilies(t *testing.T, h http.Handler, id int64, key ed25519.PrivateKey, features ...string) map[int64]string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/agent/targets", nil)
	if err := ingest.Sign(req, id, key, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, f := range features {
		req.Header.Add("X-Agent-Features", f)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// The answer depends on a request header, so nothing between the master and
	// an agent may keep it and hand it to another agent: an agent that cannot
	// measure both and gets "both" loses its whole target sync. The
	// response was already marked no-store; this pins it now that a kept copy
	// would break something.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("assignments response: Cache-Control %q, want no-store", cc)
	}
	var body struct {
		Targets []struct {
			ID     int64  `json:"target_id"`
			Family string `json:"address_family"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("assignments: %v, %s", err, rec.Body)
	}
	out := map[int64]string{}
	for _, tg := range body.Targets {
		out[tg.ID] = tg.Family
	}
	return out
}

// Protects the agents against a value they cannot read, and the agents that
// can read it. An agent of an older version refuses an address_family it does
// not know and then drops its whole target sync, so a target measured over
// both is sent as both only to an agent that listed "both" in
// X-Agent-Features (whatever else the list holds, in any order, with spaces),
// and as v4 to every other. A v4 or v6 target is never affected by the header.
func TestAgentAssignmentSendsBothOnlyToAnAgentThatSaidItCan(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	tg := tree.Target{ID: mine, ParentID: new(int64(1)), Name: "mine", Enabled: true,
		Host: new("mine.example.net"), AddressFamily: new("both"), Settings: tree.Settings{Agents: new("ams")}}
	v6 := tree.Target{ParentID: new(int64(1)), Name: "six", Enabled: true,
		Host: new("2001:db8::1"), AddressFamily: new("v6"), Settings: tree.Settings{Agents: new("ams")}}
	v4 := tree.Target{ParentID: new(int64(1)), Name: "four", Enabled: true,
		Host: new("192.0.2.1"), AddressFamily: new("v4"), Settings: tree.Settings{Agents: new("ams")}}
	for _, n := range []*tree.Target{&tg, &v6, &v4} {
		if err := st.UpsertTarget(t.Context(), n); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name     string
		features []string
		both     string
	}{
		{"no header", nil, "v4"},
		{"both", []string{"both"}, "both"},
		{"both among other words, spaced, any order", []string{"future-thing , both ,other"}, "both"},
		{"both in a second header line", []string{"other", "both"}, "both"},
		{"unknown words only", []string{"future-thing,other"}, "v4"},
		{"a word that merely contains both", []string{"bothways,notboth"}, "v4"},
		{"empty header", []string{""}, "v4"},
	} {
		got := assignedFamilies(t, h, agentID, key, c.features...)
		if got[tg.ID] != c.both || got[v6.ID] != "v6" || got[v4.ID] != "v4" || len(got) != 3 {
			t.Errorf("%s: sent %v, want the both target as %s and the v6 and v4 targets unchanged", c.name, got, c.both)
		}
	}
}
