package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/timdebruijn/smokeng/internal/report"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// handleAvailability computes uptime over a window for one target, per vantage
// point. It keeps two numbers apart on purpose: availability is over the
// intervals there is data for, and coverage is how much of the window that was —
// so "100% available" over a half-covered window cannot masquerade as the same
// claim as one over a full one.
func (s *server) handleAvailability(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	targetID, err := strconv.ParseInt(q.Get("target_id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("target_id is required"))
		return
	}
	// A node outside the caller's scope is answered as absent, like everywhere.
	if !s.requireVisible(w, r, targetID) {
		return
	}
	from, to, err := timeRange(q.Get("from"), q.Get("to"))
	if err != nil {
		badRequest(w, err)
		return
	}
	// The loss at which an interval counts as down. 100 (the default) means only
	// a total blackout is down; a lower SLA threshold counts heavy loss as down.
	threshold := 100.0
	if v := q.Get("down_threshold"); v != "" {
		t, err := strconv.ParseFloat(v, 64)
		if err != nil || t <= 0 || t > 100 {
			badRequest(w, errors.New("down_threshold must be between 0 (exclusive) and 100"))
			return
		}
		threshold = t
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
	res, err := tr.Resolve(targetID)
	if err != nil {
		notFound(w)
		return
	}
	intervalS := res.IntervalS.Effective
	path, _ := tr.Path(targetID)

	// Which vantage points to report. Default is every agent the target is
	// assigned to, so an assigned-but-silent one shows as coverage 0 rather than
	// vanishing; a single agent_id narrows it to one.
	agentRecords, err := s.agents.ListAgents(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	idByName := map[string]int64{}
	nameByID := map[int64]string{}
	for _, a := range agentRecords {
		idByName[a.Name] = a.ID
		nameByID[a.ID] = a.Name
	}

	type vantage struct {
		id   int64
		name string
	}
	var wants []vantage
	if v := q.Get("agent_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			badRequest(w, errors.New("bad agent_id"))
			return
		}
		wants = append(wants, vantage{id, nameByID[id]})
	} else {
		for _, n := range strings.Fields(res.Agents.Effective) {
			if id, ok := idByName[n]; ok {
				wants = append(wants, vantage{id, n})
			}
		}
	}

	agentsOut := make([]map[string]any, 0, len(wants))
	for _, v := range wants {
		points, err := s.st.AvailabilitySeries(r.Context(), targetID, v.id, from, to)
		if err != nil {
			internalError(w, err)
			return
		}
		agentsOut = append(agentsOut, map[string]any{
			"agent_id": v.id,
			"agent":    v.name,
			"report":   report.Availability(points, intervalS, from, to, threshold),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"target":             path,
		"target_id":          targetID,
		"from":               from,
		"to":                 to,
		"interval_s":         intervalS,
		"down_threshold_pct": threshold,
		"agents":             agentsOut,
	})
}
