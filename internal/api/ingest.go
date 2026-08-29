package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/timdebruijn/smokeng/internal/ingest"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// maxIngestBody caps a submission. A batch is a few tens of bytes per
// measurement, so this is orders of magnitude above any honest agent.
const maxIngestBody = 8 << 20

// PathStore reads the route change log.
type PathStore interface {
	PathChanges(ctx context.Context, targetID, agentID, from, to int64) ([]store.PathChange, error)
}

// AgentStore is the persistence the agent endpoints need.
type AgentStore interface {
	ListAgents(ctx context.Context) ([]store.AgentRecord, error)
	TouchAgent(ctx context.Context, id, at int64, version string) error
}

// agentAuth verifies a signed agent request and returns the agent it came
// from. A rejection is answered with one generic status: telling a caller
// which check failed hands them an oracle for guessing at the next one.
func (s *server) agentAuth(w http.ResponseWriter, r *http.Request) (ingest.Agent, []byte, bool) {
	if s.verifier == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return ingest.Agent{}, nil, false
	}
	parsed, err := ingest.Parse(r, maxIngestBody)
	if err != nil {
		log.Printf("ingest: rejected a request from %s: %v", s.clientIP(r), err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "rejected"})
		return ingest.Agent{}, nil, false
	}
	agent, err := s.verifier.Check(parsed, time.Now())
	if err != nil {
		// The reason goes to the log, with the agent id, exactly as designed.
		log.Printf("ingest: %v (from %s)", err, s.clientIP(r))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "rejected"})
		return ingest.Agent{}, nil, false
	}
	return agent, parsed.Body, true
}

