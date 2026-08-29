package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/timdebruijn/smokeng/internal/store"
)

func TestLoadOrCreateKeyGeneratesOnceAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "probe.key")

	key, created, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if !created {
		t.Error("first load did not report the key as newly created")
	}

	// The private key is the agent's whole identity: it must not be readable
	// by anyone else on the host.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode is %o, want 600", perm)
	}

	again, created, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if created {
		t.Error("second load regenerated the key; the agent would lose its enrolment")
	}
	if !key.Equal(again) {
		t.Error("second load returned a different key")
	}
	if PublicKey(key) != PublicKey(again) {
		t.Error("public halves differ across loads")
	}
}

func TestLoadOrCreateKeyRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.key")
	if err := os.WriteFile(path, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Silently replacing an unreadable key would enrol the agent as a stranger
	// and drop its measurements on the floor. It has to complain instead.
	if _, _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("a corrupt key file was accepted")
	}
}

func TestNewRefusesPlainHTTPMaster(t *testing.T) {
	key, _, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "probe.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Master: "http://master.example.org"}, key, nil); err == nil {
		t.Fatal("a plain-HTTP master was accepted without --insecure-allow-http")
	}
	if _, err := New(Config{Master: "http://master.example.org", Insecure: true}, key, nil); err != nil {
		t.Fatalf("--insecure-allow-http did not permit a plain-HTTP master: %v", err)
	}
}

// newTestAgent wires an agent to a master handler and a fresh buffer database.
func newTestAgent(t *testing.T, h http.Handler) (*Agent, *store.SQLite, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, _, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "probe.key"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Master: srv.URL, AgentID: 7, Insecure: true}, key, st)
	if err != nil {
		t.Fatal(err)
	}
	return a, st, srv
}

func assignmentsJSON(t *testing.T, w http.ResponseWriter, as []assignment) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(struct {
		Targets []assignment `json:"targets"`
	}{as}); err != nil {
		t.Error(err)
	}
}

func TestPullSignsAndMirrorsAssignments(t *testing.T) {
	var gotAgentID, gotNonce, gotSig, gotPath string
	assigned := []assignment{{
		TargetID: 42, Path: "/Internet/cloudflare-v4", Host: "1.1.1.1",
		AddressFamily: "v4", IntervalS: 30, Pings: 40, ProbeMode: "spread",
		BurstGapMS: 10, TimeoutMS: 1000, PacketSize: 56, DSCP: 0,
	}, {
		TargetID: 43, Path: "/Internet/quad9-v4", Host: "9.9.9.9",
		AddressFamily: "v4", IntervalS: 60, Pings: 20, ProbeMode: "burst",
		BurstGapMS: 10, TimeoutMS: 1000, PacketSize: 56, DSCP: 0,
	}}

	a, st, _ := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAgentID = r.Header.Get("X-Agent-Id")
		gotNonce = r.Header.Get("X-Nonce")
		gotSig = r.Header.Get("X-Signature")
		assignmentsJSON(t, w, assigned)
	}))

	ctx := context.Background()
	if err := a.pull(ctx); err != nil {
		t.Fatalf("pull: %v", err)
	}

	if gotPath != "/api/v1/agent/targets" {
		t.Errorf("pulled from %q", gotPath)
	}
	if gotAgentID != "7" {
		t.Errorf("X-Agent-Id = %q, want 7", gotAgentID)
	}
	if gotNonce == "" || gotSig == "" {
		t.Error("the request went out unsigned")
	}

	targets, err := st.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]bool{}
	for _, tg := range targets {
		if tg.Host == nil {
			continue // the root node
		}
		byID[tg.ID] = tg.Enabled
	}
	// Target ids must match the master's, or the measurements will not line up
	// with the series they belong to when they arrive.
	for _, want := range []int64{42, 43} {
		if enabled, ok := byID[want]; !ok {
			t.Errorf("target %d was not mirrored", want)
		} else if !enabled {
			t.Errorf("target %d was mirrored but not enabled", want)
		}
	}

	// The master resolved inheritance already; the agent must store what it was
	// told rather than re-deriving it.
	for _, tg := range targets {
		if tg.ID != 42 {
			continue
		}
		if tg.Settings.IntervalS == nil || *tg.Settings.IntervalS != 30 {
			t.Errorf("target 42 interval = %v, want 30", tg.Settings.IntervalS)
		}
		if tg.Settings.PingsPerInterval == nil || *tg.Settings.PingsPerInterval != 40 {
			t.Errorf("target 42 pings = %v, want 40", tg.Settings.PingsPerInterval)
		}
		if tg.Settings.ProbeMode == nil || *tg.Settings.ProbeMode != "spread" {
			t.Errorf("target 42 probe mode = %v, want spread", tg.Settings.ProbeMode)
		}
	}
}

