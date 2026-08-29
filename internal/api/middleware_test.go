package api

import (
	"context"
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

// fakeAuth stands in for the OIDC flow: the flow itself is the provider's to
// get right, but what a session is allowed to do is ours.
type fakeAuth struct {
	session *auth.Session
}

func (f *fakeAuth) SessionFrom(*http.Request) (auth.Session, bool) {
	if f.session == nil {
		return auth.Session{}, false
	}
	return *f.session, true
}

func (f *fakeAuth) Routes(*http.ServeMux) {}

func authedServer(t *testing.T, sess *auth.Session) http.Handler {
	t.Helper()
	st := seededStore(t)
	return New(st, Options{Auth: &fakeAuth{session: sess}}, fstest.MapFS{})
}

// seededStore has a target and a rule to act on. A refusal only proves
// anything against a row that exists: against a missing one, "not found" and
// "not allowed" are indistinguishable, and the test would still pass with the
// authorisation removed.
func seededStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	root := int64(1)
	n := tree.Target{
		ParentID: &root, Name: "t", Enabled: true,
		Host: ptr("1.1.1.1"), AddressFamily: ptr("v4"),
	}
	if err := st.UpsertTarget(ctx, &n); err != nil {
		t.Fatal(err)
	}
	rule := alert.Rule{
		TargetID: n.ID, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 3, ClearFor: 3, Enabled: true,
	}
	if err := st.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.ID != 1 || n.ID != 2 {
		t.Fatalf("fixture ids moved: target %d, rule %d", n.ID, rule.ID)
	}
	return st
}

func ptr[T any](v T) *T { return &v }

// Every route that changes state must require an admin, and every route that
// reads must require at least a viewer. Checking the whole table rather than
// a sample is the point: a route added without a guard is exactly the bug
// this catches.
func TestRolesAreEnforcedOnEveryRoute(t *testing.T) {
	reads := []struct{ method, path string }{
		{"GET", "/api/v1/targets"},
		{"GET", "/api/v1/measurements?target_id=1"},
		{"GET", "/api/v1/alert-rules"},
		{"GET", "/api/v1/alerts"},
	}
	// Bodies are valid on purpose: a create that fails validation never
	// reaches the authorisation check, so an empty body would prove nothing.
	writes := []struct{ method, path, body string }{
		{"POST", "/api/v1/targets", `{"parent_id":1,"name":"new"}`},
		{"PATCH", "/api/v1/targets/2", `{"title":"x"}`},
		{"DELETE", "/api/v1/targets/2", "{}"},
		{"POST", "/api/v1/alert-rules",
			`{"target_id":2,"name":"extra","metric":"loss","op":">","threshold":10}`},
		{"PATCH", "/api/v1/alert-rules/1", `{"threshold":40}`},
		{"DELETE", "/api/v1/alert-rules/1", "{}"},
	}

	viewer := &auth.Session{Subject: "v", Role: auth.RoleViewer, Expires: 1 << 40}
	admin := &auth.Session{Subject: "a", Role: auth.RoleAdmin, Expires: 1 << 40}

	call := func(h http.Handler, method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	anon := authedServer(t, nil)
	for _, r := range reads {
		if code := call(anon, r.method, r.path, "{}"); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", r.method, r.path, code)
		}
	}
	for _, r := range writes {
		if code := call(anon, r.method, r.path, r.body); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", r.method, r.path, code)
		}
	}

	viewerSrv := authedServer(t, viewer)
	for _, r := range reads {
		if code := call(viewerSrv, r.method, r.path, "{}"); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want to be allowed to read", r.method, r.path, code)
		}
	}
	for _, r := range writes {
		// 403 for something they can see, 404 for something they cannot —
		// what matters is that the write did not happen.
		code := call(viewerSrv, r.method, r.path, r.body)
		if code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("viewer %s %s = %d, want the write refused", r.method, r.path, code)
		}
	}

	adminSrv := authedServer(t, admin)
	for _, r := range writes {
		if code := call(adminSrv, r.method, r.path, r.body); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("admin %s %s = %d, want to be allowed to write", r.method, r.path, code)
		}
	}
}

// A health check behind a login is not a health check, and the UI shell holds
// no data of its own.
func TestPublicRoutesStayPublic(t *testing.T) {
	h := authedServer(t, nil)
	for _, path := range []string{"/healthz", "/api/v1/me"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 without a session", path, rec.Code)
		}
	}
}

// With no authenticator configured smokeng runs open, which serve only allows
// on loopback. The API must say so rather than implying a login exists.
func TestUnauthenticatedModeReportsItself(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "open.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := New(st, Options{}, fstest.MapFS{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/me", nil))
	if !strings.Contains(rec.Body.String(), `"auth_enabled":false`) {
		t.Errorf("/api/v1/me = %s, want auth_enabled false", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/targets", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("targets without auth configured = %d, want 200", rec.Code)
	}
}

// /metrics names agents and counts targets, so it stays behind the session by
// default and opens only when an operator asks for it.
func TestMetricsAccess(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	guarded := New(st, Options{Auth: &fakeAuth{}}, fstest.MapFS{})
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous /metrics = %d, want 401 by default", rec.Code)
	}

	open := New(st, Options{Auth: &fakeAuth{}, MetricsPublic: true}, fstest.MapFS{})
	rec = httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("anonymous /metrics with --metrics-public = %d, want 200", rec.Code)
	}
}

// The point of these metrics is that they describe smokeng, not the network:
// latency and loss must never leak into a scrape.
func TestMetricsCarryNoMeasurementData(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.WriteMeasurements(t.Context(), []store.Measurement{{
		TargetID: 1, TS: 1_756_400_000, Sent: 5, Received: 3,
		Samples: []uint32{1234, 5678, 9012},
	}}); err != nil {
		t.Fatal(err)
	}

	h := New(st, Options{Version: "test"}, fstest.MapFS{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	if !strings.Contains(body, `smokeng_build_info{version="test"} 1`) {
		t.Errorf("no build info in:\n%s", body)
	}
	for _, forbidden := range []string{"rtt", "latency", "median", "1234", "5678", "9012"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("scrape leaks measurement data (%q):\n%s", forbidden, body)
		}
	}
}
