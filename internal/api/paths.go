package api

import (
	"net/http"
	"strconv"
	"strings"

	"smokeng/internal/store"
)

// handlePaths returns the route changes for one series over a window, so the
// UI can put "the path changed at 14:02" next to "the smoke widened at
// 14:03". That correlation is the whole reason paths are recorded; there is
// deliberately no standalone traceroute view.
func (s *server) handlePaths(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	targetID, err := strconv.ParseInt(q.Get("target_id"), 10, 64)
	if err != nil {
		badRequestMsg(w, "target_id is required")
		return
	}
	agentID := store.LocalAgentID
	if v := q.Get("agent_id"); v != "" {
		if agentID, err = strconv.ParseInt(v, 10, 64); err != nil {
			badRequestMsg(w, "bad agent_id")
			return
		}
	}
	from, to, err := timeRange(q.Get("from"), q.Get("to"))
	if err != nil {
		badRequestMsg(w, err.Error())
		return
	}

	changes, err := s.st.PathChanges(r.Context(), targetID, agentID, from, to)
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		out = append(out, map[string]any{
			"ts":   c.TS,
			"hops": strings.Split(c.Hops, ","),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out})
}
