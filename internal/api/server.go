// Package api serves the HTTP API and the embedded web UI (DESIGN.md §7).
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/ingest"
	"github.com/timdebruijn/smokeng/internal/store"
)

// AlertStore is the persistence the alert endpoints need, on top of Store.
type AlertStore interface {
	store.Store
	ListAlertRules(ctx context.Context) ([]alert.Rule, error)
	UpsertAlertRule(ctx context.Context, r *alert.Rule) error
	DeleteAlertRule(ctx context.Context, id int64) error
}

// AlertView exposes what is currently firing. Nil when alerting is not
// configured, which the API reports rather than pretending nothing is wrong.
type AlertView interface {
	Firing() []alert.Alert
	// Delivering reports whether transitions go anywhere beyond the log.
	// Evaluation no longer depends on it, so the two are separate facts.
	Delivering() bool
	// Acknowledge marks a firing alert seen (or clears it), returning whether a
	// firing alert existed to change.
	Acknowledge(ctx context.Context, ruleID, targetID, agentID int64, ack bool, by string) (bool, error)
}

// Store is everything the API persists through: the measurement store plus
// the alert and agent tables.
type Store interface {
	AlertStore
	AgentStore
	PathStore
}

type server struct {
	st     Store
	alerts AlertView
	auth   Authenticator
	agents AgentStore
	enrol  EnrolStore
	grants GrantStore
	events EventStore
	config ConfigStore
	routes *router
	// defaultRole is what an authenticated caller with no grant gets. It is a
	// setting rather than a consequence, so that adding the first grant does
	// not silently lock out everyone who could already read (DESIGN.md §7.4).
	defaultRole auth.Role
	externalURL string
	trusted     TrustedProxies
	verifier    *ingest.Verifier
	probe       ProbeStats
	ingest      IngestStats
	version     string
	// agentCAs are the PEM blocks this master was given on the command line,
	// handed down to agents so a CA rotates in one place. Never anything an
	// agent supplied: a master relays its own operator's decision.
	agentCAs [][]byte
}

// Options are the parts of the server that are optional or supplied later.
type Options struct {
	Alerts AlertView
	Auth   Authenticator
	// Probe is the local engine, absent on an instance that only serves.
	Probe   ProbeStats
	Version string
	// MetricsPublic serves /metrics without a session even when
	// authentication is enabled, so Prometheus can scrape it. Off by default:
	// an endpoint that bypasses login should be asked for, not assumed.
	MetricsPublic bool
	// ExternalURL is the address others reach this instance at, when that is
	// not the address it listens on — a reverse proxy in front, most often.
	// It is what the enrolment command in the UI must name: an agent has to
	// be told where to connect, and that is not necessarily where the admin
	// looking at the page happens to be connected.
	ExternalURL string
	// TrustedProxies are the peers whose X-Forwarded-For may be believed, so
	// log lines can name the real client rather than the proxy. Nothing is
	// authorised on a client address, so this affects logging only.
	TrustedProxies TrustedProxies
	// DefaultRole is what an authenticated caller holding no grant gets.
	// Empty means viewer, which is what smokeng did before grants existed.
	// Set it to "none" once grants describe who may see what.
	DefaultRole auth.Role
	// AgentCAs are the CA certificates agents should trust when probing
	// https targets, as PEM. Handing them down means a rotation happens on
	// the master rather than on every agent host.
	AgentCAs [][]byte
}

