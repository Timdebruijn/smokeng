package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
)

// handleListBaselines reports the captured reference distribution of every
// golden-baseline shape rule the caller can see — what it was taken from, not
// the samples, which are large and only meaningful drawn.
func (s *server) handleListBaselines(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"baselines": []any{}})
		return
	}
	sc, _, ok := s.withScope(w, r)
	if !ok {
		return
	}
	bs, err := s.alerts.Baselines(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(bs))
	for _, b := range bs {
		if !sc.Visible(b.TargetID) {
			continue
		}
		out = append(out, map[string]any{
			"rule_id": b.RuleID, "target_id": b.TargetID, "agent_id": b.AgentID,
			"from_ts": b.FromTS, "to_ts": b.ToTS, "intervals": b.Intervals,
			"samples":     len(b.Samples),
			"captured_at": b.CapturedAt, "captured_by": b.CapturedBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"baselines": out})
}

// handleCaptureBaseline records the distribution measured over a window as a
// shape rule's reference: "this is what good looks like". The window is read
// from the stored measurements rather than supplied by the caller, so a baseline
// is always something smokeng actually measured.
func (s *server) handleCaptureBaseline(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		notFound(w)
		return
	}
	ruleID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("bad rule id"))
		return
	}
	var body struct {
		TargetID int64 `json:"target_id"`
		AgentID  int64 `json:"agent_id"`
		From     int64 `json:"from"`
		To       int64 `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}

	rules, err := s.st.ListAlertRules(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	var rule *alert.Rule
	for i := range rules {
		if rules[i].ID == ruleID {
			rule = &rules[i]
		}
	}
	if rule == nil {
		notFound(w)
		return
	}
	if rule.Metric != alert.MetricShape || rule.Baseline != alert.BaselineGolden {
		badRequest(w, errors.New("only a shape rule with a golden baseline has a reference to capture"))
		return
	}
	// Capturing a reference changes what the rule fires on, so it is a write on
	// the node the rule is defined on.
	if _, ok := s.requireWrite(w, r, rule.TargetID); !ok {
		return
	}
	if body.TargetID == 0 {
		body.TargetID = rule.TargetID
	}
	if body.To == 0 {
		body.To = time.Now().Unix()
	}
	if body.From == 0 {
		body.From = body.To - 3600
	}
	if body.From >= body.To {
		badRequest(w, errors.New("the capture window must end after it starts"))
		return
	}

	ms, err := s.st.QueryRange(r.Context(), body.TargetID, body.AgentID, body.From, body.To)
	if err != nil {
		internalError(w, err)
		return
	}
	var samples []uint32
	for i := range ms {
		samples = append(samples, ms[i].Samples...)
	}
	if len(samples) == 0 {
		badRequest(w, errors.New("no measurements in that window: there is nothing to capture as a reference"))
		return
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	b := alert.Baselined{
		RuleID: ruleID, TargetID: body.TargetID, AgentID: body.AgentID,
		FromTS: body.From, ToTS: body.To, Intervals: len(ms), Samples: samples,
		CapturedAt: time.Now().Unix(), CapturedBy: s.callerName(r),
	}
	if err := s.alerts.CaptureBaseline(r.Context(), b); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rule_id": ruleID, "intervals": b.Intervals, "samples": len(samples),
		"from_ts": b.FromTS, "to_ts": b.ToTS,
	})
}

// handleClearBaseline drops a rule's captured reference. The rule then has
// nothing to compare against and stops firing, which is the honest outcome.
func (s *server) handleClearBaseline(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		notFound(w)
		return
	}
	ruleID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
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
		if rules[i].ID == ruleID {
			rule = &rules[i]
		}
	}
	if rule == nil {
		notFound(w)
		return
	}
	if _, ok := s.requireWrite(w, r, rule.TargetID); !ok {
		return
	}
	ok, err := s.alerts.ClearBaseline(r.Context(), ruleID)
	if err != nil {
		internalError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": ruleID})
}

// handleShapeReference returns the two distributions a fired shape alert is
// about: the reference it is compared against, and the current interval. A
// z-score is a claim; these are the evidence, and the UI draws them together so
// a person can see what changed rather than take the number on faith.
func (s *server) handleShapeReference(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		notFound(w)
		return
	}
	q := r.URL.Query()
	ruleID, err := strconv.ParseInt(q.Get("rule_id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("rule_id is required"))
		return
	}
	targetID, err := strconv.ParseInt(q.Get("target_id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("target_id is required"))
		return
	}
	agentID := int64(0)
	if v := q.Get("agent_id"); v != "" {
		if agentID, err = strconv.ParseInt(v, 10, 64); err != nil {
			badRequest(w, errors.New("bad agent_id"))
			return
		}
	}
	if !s.requireVisible(w, r, targetID) {
		return
	}

	reference, kind, ok := s.alerts.ShapeReference(ruleID, targetID, agentID)
	// The current side: the most recent interval measured for this series.
	to := time.Now().Unix()
	ms, err := s.st.QueryRange(r.Context(), targetID, agentID, to-6*3600, to)
	if err != nil {
		internalError(w, err)
		return
	}
	var current []uint32
	if len(ms) > 0 {
		current = ms[len(ms)-1].Samples
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rule_id": ruleID, "target_id": targetID, "agent_id": agentID,
		"kind":      kind,
		"available": ok,
		"reference": reference,
		"current":   current,
	})
}
