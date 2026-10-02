package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/timdebruijn/smokeng/internal/alert"
)

// handleAlertRules lists every rule with the node it is defined on. Which
// rules apply to a given target is inheritance, resolved server-side by the
// alert manager; the list here is the definitions themselves.
func (s *server) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.withScope(w, r)
	if !ok {
		return
	}
	rules, err := s.st.ListAlertRules(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rules))
	for i := range rules {
		// A rule names the node it is defined on, so an unfiltered list would
		// enumerate the tree for anyone allowed to read any of it.
		if !sc.Visible(rules[i].TargetID) {
			continue
		}
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
		"mode":            string(r.Mode),
		"baseline":        string(r.Baseline),
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
	Mode      *string  `json:"mode"`
	Baseline  *string  `json:"baseline"`
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
	if p.Mode != nil {
		r.Mode = alert.Mode(*p.Mode)
	}
	if p.Baseline != nil {
		r.Baseline = alert.Baseline(*p.Baseline)
	}
}

// noSuchRule is the one answer for a rule the caller may not know about, which
// is also the answer for a rule that does not exist. They used to differ — a
// missing rule was "not found" and an invisible one was refused as a target the
// caller could not see — so asking for rule ids one after another told them
// which ones were real, and whose.
func noSuchRule(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such rule"})
}

// ruleFor finds the rule an endpoint addresses and authorises the caller
// against the node it is defined on, before anything else about it is said. A
// rule on a node they cannot see is answered as absent; one they can see but
// not change is forbidden, if the endpoint writes.
func (s *server) ruleFor(w http.ResponseWriter, r *http.Request, id int64, write bool) (*alert.Rule, *Scope, bool) {
	sc, _, ok := s.withScope(w, r)
	if !ok {
		return nil, nil, false
	}
	rules, err := s.st.ListAlertRules(r.Context())
	if err != nil {
		internalError(w, err)
		return nil, nil, false
	}
	for i := range rules {
		if rules[i].ID != id {
			continue
		}
		rule := &rules[i]
		if !sc.Visible(rule.TargetID) {
			break
		}
		if write && !sc.CanWrite(rule.TargetID) {
			sc.deny(w, rule.TargetID)
			return nil, nil, false
		}
		return rule, sc, true
	}
	noSuchRule(w)
	return nil, nil, false
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
	// A rule is defined on a tree node, so writing one is a write to that node.
	if _, ok := s.requireWrite(w, r, rule.TargetID); !ok {
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
	rule, sc, ok := s.ruleFor(w, r, id, true)
	if !ok {
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
	// Moving a rule to another node is a write to that node too.
	if !sc.CanWrite(rule.TargetID) {
		sc.deny(w, rule.TargetID)
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
	if _, _, ok := s.ruleFor(w, r, id, true); !ok {
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
		writeJSON(w, http.StatusOK, map[string]any{
			"alerts": []any{}, "enabled": false, "delivering": false,
		})
		return
	}
	sc, _, ok := s.withScope(w, r)
	if !ok {
		return
	}
	firing := s.alerts.Firing()
	out := make([]map[string]any, 0, len(firing))
	for _, a := range firing {
		if !sc.Visible(a.Rule.TargetID) {
			continue
		}
		// The path the manager recorded runs from the real root. Rendered as
		// this caller sees it; a target that no longer exists has no path to
		// render, and the recorded one is not a fallback for anyone but an
		// admin, who is allowed to see all of it.
		path, err := sc.PathIn(a.TargetID)
		if err != nil {
			path = ""
			if sc.IsGlobalAdmin() {
				path = a.TargetPath
			}
		}
		item := map[string]any{
			"rule":      a.Rule.Name,
			"metric":    string(a.Rule.Metric),
			"target":    path,
			"host":      a.TargetHost,
			"agent":     a.AgentName,
			"value":     a.Value,
			"describes": a.Rule.Describe(),
			// The identifiers the acknowledge endpoint needs — the names alone
			// cannot address one (target, agent) pair.
			"rule_id":   a.Rule.ID,
			"target_id": a.TargetID,
			"agent_id":  a.AgentID,
			"acked":     a.Acked,
			"silenced":  a.Silenced,
		}
		if !a.Since.IsZero() {
			item["since"] = a.Since.Unix()
		}
		if a.Acked {
			item["acked_by"] = a.AckedBy
			if !a.AckedAt.IsZero() {
				item["acked_at"] = a.AckedAt.Unix()
			}
		}
		if a.Silenced && !a.SilencedUntil.IsZero() {
			item["silenced_until"] = a.SilencedUntil.Unix()
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"alerts": out, "enabled": true, "delivering": s.alerts.Delivering(),
	})
}

// EventStore is the slice of the store alert history needs. Optional, like the
// rest: an instance whose store cannot keep history simply has no endpoint.
type EventStore interface {
	ListAlertEvents(ctx context.Context, limit int) ([]alert.Event, error)
}

// handleAlertEvents reports what alerting has done, as opposed to what it is
// doing now. It is filtered by scope for the same reason the rule list is: an
// event names the node it happened on.
func (s *server) handleAlertEvents(w http.ResponseWriter, r *http.Request) {
	sc, targets, ok := s.withScope(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.events.ListAlertEvents(r.Context(), limit)
	if err != nil {
		internalError(w, err)
		return
	}
	paths := map[int64]string{}
	for i := range targets {
		if p, err := sc.PathIn(targets[i].ID); err == nil {
			paths[targets[i].ID] = p
		}
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		if !sc.Visible(e.TargetID) {
			continue
		}
		out = append(out, map[string]any{
			"id": e.ID, "ts": e.TS, "firing": e.Firing,
			"rule": e.RuleName, "describes": e.Describes, "value": e.Value,
			"target": paths[e.TargetID], "agent_id": e.AgentID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// handleAckAlert marks a firing alert acknowledged, or clears the mark. The
// alert keeps firing and delivery is untouched — this only quiets the UI's own
// attention, so a person can say "seen, handling it" without editing the rule
// or waiting for the condition to resolve.
func (s *server) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		notFound(w)
		return
	}
	var body struct {
		RuleID   int64 `json:"rule_id"`
		TargetID int64 `json:"target_id"`
		AgentID  int64 `json:"agent_id"`
		Ack      *bool `json:"ack"` // absent means acknowledge; false clears it
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	// Write on the target the alert is about: acknowledging is an operational
	// action on shared state, so an editor of that subtree may do it and a
	// viewer may not.
	if _, ok := s.requireWrite(w, r, body.TargetID); !ok {
		return
	}
	ack := body.Ack == nil || *body.Ack
	changed, err := s.alerts.Acknowledge(r.Context(), body.RuleID, body.TargetID, body.AgentID, ack, s.callerName(r))
	if err != nil {
		internalError(w, err)
		return
	}
	if !changed {
		// Nothing firing for that (rule, target, agent) — it may have resolved
		// between the page loading and the click. Say so rather than imply an
		// ack landed on a problem that is over.
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acked": ack})
}

// callerName is a human label for the acknowledging user, for display and
// audit. Empty on an unauthenticated instance, where there is no identity to
// record and none is claimed.
func (s *server) callerName(r *http.Request) string {
	if s.auth == nil {
		return ""
	}
	sess, ok := s.auth.SessionFrom(r)
	if !ok {
		return ""
	}
	if sess.Email != "" {
		return sess.Email
	}
	if sess.Name != "" {
		return sess.Name
	}
	return sess.Subject
}