// New builds the HTTP handler: API routes plus the embedded frontend.
// authenticator may be nil, in which case every request is treated as an
// admin — permitted only on loopback, which serve enforces.
// /metrics (Prometheus self-observability, §7.1) is registered below.
func New(st Store, opts Options, webFS fs.FS) http.Handler {
	authenticator := opts.Auth
	s := &server{
		st: st, alerts: opts.Alerts, auth: authenticator, agents: st,
		probe: opts.Probe, version: opts.Version, agentCAs: opts.AgentCAs,
	}
	// Enrolment is optional in the same way the local prober is: a store that
	// cannot mint tokens simply does not get the routes, rather than forcing
	// every implementation of Store to grow seven methods it may not want.
	if es, ok := st.(EnrolStore); ok {
		s.enrol = es
	}
	if gs, ok := st.(GrantStore); ok {
		s.grants = gs
	}
	if es, ok := st.(EventStore); ok {
		s.events = es
	}
	if cs, ok := st.(ConfigStore); ok {
		s.config = cs
	}
	s.externalURL = opts.ExternalURL
	s.trusted = opts.TrustedProxies
	s.defaultRole = opts.DefaultRole
	if s.defaultRole == "" {
		s.defaultRole = auth.RoleViewer
	}
	if s.version == "" {
		s.version = "unknown"
	}
	// Agents authenticate with their own Ed25519 signatures, never with a
	// browser session, so the verifier reads the same enrolment table the
	// admin CLI writes.
	s.verifier = &ingest.Verifier{Lookup: func(id int64) (ingest.Agent, bool) {
		records, err := st.ListAgents(context.Background())
		if err != nil {
			log.Printf("ingest: agent lookup: %v", err)
			return ingest.Agent{}, false
		}
		for _, a := range records {
			if a.ID == id && len(a.PubKey) > 0 {
				return ingest.Agent{ID: a.ID, Name: a.Name, PubKey: a.PubKey, Enabled: a.Enabled}, true
			}
		}
		return ingest.Agent{}, false
	}}
	s.ingest = s.verifier
	rt := newRouter(s)
	mux := rt.mux

	// Unauthenticated: a health check that needs a login is not a health
	// check, and the UI shell holds no data of its own.
	rt.handle(classPublic, "GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	rt.handle(classPublic, "GET /api/v1/me", s.handleMe)
	if authenticator != nil {
		authenticator.Routes(mux)
	}
	// /metrics counts and names things across the whole installation, so a
	// grant never reaches it: global admin, or explicitly opened for a scraper
	// that cannot present a session cookie.
	if opts.MetricsPublic {
		rt.handle(classMetricsPublic, "GET /metrics", s.handleMetrics)
	} else {
		rt.handle(classGlobalAdmin, "GET /metrics", s.handleMetrics)
	}

	rt.handle(classScopedRead, "GET /api/v1/targets", s.handleTargets)
	rt.handle(classScopedWrite, "POST /api/v1/targets", s.handleCreateTarget)
	rt.handle(classScopedWrite, "PATCH /api/v1/targets/{id}", s.handleUpdateTarget)
	rt.handle(classScopedWrite, "DELETE /api/v1/targets/{id}", s.handleDeleteTarget)
	rt.handle(classScopedRead, "GET /api/v1/measurements", s.handleMeasurements)
	rt.handle(classScopedRead, "GET /api/v1/alert-rules", s.handleAlertRules)
	rt.handle(classScopedWrite, "POST /api/v1/alert-rules", s.handleCreateAlertRule)
	rt.handle(classScopedWrite, "PATCH /api/v1/alert-rules/{id}", s.handleUpdateAlertRule)
	rt.handle(classScopedWrite, "DELETE /api/v1/alert-rules/{id}", s.handleDeleteAlertRule)
	rt.handle(classScopedRead, "GET /api/v1/alerts", s.handleFiringAlerts)
	rt.handle(classScopedWrite, "POST /api/v1/alerts/ack", s.handleAckAlert)
	if s.events != nil {
		rt.handle(classScopedRead, "GET /api/v1/alert-events", s.handleAlertEvents)
	}
	rt.handle(classScopedRead, "GET /api/v1/agents", s.handleAgents)
	rt.handle(classScopedRead, "GET /api/v1/paths", s.handlePaths)
	if s.enrol != nil {
		rt.handle(classGlobalAdmin, "PATCH /api/v1/agents/{id}", s.handleUpdateAgent)
		rt.handle(classGlobalAdmin, "DELETE /api/v1/agents/{id}", s.handleDeleteAgent)
		rt.handle(classGlobalAdmin, "GET /api/v1/agent-tokens", s.handleListEnrolTokens)
		rt.handle(classGlobalAdmin, "POST /api/v1/agent-tokens", s.handleMintEnrolToken)
		rt.handle(classGlobalAdmin, "DELETE /api/v1/agent-tokens/{id}", s.handleRevokeEnrolToken)
		// Enrolment carries its own credential, so like the signed agent
		// endpoints it sits outside the session middleware.
		rt.handle(classAgentSigned, "POST /api/v1/agent/enrol", s.handleEnrol)
	}
	if s.config != nil {
		// Declarative over the whole tree, so there is no scoped form of
		// either: global admin, the same as the command line.
		rt.handle(classGlobalAdmin, "GET /api/v1/config", s.handleConfigExport)
		rt.handle(classGlobalAdmin, "PUT /api/v1/config", s.handleConfigImport)
	}
	if s.grants != nil {
		rt.handle(classGlobalAdmin, "GET /api/v1/grants", s.handleGrants)
		rt.handle(classGlobalAdmin, "POST /api/v1/grants", s.handleUpsertGrant)
		rt.handle(classGlobalAdmin, "DELETE /api/v1/grants/{id}", s.handleDeleteGrant)
	}
	// Signed agent endpoints (§9). These carry their own authentication, so
	// they are deliberately outside the session middleware.
	rt.handle(classAgentSigned, "POST /api/v1/ingest", s.handleIngest)
	rt.handle(classAgentSigned, "GET /api/v1/agent/targets", s.handleAgentTargets)
	s.routes = rt
	mux.Handle("/", http.FileServerFS(webFS))
	return &handler{Handler: mux, srv: s}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	// The target tree changes under the caller's feet — an admin edit, a TOML
	// import, the prober's own writes. Without this, a browser is free to
	// serve a heuristically cached copy and show state that no longer exists.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// badRequest reports a caller mistake verbatim: validation messages name the
// offending field and are meant to be shown in the admin UI.
func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

// badRequestMsg reports a caller mistake without wrapping a Go error.
func badRequestMsg(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// internalError logs the detail and returns a generic message.
func internalError(w http.ResponseWriter, err error) {
	log.Printf("api: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
