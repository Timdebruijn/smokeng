package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// handlePaths returns the route changes for one series over a window, so the
// UI can put "the path changed at 14:02" next to "the smoke widened at
// 14:03". That correlation is the whole reason paths are recorded; there is
// deliberately no standalone traceroute view.
func (s *server) handlePaths(w http.ResponseWriter, r *http.Request) {
	targetID, agentID, family, from, to, ok := s.seriesQuery(w, r)
	if !ok {
		return
	}
	changes, err := s.st.PathChanges(r.Context(), targetID, agentID, family, from, to)
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
	writeJSON(w, http.StatusOK, map[string]any{"family": family.String(), "changes": out})
}

// seriesQuery reads the target_id, agent_id and time range a per-series route is
// asked for and checks the caller may see that target. The series is read over
// the family the target is measured over now, unless family= names another
// (see familyParam). It has answered the request when ok is false.
func (s *server) seriesQuery(w http.ResponseWriter, r *http.Request) (targetID, agentID int64, family tree.Family, from, to int64, ok bool) {
	q := r.URL.Query()
	targetID, err := strconv.ParseInt(q.Get("target_id"), 10, 64)
	if err != nil {
		badRequestMsg(w, "target_id is required")
		return
	}
	agentID = store.LocalAgentID
	if v := q.Get("agent_id"); v != "" {
		if agentID, err = strconv.ParseInt(v, 10, 64); err != nil {
			badRequestMsg(w, "bad agent_id")
			return
		}
	}
	from, to, err = timeRange(q.Get("from"), q.Get("to"))
	if err != nil {
		badRequestMsg(w, err.Error())
		return
	}
	fam, named, ok := familyParam(w, q)
	if !ok {
		return
	}
	sc, ok := s.requireVisible(w, r, targetID)
	if !ok {
		return
	}
	family = fam
	if !named {
		family = sc.tr.FamilyOf(targetID)
	}
	return
}

// familyParam reads the optional family= of a series route. named is false when
// it is absent, and the caller then reads the target's current family. It is
// parsed before the scope check but only picks which of a visible target's
// series is read, so it opens nothing the caller could not read before. It has
// answered the request when ok is false.
func familyParam(w http.ResponseWriter, q url.Values) (family tree.Family, named, ok bool) {
	if !q.Has("family") {
		return 0, false, true
	}
	family, ok = tree.ParseFamily(q.Get("family"))
	if !ok {
		badRequestMsg(w, "family must be v4, v6 or unknown")
		return 0, false, false
	}
	return family, true, true
}
