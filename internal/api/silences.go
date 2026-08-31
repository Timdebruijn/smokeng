package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// handleListSilences reports every silence, filtered by scope: a targeted
// silence is shown to those who can see its target, a global one to anyone who
// can read.
func (s *server) handleListSilences(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"silences": []any{}})
		return
	}
	sc, _, ok := s.withScope(w, r)
	if !ok {
		return
	}
	sils, err := s.alerts.ListSilences(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	now := time.Now().Unix()
	out := make([]map[string]any, 0, len(sils))
	for _, sl := range sils {
		if sl.TargetID != nil && !sc.Visible(*sl.TargetID) {
			continue
		}
		item := map[string]any{
			"id":         sl.ID,
			"starts_at":  sl.StartsAt,
			"ends_at":    sl.EndsAt,
			"reason":     sl.Reason,
			"created_by": sl.CreatedBy,
			"created_at": sl.CreatedAt,
			// Derived state the UI would otherwise recompute: active now, or a
			// window still in the future, or already past.
			"active":   now >= sl.StartsAt && now < sl.EndsAt,
			"upcoming": now < sl.StartsAt,
		}
		if sl.TargetID != nil {
			item["target_id"] = *sl.TargetID
			if p, err := sc.PathIn(*sl.TargetID); err == nil {
				item["target"] = p
			}
		}
		if sl.AgentID != nil {
			item["agent_id"] = *sl.AgentID
		}
		if sl.RuleID != nil {
			item["rule_id"] = *sl.RuleID
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"silences": out})
}

// handleCreateSilence books a silence. Two shapes are accepted: an explicit
// [starts_at, ends_at) window (a maintenance window planned ahead), or a
// duration_s from now (the quick "silence this for two hours"). Scope defaults
// to everything; narrowing it to a target and its subtree, an agent or a rule
// is optional.
func (s *server) handleCreateSilence(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		notFound(w)
		return
	}
	var body struct {
		TargetID  *int64 `json:"target_id"`
		AgentID   *int64 `json:"agent_id"`
		RuleID    *int64 `json:"rule_id"`
		StartsAt  int64  `json:"starts_at"`
		EndsAt    int64  `json:"ends_at"`
		DurationS int64  `json:"duration_s"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}

	now := time.Now().Unix()
	sil := alert.Silence{
		TargetID: body.TargetID,
		AgentID:  body.AgentID,
		RuleID:   body.RuleID,
		StartsAt: body.StartsAt,
		EndsAt:   body.EndsAt,
		Reason:   body.Reason,
	}
	if sil.StartsAt == 0 {
		sil.StartsAt = now
	}
	// duration_s is sugar over ends_at, from the start: the quick-silence UI has
	// a duration, not a clock time, and computing the end here keeps the two
	// request shapes from disagreeing.
	if sil.EndsAt == 0 && body.DurationS > 0 {
		sil.EndsAt = sil.StartsAt + body.DurationS
	}
	if sil.EndsAt <= sil.StartsAt {
		badRequest(w, errors.New("a silence needs an end after its start: set ends_at, or duration_s"))
		return
	}

	// Authorised on the target it covers — a global silence needs write at the
	// root, which is the same as saying it needs an editor of everything.
	sc, targets, ok := s.withScope(w, r)
	if !ok {
		return
	}
	scopeID := rootTargetID(targets)
	if sil.TargetID != nil {
		scopeID = *sil.TargetID
	}
	if !sc.CanWrite(scopeID) {
		sc.deny(w, scopeID)
		return
	}
	sil.CreatedBy = s.callerName(r)

	created, err := s.alerts.AddSilence(r.Context(), sil)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": created.ID})
}

// handleDeleteSilence lifts a silence early. Deleting one is a write on the
// target it covers, so an editor of that subtree may cancel it and a viewer may
// not.
func (s *server) handleDeleteSilence(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		notFound(w)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("bad silence id"))
		return
	}
	sils, err := s.alerts.ListSilences(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	var found *alert.Silence
	for i := range sils {
		if sils[i].ID == id {
			found = &sils[i]
			break
		}
	}
	if found == nil {
		notFound(w)
		return
	}
	sc, targets, ok := s.withScope(w, r)
	if !ok {
		return
	}
	scopeID := rootTargetID(targets)
	if found.TargetID != nil {
		scopeID = *found.TargetID
	}
	if !sc.CanWrite(scopeID) {
		sc.deny(w, scopeID)
		return
	}
	ok, err = s.alerts.RemoveSilence(r.Context(), id)
	if err != nil {
		internalError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// rootTargetID is the id of the tree's root, the scope a global silence is
// authorised against. The root always exists; -1 is a scope nobody can write,
// which fails closed if it somehow does not.
func rootTargetID(targets []tree.Target) int64 {
	for i := range targets {
		if targets[i].ParentID == nil {
			return targets[i].ID
		}
	}
	return -1
}
