package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"smokeng/internal/alert"
)

// handleAlertRules lists every rule with the node it is defined on. Which
// rules apply to a given target is inheritance, resolved server-side by the
// alert manager; the list here is the definitions themselves.
func (s *server) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.st.ListAlertRules(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rules))
	for i := range rules {
		out = append(out, ruleJSON(&rules[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func ruleJSON(r *alert.Rule) map[string]any {
	return map[string]any{
		"id":              r.ID,
		"target_id":       r.TargetID,
		"name":            r.Name,
		"metric":          string(r.Metric),
		"op":              string(r.Op),
		"threshold":       r.Threshold,
		"for_intervals":   r.For,
		"clear_intervals": r.ClearFor,
		"enabled":         r.Enabled,
		"describes":       r.Describe(),
	}
}

// rulePayload is the wire form of a rule. Defaults are deliberate: a rule
// created without hysteresis would flap, so `for` and `clear_for` default to
// three intervals rather than one.
type rulePayload struct {
	TargetID  *int64   `json:"target_id"`
	Name      *string  `json:"name"`
	Metric    *string  `json:"metric"`
	Op        *string  `json:"op"`
	Threshold *float64 `json:"threshold"`
	For       *int     `json:"for_intervals"`
	ClearFor  *int     `json:"clear_intervals"`
	Enabled   *bool    `json:"enabled"`
}

func (p *rulePayload) applyTo(r *alert.Rule) {
	if p.TargetID != nil {
		r.TargetID = *p.TargetID
	}
	if p.Name != nil {
		r.Name = *p.Name
	}
	if p.Metric != nil {
		r.Metric = alert.Metric(*p.Metric)
	}
	if p.Op != nil {
		r.Op = alert.Op(*p.Op)
	}
	if p.Threshold != nil {
		r.Threshold = *p.Threshold
	}
	if p.For != nil {
		r.For = *p.For
	}
	if p.ClearFor != nil {
		r.ClearFor = *p.ClearFor
	}
	if p.Enabled != nil {
		r.Enabled = *p.Enabled
	}
}

func (s *server) handleCreateAlertRule(w http.ResponseWriter, r *http.Request) {
	var p rulePayload
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&p); err != nil {
		badRequest(w, err)
		return
	}
	rule := alert.Rule{For: 3, ClearFor: 3, Enabled: true}
	p.applyTo(&rule)
	if rule.TargetID == 0 {
		badRequest(w, errors.New("target_id is required: a rule is defined on a tree node"))
		return
	}
	if err := rule.Validate(); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.st.UpsertAlertRule(r.Context(), &rule); err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ruleJSON(&rule))
}

func (s *server) handleUpdateAlertRule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("bad rule id"))
		return
	}
	rules, err := s.st.ListAlertRules(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	var rule *alert.Rule
	for i := range rules {
		if rules[i].ID == id {
			rule = &rules[i]
		}
	}
	if rule == nil {
		notFound(w)
		return
	}
	var p rulePayload
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&p); err != nil {
		badRequest(w, err)
		return
	}
	p.applyTo(rule)
	if err := rule.Validate(); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.st.UpsertAlertRule(r.Context(), rule); err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ruleJSON(rule))
}

func (s *server) handleDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("bad rule id"))
		return
	}
	if err := s.st.DeleteAlertRule(r.Context(), id); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleFiringAlerts reports what is currently firing, as the manager sees
// it — the same view the webhook has been told about.
func (s *server) handleFiringAlerts(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"alerts": []any{}, "enabled": false})
		return
	}
	firing := s.alerts.Firing()
	out := make([]map[string]any, 0, len(firing))
	for _, a := range firing {
		item := map[string]any{
			"rule":      a.Rule.Name,
			"metric":    string(a.Rule.Metric),
			"target":    a.TargetPath,
			"host":      a.TargetHost,
			"agent":     a.AgentName,
			"value":     a.Value,
			"describes": a.Rule.Describe(),
		}
		if !a.Since.IsZero() {
			item["since"] = a.Since.Unix()
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out, "enabled": true})
}
