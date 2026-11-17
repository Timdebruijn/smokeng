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

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/ingest"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

func p[T any](v T) *T { return &v }

// ingestFixture builds a master with one agent enrolled and two targets: one
// assigned to that agent, one not.
func ingestFixture(t *testing.T) (http.Handler, *store.SQLite, int64, ed25519.PrivateKey, int64, int64) {
	t.Helper()
	h, st, _, agentID, key, mine, theirs := ingestFixtureAlerts(t)
	return h, st, agentID, key, mine, theirs
}

// ingestFixtureAlerts is ingestFixture with the alert manager the master
// evaluates on, for tests that define a rule and read what fired.
func ingestFixtureAlerts(t *testing.T) (http.Handler, *store.SQLite, *alert.Manager, int64, ed25519.PrivateKey, int64, int64) {
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
	mgr := alert.NewManager(st, nil)
	return New(st, Options{Alerts: mgr}, fstest.MapFS{}), st, mgr, rec.ID, priv, mine.ID, theirs.ID
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

// The mask an agent measured has to reach the master's store, or loss position
// exists only on the agents' own disks.
func TestIngestStoresLossMask(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	m := measurement(mine, 1_756_400_000) // 5 sent, 3 received
	m.LossMask = store.EncodeLossMask(false, []bool{false, false, false, true, true})
	if rec := submit(t, h, agentID, key, []store.Measurement{m}, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
	}
	got, err := st.QueryRange(t.Context(), mine, agentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].LossMask, m.LossMask) {
		t.Errorf("stored %+v, want mask %v", got, m.LossMask)
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

// An agent cannot rewrite an interval it has already reported. The upsert used
// to make a replay "change nothing" only when the bytes were identical; the
// same key with different values silently replaced stored history, which for a
// system whose point is keeping every interval as it was measured is the one
// write that must not be possible.
func TestIngestKeepsWhatIsAlreadyStored(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	const ts = 1_756_400_000

	if rec := submit(t, h, agentID, key, []store.Measurement{measurement(mine, ts)}, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("first submission = %d: %s", rec.Code, rec.Body)
	}

	// Same key, different measurement: more probes, different replies.
	rewrite := store.Measurement{TargetID: mine, TS: ts, Sent: 9, Received: 1, Samples: []uint32{42}}
	rec := submit(t, h, agentID, key, []store.Measurement{rewrite}, time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("resubmission = %d: %s", rec.Code, rec.Body)
	}
	var resp struct{ Accepted, Duplicates int }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Accepted != 0 || resp.Duplicates != 1 {
		t.Errorf("response = %+v, want 0 accepted and 1 duplicate", resp)
	}

	got, err := st.QueryRange(t.Context(), mine, agentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d rows, want 1", len(got))
	}
	if got[0].Sent != 5 || got[0].Received != 3 || len(got[0].Samples) != 3 {
		t.Errorf("the stored interval was rewritten: %+v", got[0])
	}
}

// A measurement is the interval that began at its timestamp, so none can be
// later than now. Rows beyond the clock skew the signature check tolerates are
// dropped, and the rest of the batch still lands: refusing the whole batch is
// what wedges an agent's outbox. A row inside the skew is an honest clock
// slightly ahead, and stays.
func TestIngestDropsFutureRowsButKeepsTheRest(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	now := time.Now()
	inSkew := now.Add(ingest.MaxSkew - time.Minute).Unix()
	future := now.Add(ingest.MaxSkew + time.Hour).Unix()
	// Just past the allowance, by enough that the handler reading the clock a
	// few milliseconds later cannot round it back inside: a horizon of twice the
	// skew would keep this row, and one an hour out would not tell the two apart.
	justPast := now.Add(ingest.MaxSkew + 2*time.Second).Unix()

	rec := submit(t, h, agentID, key, []store.Measurement{
		measurement(mine, future),
		measurement(mine, justPast),
		measurement(mine, inSkew),
		measurement(mine, 1_756_400_000),
	}, now)
	if rec.Code != http.StatusOK {
		t.Fatalf("a batch with a future row = %d, want it accepted: %s", rec.Code, rec.Body)
	}
	got, err := st.QueryRange(t.Context(), mine, agentID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	var have []int64
	for _, m := range got {
		have = append(have, m.TS)
	}
	if len(have) != 2 || have[0] != 1_756_400_000 || have[1] != inSkew {
		t.Errorf("stored timestamps %v, want the past row and the one inside the skew (%d), not %d",
			have, inSkew, future)
	}
}

// lossRule defines a loss rule on the root that fires after two lossy
// intervals in a row, and loads it into the manager.
func lossRule(t *testing.T, st *store.SQLite, mgr *alert.Manager) {
	t.Helper()
	rule := alert.Rule{TargetID: 1, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 2, ClearFor: 2, Enabled: true, Severity: alert.SeverityWarning}
	if err := st.UpsertAlertRule(t.Context(), &rule); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// An agent's intervals are evaluated like the local prober's, against that
// agent's own series, and a batch delivered twice counts once: the retry an
// agent makes when the answer to its first attempt was lost must not push a
// rule over its for_intervals.
func TestIngestEvaluatesAlertRules(t *testing.T) {
	h, st, mgr, agentID, key, mine, _ := ingestFixtureAlerts(t)
	lossRule(t, st, mgr)
	now := time.Now()
	const t0 = 1_756_400_000

	// measurement() is 3 of 5 answered: 40% loss, over the rule's 20%.
	first := []store.Measurement{measurement(mine, t0)}
	for range 2 {
		if rec := submit(t, h, agentID, key, first, now); rec.Code != http.StatusOK {
			t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
		}
	}
	if firing := mgr.Firing(); len(firing) != 0 {
		t.Fatalf("one lossy interval, delivered twice, fired a rule that needs two: %+v", firing)
	}

	if rec := submit(t, h, agentID, key, []store.Measurement{measurement(mine, t0+60)}, now); rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
	}
	firing := mgr.Firing()
	if len(firing) != 1 {
		t.Fatalf("two lossy intervals from an agent left %d alert(s) firing, want 1", len(firing))
	}
	if firing[0].AgentID != agentID || firing[0].TargetID != mine {
		t.Errorf("fired for target %d agent %d, want target %d agent %d",
			firing[0].TargetID, firing[0].AgentID, mine, agentID)
	}
	events, err := st.ListAlertEvents(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].Firing || events[0].AgentID != agentID {
		t.Errorf("transition log = %+v, want one fire for agent %d", events, agentID)
	}

	// Two clean intervals resolve it. The log dates the resolve by the interval
	// that cleared the rule, as it dates the fire: these intervals are months
	// old by the time this batch arrives, as an agent's backlog is hours old.
	clean := func(ts int64) store.Measurement {
		return store.Measurement{TargetID: mine, TS: ts, Sent: 5, Received: 5,
			Samples: []uint32{1000, 1000, 2000, 2000, 3000}}
	}
	if rec := submit(t, h, agentID, key, []store.Measurement{clean(t0 + 120), clean(t0 + 180)}, now); rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
	}
	if firing := mgr.Firing(); len(firing) != 0 {
		t.Fatalf("two clean intervals left it firing: %+v", firing)
	}
	if events, err = st.ListAlertEvents(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	// Newest first.
	if len(events) != 2 || events[0].Firing || events[0].TS != t0+180 || events[1].TS != t0+60 {
		t.Errorf("transition log = %+v, want a resolve at %d after the fire at %d", events, t0+180, t0+60)
	}
}

// Rows are evaluated oldest first whatever order the batch lists them in. A
// rule counts consecutive intervals and a gap resets the count; read newest
// first, two lossy intervals ten minutes apart looked consecutive and fired.
func TestIngestEvaluatesABatchInTimeOrder(t *testing.T) {
	h, st, mgr, agentID, key, mine, _ := ingestFixtureAlerts(t)
	lossRule(t, st, mgr)
	const t0 = 1_756_400_000

	rec := submit(t, h, agentID, key, []store.Measurement{
		measurement(mine, t0+600), measurement(mine, t0),
	}, time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
	}
	if firing := mgr.Firing(); len(firing) != 0 {
		t.Errorf("two lossy intervals with a gap between them fired: %+v", firing)
	}
}

// An interval older than what a rule has already evaluated for the series is
// stored and not evaluated. Stepping back is not a gap, so it would have
// counted as the next consecutive interval and fired on two that are ten
// minutes apart.
func TestIngestDoesNotEvaluateARowThatArrivesLate(t *testing.T) {
	h, st, mgr, agentID, key, mine, _ := ingestFixtureAlerts(t)
	lossRule(t, st, mgr)
	now := time.Now()
	const t0 = 1_756_400_000

	for _, ts := range []int64{t0 + 600, t0} {
		if rec := submit(t, h, agentID, key, []store.Measurement{measurement(mine, ts)}, now); rec.Code != http.StatusOK {
			t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
		}
	}
	got, err := st.QueryRange(t.Context(), mine, agentID, 0, 1<<40)
	if err != nil || len(got) != 2 {
		t.Fatalf("stored %d measurements, %v; the late one must still be kept", len(got), err)
	}
	if firing := mgr.Firing(); len(firing) != 0 {
		t.Errorf("a late interval counted as the next one and fired: %+v", firing)
	}
}

// tailBatch is sixty intervals of twenty probes from an agent, lossy in pairs
// (two in every five), each lossy one having lost its last five probes: the
// shape a rate limiter leaves, flagged by the master from the twelfth lossy
// interval on.
func tailBatch(target int64) []store.Measurement {
	const t0, n = 1_756_400_000, 20
	var ms []store.Measurement
	for i := range 60 {
		m := store.Measurement{TargetID: target, TS: t0 + int64(i)*60, Sent: n, Received: n}
		lost := make([]bool, n)
		if i%5 < 2 {
			for p := 15; p < n; p++ {
				lost[p] = true
			}
			m.Received = 15
			m.LossMask = store.EncodeLossMask(false, lost)
		}
		for k := range m.Received {
			m.Samples = append(m.Samples, uint32(1000+k))
		}
		ms = append(ms, m)
	}
	return ms
}

// Protects the path a flag travels on a live master, from the write that sets
// it to the rule that reads it: an agent's intervals are judged by the store as
// they are stored, and the rows handed to alerting carry the verdict. With
// loss at fixed positions left out, the rule stops firing once the flags begin;
// with flag_only on the target it fires on every lossy pair, as it always did.
func TestIngestCarriesPositionalLossToTheRules(t *testing.T) {
	fires := func(policy string) int {
		h, st, mgr, agentID, key, mine, _ := ingestFixtureAlerts(t)
		st.DetectPositionalLoss()
		if policy != "" {
			targets, err := st.ListTargets(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for i := range targets {
				if targets[i].ID == mine {
					targets[i].Settings.PositionalLoss = &policy
					if err := st.UpsertTarget(t.Context(), &targets[i]); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		lossRule(t, st, mgr)
		batch := tailBatch(mine)
		for i := 0; i < len(batch); i += 10 {
			if rec := submit(t, h, agentID, key, batch[i:i+10], time.Now()); rec.Code != http.StatusOK {
				t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
			}
		}
		events, err := st.ListAlertEvents(t.Context(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range events {
			if e.Firing {
				n++
			}
		}
		return n
	}
	// Twenty-four lossy intervals in twelve pairs. Left out from the twelfth
	// lossy interval on, only the first five pairs can fire.
	excluded, counted := fires(""), fires("flag_only")
	if counted != 12 {
		t.Errorf("with flag_only the rule fired %d times, want once per lossy pair, 12", counted)
	}
	if excluded != 5 {
		t.Errorf("with the default the rule fired %d times, want 5: the pairs before the flags begin", excluded)
	}
}
