// Package api serves the HTTP API and the embedded web UI (DESIGN.md §7).
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"

	"smokeng/internal/alert"
	"smokeng/internal/store"
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

type server struct {
	st     AlertStore
	alerts AlertView
}

// New builds the HTTP handler: API routes plus the embedded frontend.
// /metrics (Prometheus self-observability, §7.1) is still to come.
func New(st AlertStore, alerts AlertView, webFS fs.FS) http.Handler {
	s := &server{st: st, alerts: alerts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/v1/targets", s.handleTargets)
	mux.HandleFunc("POST /api/v1/targets", s.handleCreateTarget)
	mux.HandleFunc("PATCH /api/v1/targets/{id}", s.handleUpdateTarget)
	mux.HandleFunc("DELETE /api/v1/targets/{id}", s.handleDeleteTarget)
	mux.HandleFunc("GET /api/v1/measurements", s.handleMeasurements)
	mux.HandleFunc("GET /api/v1/alert-rules", s.handleAlertRules)
	mux.HandleFunc("POST /api/v1/alert-rules", s.handleCreateAlertRule)
	mux.HandleFunc("PATCH /api/v1/alert-rules/{id}", s.handleUpdateAlertRule)
	mux.HandleFunc("DELETE /api/v1/alert-rules/{id}", s.handleDeleteAlertRule)
	mux.HandleFunc("GET /api/v1/alerts", s.handleFiringAlerts)
	// Signed agent ingest (§9); v0.4.
	mux.HandleFunc("POST /api/v1/ingest", notImplemented)
	mux.Handle("/", http.FileServerFS(webFS))
	return mux
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented yet"})
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

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// internalError logs the detail and returns a generic message.
func internalError(w http.ResponseWriter, err error) {
	log.Printf("api: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
