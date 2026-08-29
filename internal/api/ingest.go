package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log"
	"net/http"
	"strings"
	"time"

	"smokeng/internal/ingest"
	"smokeng/internal/store"
	"smokeng/internal/tree"
)

// maxIngestBody caps a submission. A batch is a few tens of bytes per
// measurement, so this is orders of magnitude above any honest agent.
const maxIngestBody = 8 << 20

// AgentStore is the persistence the agent endpoints need.
type AgentStore interface {
	ListAgents(ctx context.Context) ([]store.AgentRecord, error)
	TouchAgent(ctx context.Context, id, at int64) error
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
		log.Printf("ingest: rejected a request from %s: %v", r.RemoteAddr, err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "rejected"})
		return ingest.Agent{}, nil, false
	}
	agent, err := s.verifier.Check(parsed, time.Now())
	if err != nil {
		// The reason goes to the log, with the agent id, exactly as designed.
		log.Printf("ingest: %v (from %s)", err, r.RemoteAddr)
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
	for _, m := range measurements {
		if !assigned[m.TargetID] {
			log.Printf("ingest: agent %q submitted target %d, which is not assigned to it",
				agent.Name, m.TargetID)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "batch contains targets not assigned to this agent",
			})
			return
		}
	}

	// Writes upsert on (target, agent, ts), so a replayed batch is a
	// byte-identical no-op. That, not the nonce cache, is the replay defense.
	if err := s.st.WriteMeasurements(r.Context(), measurements); err != nil {
		internalError(w, err)
		return
	}
	if err := s.agents.TouchAgent(r.Context(), agent.ID, time.Now().Unix()); err != nil {
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
func (s *server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.agents.ListAgents(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		item := map[string]any{
			"id": a.ID, "name": a.Name, "enabled": a.Enabled,
			"is_local": a.ID == store.LocalAgentID,
		}
		if a.LastSeen != 0 {
			item["last_seen"] = a.LastSeen
		}
		if len(a.PubKey) > 0 {
			item["pubkey"] = encodeKey(a.PubKey)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func encodeKey(k ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(k)
}
