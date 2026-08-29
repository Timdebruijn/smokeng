package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"smokeng/internal/auth"
	"smokeng/internal/store"
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
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil, &fakeAuth{session: sess}, fstest.MapFS{})
}

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
	writes := []struct{ method, path string }{
		{"POST", "/api/v1/targets"},
		{"PATCH", "/api/v1/targets/1"},
		{"DELETE", "/api/v1/targets/1"},
		{"POST", "/api/v1/alert-rules"},
		{"PATCH", "/api/v1/alert-rules/1"},
		{"DELETE", "/api/v1/alert-rules/1"},
	}

	viewer := &auth.Session{Subject: "v", Role: auth.RoleViewer, Expires: 1 << 40}
	admin := &auth.Session{Subject: "a", Role: auth.RoleAdmin, Expires: 1 << 40}

	call := func(h http.Handler, method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	anon := authedServer(t, nil)
	for _, r := range append(append([]struct{ method, path string }{}, reads...), writes...) {
		if code := call(anon, r.method, r.path); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", r.method, r.path, code)
		}
	}

	viewerSrv := authedServer(t, viewer)
	for _, r := range reads {
		if code := call(viewerSrv, r.method, r.path); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want to be allowed to read", r.method, r.path, code)
		}
	}
	for _, r := range writes {
		if code := call(viewerSrv, r.method, r.path); code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", r.method, r.path, code)
		}
	}

	adminSrv := authedServer(t, admin)
	for _, r := range writes {
		if code := call(adminSrv, r.method, r.path); code == http.StatusUnauthorized || code == http.StatusForbidden {
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
	h := New(st, nil, nil, fstest.MapFS{})

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
