package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TokenPrefix marks an enrolment token in a log or a secret scanner. It is not
// a security property; it is so a leaked one is recognisable as what it is.
const TokenPrefix = "smk_"

// ErrTokenInvalid covers every reason a token cannot be redeemed: unknown,
// already spent, or expired. The agent is told one thing, the log gets the
// real reason — an enrolment endpoint that distinguishes "wrong" from "expired"
// tells an attacker which guesses were close.
var ErrTokenInvalid = errors.New("store: enrolment token is not valid")

// ErrNameTaken is separated because it is the operator's own mistake, not an
// attack, and retrying with another name must not cost them the token.
var ErrNameTaken = errors.New("store: an agent with that name already exists")

// EnrolmentToken is a minted token as the API reports it. Plaintext is set
// only by MintEnrolmentToken, and only that once.
type EnrolmentToken struct {
	ID        int64
	Name      string
	CreatedAt int64
	ExpiresAt int64
	UsedAt    int64 // 0 while unspent
	AgentID   int64 // the agent it created, once spent
	Plaintext string
}

func hashToken(tok string) []byte {
	sum := sha256.Sum256([]byte(tok))
	return sum[:]
}

// MintEnrolmentToken issues a single-use token for a name that is not yet
// taken. The plaintext is returned once and never stored.
func (s *SQLite) MintEnrolmentToken(ctx context.Context, name string, ttl time.Duration, now time.Time) (EnrolmentToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "local" {
		return EnrolmentToken{}, fmt.Errorf("store: %q is not a usable agent name", name)
	}
	if ttl <= 0 {
		return EnrolmentToken{}, fmt.Errorf("store: enrolment token lifetime must be positive")
	}
	// Refuse up front rather than at redemption, so the operator finds out
	// while they are still looking at the form.
	var exists int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agents WHERE name = ?", name).Scan(&exists); err != nil {
		return EnrolmentToken{}, err
	}
	if exists > 0 {
		return EnrolmentToken{}, ErrNameTaken
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return EnrolmentToken{}, err
	}
	plain := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	created, expires := now.Unix(), now.Add(ttl).Unix()

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO enrolment_tokens (token_hash, name, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`, hashToken(plain), name, created, expires)
	if err != nil {
		return EnrolmentToken{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return EnrolmentToken{}, err
	}
	return EnrolmentToken{
		ID: id, Name: name, CreatedAt: created, ExpiresAt: expires, Plaintext: plain,
	}, nil
}

// RedeemEnrolmentToken spends a token and creates the agent it names, in one
// transaction. A token that half-enrolled an agent would be worse than no
// token at all.
func (s *SQLite) RedeemEnrolmentToken(ctx context.Context, tok string, pub ed25519.PublicKey, now time.Time) (AgentRecord, error) {
	if len(pub) != ed25519.PublicKeySize {
		return AgentRecord{}, fmt.Errorf("store: public key is %d bytes, want %d",
			len(pub), ed25519.PublicKeySize)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentRecord{}, err
	}
	defer tx.Rollback()

	var id int64
	var name string
	var expires int64
	var usedAt sql.NullInt64
	err = tx.QueryRowContext(ctx,
		"SELECT id, name, expires_at, used_at FROM enrolment_tokens WHERE token_hash = ?",
		hashToken(tok)).Scan(&id, &name, &expires, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentRecord{}, fmt.Errorf("%w: no such token", ErrTokenInvalid)
	}
	if err != nil {
		return AgentRecord{}, err
	}
	if usedAt.Valid {
		return AgentRecord{}, fmt.Errorf("%w: already redeemed at %d", ErrTokenInvalid, usedAt.Int64)
	}
	if now.Unix() > expires {
		return AgentRecord{}, fmt.Errorf("%w: expired at %d", ErrTokenInvalid, expires)
	}

	// The name was claimed when the token was minted, but an agent may have
	// been added under it in the meantime. Reject without spending the token.
	var taken int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agents WHERE name = ?", name).Scan(&taken); err != nil {
		return AgentRecord{}, err
	}
	if taken > 0 {
		return AgentRecord{}, ErrNameTaken
	}

	res, err := tx.ExecContext(ctx,
		"INSERT INTO agents (name, pubkey, enabled) VALUES (?, ?, 1)", name, []byte(pub))
	if err != nil {
		return AgentRecord{}, err
	}
	agentID, err := res.LastInsertId()
	if err != nil {
		return AgentRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE enrolment_tokens SET used_at = ?, agent_id = ? WHERE id = ?",
		now.Unix(), agentID, id); err != nil {
		return AgentRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentRecord{}, err
	}
	return AgentRecord{ID: agentID, Name: name, PubKey: pub, Enabled: true}, nil
}

// ListEnrolmentTokens reports tokens for the UI. Plaintext is never among
// them: it existed once, in the response that minted it.
func (s *SQLite) ListEnrolmentTokens(ctx context.Context) ([]EnrolmentToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_at, expires_at, used_at, agent_id
		 FROM enrolment_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnrolmentToken
	for rows.Next() {
		var t EnrolmentToken
		var usedAt, agentID sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.ExpiresAt, &usedAt, &agentID); err != nil {
			return nil, err
		}
		t.UsedAt, t.AgentID = usedAt.Int64, agentID.Int64
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeEnrolmentToken deletes an unspent token. A spent one is kept: it is
// the record of how an agent came to exist.
func (s *SQLite) RevokeEnrolmentToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM enrolment_tokens WHERE id = ? AND used_at IS NULL", id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("store: no unspent enrolment token with id %d", id)
	}
	return nil
}

// RenameAgent changes an agent's name. Targets refer to agents by name, so the
// caller is responsible for rewriting any `agents` list that names the old one
// — which is why the API does that in the same request.
func (s *SQLite) RenameAgent(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "local" {
		return fmt.Errorf("store: %q is not a usable agent name", name)
	}
	var taken int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agents WHERE name = ? AND id <> ?", name, id).Scan(&taken); err != nil {
		return err
	}
	if taken > 0 {
		return ErrNameTaken
	}
	res, err := s.db.ExecContext(ctx, "UPDATE agents SET name = ? WHERE id = ?", name, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("store: no agent with id %d", id)
	}
	return nil
}
