// Package api serves the HTTP API and the embedded web UI (DESIGN.md §7).
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"

	"smokeng/internal/store"
	"smokeng/internal/tree"
)

type server struct {
	st store.Store
}

// New builds the HTTP handler: API routes plus the embedded frontend.
// /metrics (Prometheus self-observability, §7.1) is added with the prober.
func New(st store.Store, webFS fs.FS) http.Handler {
	s := &server{st: st}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/v1/targets", s.handleTargets)
	mux.HandleFunc("GET /api/v1/measurements", s.handleMeasurements)
	// Signed agent ingest (§9); v0.4.
	mux.HandleFunc("POST /api/v1/ingest", notImplemented)
	mux.Handle("/", http.FileServerFS(webFS))
	return mux
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented yet"})
}

// handleTargets returns the full tree. Every inheritable setting is a
// {local, effective, source} object (DESIGN.md §4.2) — never a flat value.
func (s *server) handleTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		internalError(w, r.Context(), err)
		return
	}
	tr, err := tree.New(targets)
	if err != nil {
		internalError(w, r.Context(), err)
		return
	}
	out := make([]map[string]any, 0, len(targets))
	for i := range targets {
		n := &targets[i]
		res, err := tr.Resolve(n.ID)
		if err != nil {
			internalError(w, r.Context(), err)
			return
		}
		path, _ := tr.Path(n.ID)
		out = append(out, map[string]any{
			"id":             n.ID,
			"parent_id":      n.ParentID,
			"name":           n.Name,
			"path":           path,
			"host":           n.Host,
			"address_family": n.AddressFamily,
			"title":          n.Title,
			"notes":          n.Notes,
			"hidden":         n.Hidden,
			"enabled":        n.Enabled,
			"sort_order":     n.SortOrder,
			"settings": map[string]any{
				"interval_s":         settingJSON(n.ID, res.IntervalS),
				"pings_per_interval": settingJSON(n.ID, res.PingsPerInterval),
				"probe_mode":         settingJSON(n.ID, res.ProbeMode),
				"burst_gap_ms":       settingJSON(n.ID, res.BurstGapMS),
				"timeout_ms":         settingJSON(n.ID, res.TimeoutMS),
				"packet_size":        settingJSON(n.ID, res.PacketSize),
				"dscp":               settingJSON(n.ID, res.DSCP),
				"agents":             settingJSON(n.ID, res.Agents),
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

// settingJSON renders one resolved setting: source is the literal string
// "local" when the node sets the value itself, else the providing ancestor.
func settingJSON[T any](nodeID int64, v tree.Value[T]) map[string]any {
	src := any("local")
	if v.Source.ID != nodeID {
		src = v.Source
	}
	return map[string]any{"local": v.Local, "effective": v.Effective, "source": src}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func internalError(w http.ResponseWriter, ctx context.Context, err error) {
	log.Printf("api: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	_ = ctx
}
