package api

import (
	"net/http"
	"sort"
	"testing"
	"testing/fstest"
)

// Enforcement lives at the API boundary, which buys one place to reason about
// and costs the risk that a route added later quietly forgets to filter — a
// disclosure rather than a bug (DESIGN.md §7.4).
//
// This is the payment. Every route the router actually built must appear here
// with the class it was registered under. A new endpoint fails this test until
// somebody has said, in writing, who is allowed to reach it. Changing a class
// fails it too, so widening access is never a silent diff.
func TestEveryRouteIsClassified(t *testing.T) {
	st := seededStore(t)
	h := New(st, Options{Auth: &fakeAuth{}}, fstest.MapFS{})
	built := h.(*handler).srv.routes.classified()

	want := map[string]routeClass{
		"GET /healthz":                     classPublic,
		"GET /api/v1/me":                   classPublic,
		"GET /metrics":                     classGlobalAdmin,
		"GET /api/v1/targets":              classScopedRead,
		"POST /api/v1/targets":             classScopedWrite,
		"PATCH /api/v1/targets/{id}":       classScopedWrite,
		"DELETE /api/v1/targets/{id}":      classScopedWrite,
		"GET /api/v1/measurements":         classScopedRead,
		"GET /api/v1/alert-rules":          classScopedRead,
		"POST /api/v1/alert-rules":         classScopedWrite,
		"PATCH /api/v1/alert-rules/{id}":   classScopedWrite,
		"DELETE /api/v1/alert-rules/{id}":  classScopedWrite,
		"GET /api/v1/alerts":               classScopedRead,
		"GET /api/v1/agents":               classScopedRead,
		"GET /api/v1/paths":                classScopedRead,
		"PATCH /api/v1/agents/{id}":        classGlobalAdmin,
		"DELETE /api/v1/agents/{id}":       classGlobalAdmin,
		"GET /api/v1/agent-tokens":         classGlobalAdmin,
		"POST /api/v1/agent-tokens":        classGlobalAdmin,
		"DELETE /api/v1/agent-tokens/{id}": classGlobalAdmin,
		"POST /api/v1/agent/enrol":         classAgentSigned,
		"GET /api/v1/grants":               classGlobalAdmin,
		"POST /api/v1/grants":              classGlobalAdmin,
		"DELETE /api/v1/grants/{id}":       classGlobalAdmin,
		"POST /api/v1/ingest":              classAgentSigned,
		"GET /api/v1/agent/targets":        classAgentSigned,
	}

	for pattern, class := range built {
		expect, known := want[pattern]
		if !known {
			t.Errorf("route %q is not in this test. Decide who may reach it, "+
				"register it under that class, and add it here.", pattern)
			continue
		}
		if expect != class {
			t.Errorf("route %q is registered as %q but this test expects %q. "+
				"If the change is intended, say so here as well.", pattern, class, expect)
		}
	}
	for pattern := range want {
		if _, ok := built[pattern]; !ok {
			t.Errorf("route %q is expected but was never registered", pattern)
		}
	}
}

// A public route is one that answers without a session, so the list of them
// deserves to be short and stated on its own.
func TestPublicRoutesAreOnlyTheHarmlessOnes(t *testing.T) {
	st := seededStore(t)
	h := New(st, Options{Auth: &fakeAuth{}}, fstest.MapFS{})
	built := h.(*handler).srv.routes.classified()

	var public []string
	for pattern, class := range built {
		if class == classPublic {
			public = append(public, pattern)
		}
	}
	sort.Strings(public)

	want := []string{"GET /api/v1/me", "GET /healthz"}
	if len(public) != len(want) {
		t.Fatalf("public routes = %v, want %v", public, want)
	}
	for i := range want {
		if public[i] != want[i] {
			t.Fatalf("public routes = %v, want %v", public, want)
		}
	}
}

// /metrics counts and names things across the whole installation, so it is
// global admin unless the operator has deliberately opened it for a scraper.
func TestMetricsIsAdminUnlessOpened(t *testing.T) {
	st := seededStore(t)
	closed := New(st, Options{Auth: &fakeAuth{}}, fstest.MapFS{})
	if got := closed.(*handler).srv.routes.classified()["GET /metrics"]; got != classGlobalAdmin {
		t.Errorf("GET /metrics = %q, want %q", got, classGlobalAdmin)
	}
	open := New(st, Options{Auth: &fakeAuth{}, MetricsPublic: true}, fstest.MapFS{})
	if got := open.(*handler).srv.routes.classified()["GET /metrics"]; got != classMetricsPublic {
		t.Errorf("GET /metrics with MetricsPublic = %q, want %q", got, classMetricsPublic)
	}
}

var _ http.Handler = (*handler)(nil)
