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
}

// Store is everything the API persists through: the measurement store plus
// the alert and agent tables.
type Store interface {
	AlertStore
	AgentStore
	PathStore
}

type server struct {
	st       Store
	alerts   AlertView
	auth     Authenticator
	agents   AgentStore
	enrol    EnrolStore
	verifier *ingest.Verifier
	probe    ProbeStats
	ingest   IngestStats
	version  string
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
}

// New builds the HTTP handler: API routes plus the embedded frontend.
// authenticator may be nil, in which case every request is treated as an
// admin — permitted only on loopback, which serve enforces.
// /metrics (Prometheus self-observability, §7.1) is still to come.
func New(st Store, opts Options, webFS fs.FS) http.Handler {
	authenticator := opts.Auth
	s := &server{
		st: st, alerts: opts.Alerts, auth: authenticator, agents: st,
		probe: opts.Probe, version: opts.Version,
	}
	// Enrolment is optional in the same way the local prober is: a store that
	// cannot mint tokens simply does not get the routes, rather than forcing
	// every implementation of Store to grow seven methods it may not want.
	if es, ok := st.(EnrolStore); ok {
		s.enrol = es
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
	viewer := func(h http.HandlerFunc) http.HandlerFunc { return s.requireRole(auth.RoleViewer, h) }
	admin := func(h http.HandlerFunc) http.HandlerFunc { return s.requireRole(auth.RoleAdmin, h) }

	mux := http.NewServeMux()
	// Unauthenticated: a health check that needs a login is not a health
	// check, and the UI shell holds no data of its own.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/v1/me", s.handleMe)
	if authenticator != nil {
		authenticator.Routes(mux)
	}
	// /metrics carries no measurement data, but it does name agents and count
	// targets, so it stays behind the session unless explicitly opened for a
	// scraper.
	if opts.MetricsPublic {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
	} else {
		mux.HandleFunc("GET /metrics", viewer(s.handleMetrics))
	}

	mux.HandleFunc("GET /api/v1/targets", viewer(s.handleTargets))
	mux.HandleFunc("POST /api/v1/targets", admin(s.handleCreateTarget))
	mux.HandleFunc("PATCH /api/v1/targets/{id}", admin(s.handleUpdateTarget))
	mux.HandleFunc("DELETE /api/v1/targets/{id}", admin(s.handleDeleteTarget))
	mux.HandleFunc("GET /api/v1/measurements", viewer(s.handleMeasurements))
	mux.HandleFunc("GET /api/v1/alert-rules", viewer(s.handleAlertRules))
	mux.HandleFunc("POST /api/v1/alert-rules", admin(s.handleCreateAlertRule))
	mux.HandleFunc("PATCH /api/v1/alert-rules/{id}", admin(s.handleUpdateAlertRule))
	mux.HandleFunc("DELETE /api/v1/alert-rules/{id}", admin(s.handleDeleteAlertRule))
	mux.HandleFunc("GET /api/v1/alerts", viewer(s.handleFiringAlerts))
	mux.HandleFunc("GET /api/v1/agents", viewer(s.handleAgents))
	if s.enrol != nil {
		mux.HandleFunc("PATCH /api/v1/agents/{id}", admin(s.handleUpdateAgent))
		mux.HandleFunc("DELETE /api/v1/agents/{id}", admin(s.handleDeleteAgent))
		mux.HandleFunc("GET /api/v1/agent-tokens", admin(s.handleListEnrolTokens))
		mux.HandleFunc("POST /api/v1/agent-tokens", admin(s.handleMintEnrolToken))
		mux.HandleFunc("DELETE /api/v1/agent-tokens/{id}", admin(s.handleRevokeEnrolToken))
		// Enrolment carries its own credential, so like the signed agent
		// endpoints it sits outside the session middleware.
		mux.HandleFunc("POST /api/v1/agent/enrol", s.handleEnrol)
	}
	mux.HandleFunc("GET /api/v1/paths", viewer(s.handlePaths))
	// Signed agent endpoints (§9). These carry their own authentication, so
	// they are deliberately outside the session middleware.
	mux.HandleFunc("POST /api/v1/ingest", s.handleIngest)
	mux.HandleFunc("GET /api/v1/agent/targets", s.handleAgentTargets)
	mux.Handle("/", http.FileServerFS(webFS))
	return mux
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
