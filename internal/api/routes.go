package api

import (
	"net/http"

	"github.com/timdebruijn/smokeng/internal/auth"
)

// routeClass says what a route does about authorisation. Every route declares
// one (DESIGN.md §7.4).
//
// Enforcement lives at the API boundary rather than in the store, which buys
// one enforcement point and costs the obvious risk: an endpoint added later
// that forgets to filter is a disclosure, not a bug. That risk is paid for
// here — the router records what every route declared, and a test fails when a
// route exists that nobody classified. Forgetting becomes a red build.
type routeClass string

const (
	// classPublic needs no session: the health check and the UI shell, which
	// holds no data of its own. The OIDC login routes (/auth/login,
	// /auth/callback, /auth/logout) are not registered through this router at
	// all — the Authenticator wires them onto the mux directly in New(), since
	// they exist to establish the session this router's other classes assume.
	classPublic routeClass = "public"
	// classGlobalAdmin is for what a grant never confers — agents, enrolment
	// tokens, grants themselves, /metrics, and TOML import and export, which
	// are declarative over the whole tree.
	classGlobalAdmin routeClass = "global-admin"
	// classScopedRead returns only what the caller's scope contains.
	classScopedRead routeClass = "scoped-read"
	// classScopedWrite additionally requires editor on the node it touches.
	classScopedWrite routeClass = "scoped-write"
	// classAgentSigned carries its own Ed25519 credential and is deliberately
	// outside the session middleware entirely.
	classAgentSigned routeClass = "agent-signed"
	// classMetricsPublic is /metrics when the operator has explicitly opened
	// it for a scraper, which cannot present a session cookie.
	classMetricsPublic routeClass = "metrics-public"
)

// router records the class of every route as it is registered, so the set can
// be asserted on.
type router struct {
	mux     *http.ServeMux
	s       *server
	classes map[string]routeClass
}

func newRouter(s *server) *router {
	return &router{mux: http.NewServeMux(), s: s, classes: map[string]routeClass{}}
}

// handle registers a route under a class, and applies the session middleware
// that class implies. Declaring the class and choosing the middleware are the
// same act, so the two cannot disagree.
func (rt *router) handle(class routeClass, pattern string, h http.HandlerFunc) {
	if _, dup := rt.classes[pattern]; dup {
		panic("api: route registered twice: " + pattern)
	}
	rt.classes[pattern] = class
	switch class {
	case classGlobalAdmin:
		h = rt.s.requireRole(auth.RoleAdmin, h)
	case classScopedRead, classScopedWrite:
		// The handler resolves and applies the scope itself; requiring a
		// session here keeps an anonymous caller from reaching it at all.
		h = rt.s.requireRole(auth.RoleViewer, h)
	case classPublic, classAgentSigned, classMetricsPublic:
		// Deliberately unwrapped.
	default:
		panic("api: unknown route class " + string(class))
	}
	rt.mux.HandleFunc(pattern, h)
}

// classified returns every registered route and its class, sorted.
func (rt *router) classified() map[string]routeClass {
	out := make(map[string]routeClass, len(rt.classes))
	for k, v := range rt.classes {
		out[k] = v
	}
	return out
}

// handler is what New returns. It is an http.Handler like any other; the
// reference to the server exists so the route-coverage test can assert on the
// table the router actually built, rather than on a copy of it in the test.
type handler struct {
	http.Handler
	srv *server
}
