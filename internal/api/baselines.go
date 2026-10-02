package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/tree"
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
		// A pointer, so "not given" is distinguishable from "agent 0". Absent
		// means "whichever agent measures this target", which is the useful
		// default; 0 would name the local prober, and on an installation whose
		// probing runs as its own agent that is a series with no measurements.
		AgentID *int64 `json:"agent_id"`
		From    int64  `json:"from"`
		To      int64  `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}

	// The rule is authorised before anything about it is said: whether it
	// exists, what kind it is and what it would capture are all answered only to
	// someone who may write on the node it is defined on. Capturing a reference
	// changes what the rule fires on, so that is the right to ask for.
	rule, sc, ok := s.ruleFor(w, r, ruleID, true)
	if !ok {
		return
	}
	if rule.Metric != alert.MetricShape || rule.Baseline != alert.BaselineGolden {
		badRequest(w, errors.New("only a shape rule with a golden baseline has a reference to capture"))
		return
	}
	if body.TargetID == 0 {
		body.TargetID = rule.TargetID
	}
	// The target to read from is the rule's own node or something beneath it.
	// It used to be taken from the request as given, so an editor of one
	// customer could point their own rule at another customer's target and
	// have its measurements stored as the reference, then read them back
	// through shape-reference. Anything else is answered as a target that does
	// not exist, whether it does or not.
	if !sc.Within(body.TargetID, rule.TargetID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such target"})
		return
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
	// Every sample in the window is read into memory, concatenated, sorted and
	// stored as one blob, so the window is bounded the way a measurements
	// request is.
	if rows := rangeRows(body.From, body.To); rows > maxRowsPerRequest {
		badRequest(w, fmt.Errorf("that window could hold about %d intervals, more than the %d one capture reads; "+
			"capture a shorter window", rows, maxRowsPerRequest))
		return
	}

	agentID, err := s.captureAgent(r, body.TargetID, body.AgentID)
	if err != nil {
		badRequest(w, err)
		return
	}

	ms, err := s.st.QueryRange(r.Context(), body.TargetID, agentID, body.From, body.To)
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
		RuleID: ruleID, TargetID: body.TargetID, AgentID: agentID,
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

// captureAgent decides which vantage point a reference is captured from. An
// explicit id is honoured; absent, it resolves the first enrolled agent the
// target is actually assigned to. Defaulting to 0 instead would name the local
// prober, which on an installation that probes from its own agent is a series
// with no measurements — a capture that fails for a reason the operator has no
// way to see from the button they pressed.
func (s *server) captureAgent(r *http.Request, targetID int64, want *int64) (int64, error) {
	if want != nil {
		return *want, nil
	}
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		return 0, err
	}
	tr, err := tree.New(targets)
	if err != nil {
		return 0, err
	}
	res, err := tr.Resolve(targetID)
	if err != nil {
		return 0, err
	}
	records, err := s.agents.ListAgents(r.Context())
	if err != nil {
		return 0, err
	}
	byName := map[string]int64{}
	for _, a := range records {
		byName[a.Name] = a.ID
	}
	for _, n := range strings.Fields(res.Agents.Effective) {
		if id, ok := byName[n]; ok {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no enrolled agent measures this target (%s), so there is nothing to capture",
		res.Agents.Effective)
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
	if _, _, ok := s.ruleFor(w, r, ruleID, true); !ok {
		return
	}
	cleared, err := s.alerts.ClearBaseline(r.Context(), ruleID)
	if err != nil {
		internalError(w, err)
		return
	}
	if !cleared {
		notFound(w) // the rule is theirs and has no reference to clear
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
	// Authorise before anything is resolved or said. The target comes first:
	// one the caller cannot see does not exist as far as they are concerned. The
	// rule has to be one that applies to that target, which is the node it is
	// defined on or an ancestor of it (rules inherit downward). Naming a rule
	// that does not exist, one on another customer's node, and a real rule that
	// does not reach this target all get the same answer, because which rule ids
	// exist is itself something this endpoint would otherwise tell.
	sc, _, ok := s.withScope(w, r)
	if !ok {
		return
	}
	if !sc.Visible(targetID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such target"})
		return
	}
	rules, err := s.st.ListAlertRules(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	var rule *alert.Rule
	for i := range rules {
		if rules[i].ID == ruleID && sc.Within(targetID, rules[i].TargetID) {
			rule = &rules[i]
		}
	}
	if rule == nil {
		noSuchRule(w)
		return
	}

	// The caller normally names the vantage point, since a firing alert knows
	// which one it is about. Without it, resolve the target's assigned agent
	// rather than assuming the local prober.
	var agentID int64
	if v := q.Get("agent_id"); v != "" {
		if agentID, err = strconv.ParseInt(v, 10, 64); err != nil {
			badRequest(w, errors.New("bad agent_id"))
			return
		}
	} else {
		if agentID, err = s.captureAgent(r, targetID, nil); err != nil {
			badRequest(w, err)
			return
		}
	}

	reference, kind, avail := s.alerts.ShapeReference(ruleID, targetID, agentID)
	// A golden reference is the rule's, not the target's: one distribution
	// captured from one series and compared against every series the rule
	// reaches. So it is theirs to read only if the series it came from is. A
	// caller who sees part of the rule's subtree is shown a reference captured
	// from a part they cannot see as not available, which is the same thing
	// they would see for a rule nothing has been captured for.
	if kind == "golden" && avail {
		if bs, err := s.alerts.Baselines(r.Context()); err == nil {
			for _, b := range bs {
				if b.RuleID == ruleID && !sc.Visible(b.TargetID) {
					reference, avail = nil, false
				}
			}
		}
	}
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
		"available": avail,
		"reference": reference,
		"current":   current,
	})
}
