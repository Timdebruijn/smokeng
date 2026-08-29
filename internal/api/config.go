package api

import (
	"io"
	"net/http"

	"github.com/timdebruijn/smokeng/internal/config"
)

// maxConfigBody caps an uploaded TOML file. A target tree is text; anything
// larger than this is a mistake or an attempt.
const maxConfigBody = 4 << 20

// ConfigStore is the store an import or export runs against.
type ConfigStore interface {
	config.Store
}

// handleConfigExport writes the whole tree as TOML.
//
// Global admin only, and not because the data is secret — a scoped user can
// already read their own subtree — but because export is defined over the
// entire tree. A partial export would round-trip into an import that deletes
// everything it could not see.
func (s *server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	out, err := config.Export(r.Context(), s.config)
	if err != nil {
		internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="targets.toml"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out)
}

// handleConfigImport applies a TOML file, declaratively, to the whole tree.
//
// The same reasoning as export, and more sharply: an import says what the tree
// *is*, so running one with a partial view would disable everything outside
// it. There is no scoped form of this operation, which is why there is no
// scoped route for it.
func (s *server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigBody))
	if err != nil {
		badRequestMsg(w, "could not read the request body")
		return
	}
	var opts []config.Option
	if r.URL.Query().Get("allow_unknown_agents") == "1" {
		opts = append(opts, config.AllowUnknownAgents())
	}

	// Deliberately never prunes. Absence disables here, which is recoverable
	// by importing the file again; pruning deletes, which is not. That switch
	// stays on the command line, where the operator is looking at the file
	// they are about to apply and can read the summary before running it
	// again.
	sum, err := config.Import(r.Context(), s.config, body, false, opts...)
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summaryJSON(sum))
}

func summaryJSON(sum config.Summary) map[string]any {
	return map[string]any{
		"summary":  sum.String(),
		"warnings": sum.Warnings,
		"targets": map[string]int{
			"created": sum.Created, "updated": sum.Updated,
			"disabled": sum.Disabled, "deleted": sum.Deleted,
		},
		"rules": map[string]int{
			"created": sum.RulesCreated, "updated": sum.RulesUpdated,
			"disabled": sum.RulesDisabled, "deleted": sum.RulesDeleted,
		},
		"grants": map[string]int{
			"created": sum.GrantsCreated, "updated": sum.GrantsUpdated,
			"removed": sum.GrantsRemoved,
		},
	}
}
