package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// handleGrants lists who may see what. Global admin only: a grant that could
// widen itself would not be a boundary.
func (s *server) handleGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := s.grants.ListGrants(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	tr, err := tree.New(targets)
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(grants))
	for _, g := range grants {
		item := map[string]any{
			"id": g.ID, "group": g.Group, "target_id": g.TargetID, "role": g.Role,
		}
		if p, err := tr.Path(g.TargetID); err == nil {
			item["path"] = p
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

func (s *server) handleUpsertGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Group    string `json:"group"`
		TargetID int64  `json:"target_id"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		badRequestMsg(w, "malformed request body")
		return
	}
	// A grant of admin would be a global role handed out per subtree, which is
	// not what a grant is. The ladder inside a scope stops at editor.
	if auth.Role(body.Role) == auth.RoleAdmin {
		badRequestMsg(w, "a grant confers viewer or editor; admin is global and comes from the identity provider")
		return
	}
	g := store.Grant{Group: body.Group, TargetID: body.TargetID, Role: body.Role}
	if err := s.grants.UpsertGrant(r.Context(), &g); err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": g.ID, "group": g.Group, "target_id": g.TargetID, "role": g.Role,
	})
}

func (s *server) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequestMsg(w, "id must be a number")
		return
	}
	if err := s.grants.DeleteGrant(r.Context(), id); err != nil {
		badRequest(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