// handleIngest accepts a measurement batch from a remote agent.
func (s *server) handleIngest(w http.ResponseWriter, r *http.Request) {
	agent, body, ok := s.agentAuth(w, r)
	if !ok {
		return
	}
	measurements, err := ingest.DecodeBatch(body, agent.ID)
	if err != nil {
		log.Printf("ingest: agent %q sent an undecodable batch: %v", agent.Name, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad batch"})
		return
	}

	// An agent may only submit for targets assigned to it. Without this, any
	// enrolled agent could write over any series in the system.
	assigned, err := s.assignedTargets(r.Context(), agent.Name)
	if err != nil {
		internalError(w, err)
		return
	}
	// Drop what this agent may not write, keep the rest.
	//
	// Refusing the whole batch was worse than it looked. An agent keeps a
	// submission buffered until the master confirms it and retries oldest
	// first, so a single measurement for a target that has since been
	// unassigned — an ordinary consequence of editing the tree — put the same
	// rejected batch on the wire forever and the agent never delivered
	// anything again. Skipping the offending rows preserves the property that
	// matters, which is that an agent cannot write a series it was not given,
	// without letting one stale row wedge the outbox.
	kept := measurements[:0]
	var skipped []int64
	for _, m := range measurements {
		if assigned[m.TargetID] {
			kept = append(kept, m)
			continue
		}
		skipped = append(skipped, m.TargetID)
	}
	if len(skipped) > 0 {
		log.Printf("ingest: agent %q submitted %d measurement(s) for targets not assigned to it "+
			"(%v); those were discarded and the rest of the batch accepted",
			agent.Name, len(skipped), uniqueIDs(skipped))
	}
	measurements = kept

	// Writes upsert on (target, agent, ts), so a replayed batch is a
	// byte-identical no-op. That, not the nonce cache, is the replay defense.
	if err := s.st.WriteMeasurements(r.Context(), measurements); err != nil {
		internalError(w, err)
		return
	}
	// Unsigned, and only display metadata about an agent whose identity the
	// signature has already established — see TouchAgent.
	if err := s.agents.TouchAgent(r.Context(), agent.ID, time.Now().Unix(),
		agentVersion(r)); err != nil {
		log.Printf("ingest: recording last_seen for %q: %v", agent.Name, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(measurements)})
}

// handleAgentTargets hands an agent its assignments: resolved settings and
// nothing else. Pure data, pull-only, never code — the narrow distribution
// the design allows (§9), and emphatically not SmokePing's "evaluate what the
// master sends you".
func (s *server) handleAgentTargets(w http.ResponseWriter, r *http.Request) {
	agent, _, ok := s.agentAuth(w, r)
	if !ok {
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
	out := []map[string]any{}
	for i := range targets {
		n := &targets[i]
		if n.Host == nil || !n.Enabled {
			continue
		}
		res, err := tr.Resolve(n.ID)
		if err != nil {
			internalError(w, err)
			return
		}
		if !assignedTo(res.Agents.Effective, agent.Name) {
			continue
		}
		path, _ := tr.Path(n.ID)
		out = append(out, map[string]any{
			"target_id":      n.ID,
			"path":           path,
			"host":           *n.Host,
			"address_family": *n.AddressFamily,
			"interval_s":     res.IntervalS.Effective,
			"pings":          res.PingsPerInterval.Effective,
			"probe_mode":     res.ProbeMode.Effective,
			"burst_gap_ms":   res.BurstGapMS.Effective,
			"timeout_ms":     res.TimeoutMS.Effective,
			"packet_size":    res.PacketSize.Effective,
			"dscp":           res.DSCP.Effective,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

// assignedTargets is the set a given agent may submit for.
func (s *server) assignedTargets(ctx context.Context, agentName string) (map[int64]bool, error) {
	targets, err := s.st.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := tree.New(targets)
	if err != nil {
		return nil, err
	}
	out := map[int64]bool{}
	for i := range targets {
		n := &targets[i]
		if n.Host == nil {
			continue
		}
		res, err := tr.Resolve(n.ID)
		if err != nil {
			return nil, err
		}
		if assignedTo(res.Agents.Effective, agentName) {
			out[n.ID] = true
		}
	}
	return out, nil
}

func assignedTo(list, name string) bool {
	for _, a := range strings.Fields(list) {
		if a == name {
			return true
		}
	}
	return false
}

// handleAgents lists enrolled agents for the UI.
//
// Agents are global infrastructure and grants never confer anything over them
// (DESIGN.md §7.4). A scoped caller still needs their names, or "from ams-01"
// on their own graph is unreadable — so they get the agents that measure
// something they can see, and nothing else: no public keys, and no evidence
// that other agents exist.
func (s *server) handleAgents(w http.ResponseWriter, r *http.Request) {
	sc, targets, ok := s.withScope(w, r)
	if !ok {
		return
	}
	agents, err := s.agents.ListAgents(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	admin := sc.IsGlobalAdmin()
	relevant := map[string]bool{}
	if !admin {
		for i := range targets {
			if targets[i].Host == nil || !sc.Visible(targets[i].ID) {
				continue
			}
			res, err := sc.tr.Resolve(targets[i].ID)
			if err != nil {
				internalError(w, err)
				return
			}
			for _, name := range strings.Fields(res.Agents.Effective) {
				relevant[name] = true
			}
		}
	}
	out := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		if !admin && !relevant[a.Name] {
			continue
		}
		item := map[string]any{
			"id": a.ID, "name": a.Name, "enabled": a.Enabled,
			"is_local": a.ID == store.LocalAgentID,
		}
		if a.LastSeen != 0 {
			item["last_seen"] = a.LastSeen
		}
		// What the agent said it was running when it last reported. A fleet
		// upgrades one host at a time, and this is how an operator sees which
		// agents still predate a fix to the measurement path.
		// The local prober is this process, so its version is known for
		// certain rather than reported — it never submits over the wire, and
		// would otherwise read as unknown on the one agent we cannot be
		// mistaken about.
		if a.ID == store.LocalAgentID {
			item["version"] = s.version
		} else if a.Version != "" {
			item["version"] = a.Version
		}
		// The public key is enrolment material, and belongs to whoever
		// administers agents rather than to whoever reads their graphs.
		if admin && len(a.PubKey) > 0 {
			item["pubkey"] = encodeKey(a.PubKey)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func encodeKey(k ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(k)
}

// agentVersion reads the version an agent claims to be running. Bounded and
// stripped of anything that is not plainly a version, because it is written
// to the database and shown in a UI.
func agentVersion(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("X-Agent-Version"))
	if len(v) > 64 {
		v = v[:64]
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-' || c == '+' || c == '_' || c == '/' || c == ' ' || c == '(' || c == ')') {
			return ""
		}
	}
	return v
}

// uniqueIDs keeps a log line short when a whole batch names the same target.
func uniqueIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