func TestPullDisablesUnassignedTargets(t *testing.T) {
	assigned := []assignment{{
		TargetID: 42, Path: "/a", Host: "1.1.1.1", AddressFamily: "v4",
		IntervalS: 60, Pings: 20, ProbeMode: "burst", BurstGapMS: 10,
		TimeoutMS: 1000, PacketSize: 56,
	}, {
		TargetID: 43, Path: "/b", Host: "9.9.9.9", AddressFamily: "v4",
		IntervalS: 60, Pings: 20, ProbeMode: "burst", BurstGapMS: 10,
		TimeoutMS: 1000, PacketSize: 56,
	}}

	a, st, _ := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assignmentsJSON(t, w, assigned)
	}))

	ctx := context.Background()
	if err := a.pull(ctx); err != nil {
		t.Fatal(err)
	}
	assigned = assigned[:1] // the master takes target 43 away
	if err := a.pull(ctx); err != nil {
		t.Fatal(err)
	}

	targets, err := st.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		switch tg.ID {
		case 42:
			if !tg.Enabled {
				t.Error("a still-assigned target was disabled")
			}
		case 43:
			if tg.Enabled {
				t.Error("an unassigned target is still being probed")
			}
		}
	}
}

func bufferMeasurements(t *testing.T, st *store.SQLite, n int) {
	t.Helper()
	ms := make([]store.Measurement, 0, n)
	for i := range n {
		ms = append(ms, store.Measurement{
			TargetID: 42, AgentID: 7, TS: int64(1_700_000_000 + i*60),
			Sent: 3, Received: 3, Samples: []uint32{1000, 1200, 1500},
		})
	}
	if err := st.WriteMeasurements(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
}

// A push that is not confirmed must not clear the buffer. This is the invariant
// that keeps a maintenance window on the master from costing measurements.
func TestPushKeepsBufferUntilMasterConfirms(t *testing.T) {
	status := http.StatusInternalServerError
	var pushes int

	a, st, _ := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ingest" {
			t.Errorf("pushed to %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/vnd.apache.arrow.stream" {
			t.Errorf("Content-Type = %q", ct)
		}
		pushes++
		w.WriteHeader(status)
	}))

	ctx := context.Background()
	bufferMeasurements(t, st, 5)

	a.push(ctx)
	if pushes != 1 {
		t.Fatalf("pushed %d times, want 1", pushes)
	}
	pending, err := st.PendingMeasurements(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 5 {
		t.Fatalf("%d measurements buffered after a rejected push, want 5", len(pending))
	}

	status = http.StatusOK
	a.push(ctx)
	pending, err = st.PendingMeasurements(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d measurements still buffered after the master accepted them", len(pending))
	}
}

// An unreachable master is the ordinary case during a WAN outage, and is
// exactly when the measurements matter most.
func TestPushKeepsBufferWhenMasterIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, _, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "probe.key"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Master: srv.URL, AgentID: 7, Insecure: true}, key, st)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close() // nothing is listening any more

	bufferMeasurements(t, st, 3)
	a.push(context.Background())

	pending, err := st.PendingMeasurements(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("%d measurements buffered after an unreachable master, want 3", len(pending))
	}
}

// Drain is what a clean shutdown relies on, after the engine has flushed.
func TestDrainSubmitsTheBacklog(t *testing.T) {
	a, st, _ := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	bufferMeasurements(t, st, 4)

	a.Drain(context.Background())

	pending, err := st.PendingMeasurements(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d measurements stranded by Drain", len(pending))
	}
}
