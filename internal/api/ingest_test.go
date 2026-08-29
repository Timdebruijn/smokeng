package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timdebruijn/smokeng/internal/ingest"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

func p[T any](v T) *T { return &v }

// ingestFixture builds a master with one agent enrolled and two targets: one
// assigned to that agent, one not.
func ingestFixture(t *testing.T) (http.Handler, *store.SQLite, int64, ed25519.PrivateKey, int64, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.AddAgent(ctx, "ams", pub)
	if err != nil {
		t.Fatal(err)
	}

	mine := tree.Target{ParentID: p(int64(1)), Name: "mine", Enabled: true,
		Host: p("10.0.0.1"), AddressFamily: p("v4"),
		Settings: tree.Settings{Agents: p("ams")}}
	theirs := tree.Target{ParentID: p(int64(1)), Name: "theirs", Enabled: true,
		Host: p("10.0.0.2"), AddressFamily: p("v4"),
		Settings: tree.Settings{Agents: p("local")}}
	for _, n := range []*tree.Target{&mine, &theirs} {
		if err := st.UpsertTarget(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	return New(st, Options{}, fstest.MapFS{}), st, rec.ID, priv, mine.ID, theirs.ID
}

func submit(t *testing.T, h http.Handler, id int64, key ed25519.PrivateKey,
	ms []store.Measurement, now time.Time) *httptest.ResponseRecorder {
	t.Helper()
	body, err := ingest.EncodeBatch(ms)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/ingest", bytes.NewReader(body))
	if err := ingest.Sign(req, id, key, body, now); err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func measurement(targetID, ts int64) store.Measurement {
	return store.Measurement{
		TargetID: targetID, TS: ts, Sent: 5, Received: 3,
		Samples: []uint32{1000, 2000, 3000},
	}
}

// The whole path: an enrolled agent signs an Arrow batch, the master verifies
// it, and the measurements land against that agent's series.
func TestIngestStoresMeasurements(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	now := time.Now()

	rec := submit(t, h, agentID, key, []store.Measurement{
		measurement(mine, 1_756_400_000), measurement(mine, 1_756_400_060),
	}, now)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
	}

	got, err := st.QueryRange(t.Context(), mine, agentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("stored %d measurements, want 2", len(got))
	}
	if got[0].Sent != 5 || got[0].Received != 3 || len(got[0].Samples) != 3 {
		t.Errorf("measurement did not survive the round trip: %+v", got[0])
	}
	// The agent id comes from the signature, not the payload.
	if got[0].AgentID != agentID {
		t.Errorf("agent id = %d, want %d", got[0].AgentID, agentID)
	}

	// last_seen is recorded, so a silent agent is distinguishable.
	agents, err := st.ListAgents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.ID == agentID && a.LastSeen == 0 {
			t.Error("last_seen was not recorded")
		}
	}
}

// Idempotency is the real replay defense: a batch delivered twice must leave
// exactly the same rows, not duplicates.
func TestIngestIsIdempotent(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	batch := []store.Measurement{measurement(mine, 1_756_400_000)}

	for range 3 {
		// A fresh nonce each time, as a genuine retry would have.
		if rec := submit(t, h, agentID, key, batch, time.Now()); rec.Code != http.StatusOK {
			t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
		}
	}
	got, err := st.QueryRange(t.Context(), mine, agentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d rows after three identical submissions, want 1", len(got))
	}
}

// An agent must not be able to write to a series it was not given. Without
// this check any enrolled agent could overwrite any target's history.
//
// The batch is still accepted, and the rest of it is written. Refusing the
// whole submission wedged the agent's outbox: it keeps a batch buffered until
// the master confirms it and retries oldest first, so one measurement for a
// target that had since been unassigned stopped that agent delivering anything
// ever again. What must hold is that the row is not written, not that the
// request fails.
func TestIngestDiscardsUnassignedTargetsButKeepsTheRest(t *testing.T) {
	h, st, agentID, key, mine, theirs := ingestFixture(t)

	rec := submit(t, h, agentID, key, []store.Measurement{
		measurement(theirs, 1_756_400_000),
		measurement(mine, 1_756_400_060),
	}, time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("a batch with one unassigned target = %d, want it accepted: %s", rec.Code, rec.Body)
	}
	got, err := st.QueryRange(t.Context(), theirs, agentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("wrote %d rows for an unassigned target", len(got))
	}
	// …and the assigned one in the same batch still landed, which is the
	// property that keeps the outbox draining.
	got, err = st.QueryRange(t.Context(), mine, agentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("stored %d rows for the assigned target, want 1", len(got))
	}
}

func TestIngestRejectsUnsignedAndDisabled(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	batch := []store.Measurement{measurement(mine, 1_756_400_000)}
	body, err := ingest.EncodeBatch(batch)
	if err != nil {
		t.Fatal(err)
	}

	// No signature at all.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/ingest", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned ingest = %d, want 401", rec.Code)
	}

	// Correctly signed, but the agent has been disabled.
	if err := st.SetAgentEnabled(t.Context(), agentID, false); err != nil {
		t.Fatal(err)
	}
	if rec := submit(t, h, agentID, key, batch, time.Now()); rec.Code != http.StatusUnauthorized {
		t.Errorf("disabled agent ingest = %d, want 401", rec.Code)
	}
}

// The assignment endpoint hands over resolved settings and nothing else, for
// this agent's targets only.
func TestAgentTargetsAreScopedAndResolved(t *testing.T) {
	h, _, agentID, key, mine, _ := ingestFixture(t)

	req := httptest.NewRequest("GET", "/api/v1/agent/targets", nil)
	if err := ingest.Sign(req, agentID, key, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent targets = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		Targets []map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Targets) != 1 {
		t.Fatalf("got %d assignments, want only the one assigned to this agent", len(body.Targets))
	}
	got := body.Targets[0]
	if int64(got["target_id"].(float64)) != mine {
		t.Errorf("assignment is for target %v, want %d", got["target_id"], mine)
	}
	// Settings arrive resolved: an agent must never have to re-derive them.
	for _, key := range []string{"interval_s", "pings", "probe_mode", "timeout_ms", "packet_size"} {
		if got[key] == nil {
			t.Errorf("assignment is missing resolved setting %q", key)
		}
	}
}
