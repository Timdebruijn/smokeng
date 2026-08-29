package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
)

// EnrolStore is the slice of the store the enrolment endpoints need.
type EnrolStore interface {
	MintEnrolmentToken(ctx context.Context, name string, ttl time.Duration, now time.Time) (store.EnrolmentToken, error)
	RedeemEnrolmentToken(ctx context.Context, tok string, pub ed25519.PublicKey, now time.Time) (store.AgentRecord, error)
	ListEnrolmentTokens(ctx context.Context) ([]store.EnrolmentToken, error)
	RevokeEnrolmentToken(ctx context.Context, id int64) error
	RenameAgent(ctx context.Context, id int64, name string) (int, error)
	SetAgentEnabled(ctx context.Context, id int64, enabled bool) error
	RemoveAgent(ctx context.Context, id int64) error
}

// defaultTokenTTL is short on purpose: an enrolment token is meant to be used
// in the minutes after it is minted, not to live in a wiki.
const defaultTokenTTL = time.Hour

// maxTokenTTL stops a caller from asking for one that outlives its usefulness.
const maxTokenTTL = 24 * time.Hour

func (s *server) handleListEnrolTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.enrol.ListEnrolmentTokens(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	now := time.Now().Unix()
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		item := map[string]any{
			"id": t.ID, "name": t.Name,
			"created_at": t.CreatedAt, "expires_at": t.ExpiresAt,
			// Spent and expired are different states and the UI shows them
			// differently: one is an agent that exists, the other is litter.
			"used":    t.UsedAt != 0,
			"expired": t.UsedAt == 0 && now > t.ExpiresAt,
		}
		if t.UsedAt != 0 {
			item["used_at"] = t.UsedAt
			item["agent_id"] = t.AgentID
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *server) handleMintEnrolToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		TTLS int    `json:"ttl_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		badRequestMsg(w, "malformed request body")
		return
	}
	ttl := defaultTokenTTL
	if body.TTLS > 0 {
		ttl = time.Duration(body.TTLS) * time.Second
	}
	if ttl > maxTokenTTL {
		badRequestMsg(w, "an enrolment token may not live longer than 24 hours")
		return
	}
	tok, err := s.enrol.MintEnrolmentToken(r.Context(), body.Name, ttl, time.Now())
	if errors.Is(err, store.ErrNameTaken) {
		badRequestMsg(w, "an agent named "+strconv.Quote(body.Name)+" already exists")
		return
	}
	if err != nil {
		badRequest(w, err)
		return
	}
	// The only time the plaintext is ever returned. It is not stored, so this
	// response cannot be reproduced.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": tok.ID, "name": tok.Name, "token": tok.Plaintext,
		"created_at": tok.CreatedAt, "expires_at": tok.ExpiresAt,
	})
}

func (s *server) handleRevokeEnrolToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequestMsg(w, "id must be a number")
		return
	}
	if err := s.enrol.RevokeEnrolmentToken(r.Context(), id); err != nil {
		badRequest(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEnrol is the one endpoint an unenrolled agent may call. The token is
// the authentication, which is exactly why the agent refuses a plain-HTTP
// master without being told to: this is the request that carries a usable
// credential (DESIGN.md §9b).
func (s *server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token  string `json:"token"`
		PubKey string `json:"pubkey"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		badRequestMsg(w, "malformed request body")
		return
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body.PubKey))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		badRequestMsg(w, "pubkey must be a base64-encoded Ed25519 public key")
		return
	}
	agent, err := s.enrol.RedeemEnrolmentToken(r.Context(), body.Token, pub, time.Now())
	switch {
	case errors.Is(err, store.ErrTokenInvalid):
		// One answer for every reason a token does not work. Distinguishing
		// "unknown" from "expired" tells a guesser which attempts were close.
		log.Printf("enrol: refused a token from %s: %v", s.clientIP(r), err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "rejected"})
		return
	case errors.Is(err, store.ErrNameTaken):
		// The operator's own mistake, and the token is not spent, so say so
		// plainly instead of hiding it behind the generic rejection.
		badRequestMsg(w, "an agent with that name already exists; the token is unspent")
		return
	case err != nil:
		internalError(w, err)
		return
	}
	log.Printf("enrol: agent %q enrolled from %s with id %d", agent.Name, s.clientIP(r), agent.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"agent_id": agent.ID, "name": agent.Name})
}

func (s *server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequestMsg(w, "id must be a number")
		return
	}
	if id == store.LocalAgentID {
		badRequestMsg(w, "the built-in local agent cannot be renamed or disabled")
		return
	}
	var body struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		badRequestMsg(w, "malformed request body")
		return
	}
	if body.Name != nil {
		if _, err := s.enrol.RenameAgent(r.Context(), id, *body.Name); err != nil {
			badRequest(w, err)
			return
		}
	}
	if body.Enabled != nil {
		if err := s.enrol.SetAgentEnabled(r.Context(), id, *body.Enabled); err != nil {
			badRequest(w, err)
			return
		}
	}
	s.handleAgents(w, r)
}

func (s *server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequestMsg(w, "id must be a number")
		return
	}
	if id == store.LocalAgentID {
		badRequestMsg(w, "the built-in local agent cannot be removed")
		return
	}
	if err := s.enrol.RemoveAgent(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrAgentHasHistory) {
			badRequestMsg(w, "this agent has reported measurements; removing it would leave "+
				"a series nothing can name. Disable it instead — probing stops, the history stays.")
			return
		}
		badRequest(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
