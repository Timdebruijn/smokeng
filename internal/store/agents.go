package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"

	"github.com/timdebruijn/smokeng/internal/store/enc"
)

// AgentRecord is an enrolled measurement node. The master stores only the
// public key, so a compromise of this database does not let an attacker
// impersonate an agent.
type AgentRecord struct {
	ID       int64
	Name     string
	PubKey   ed25519.PublicKey // nil for the built-in local agent
	Enabled  bool
	LastSeen int64 // unix seconds, 0 if it has never reported
	// Version is what the agent said it was running, last time it reported.
	// Empty for the local prober, and for an agent that has never been heard
	// from or predates version reporting.
	Version string
}

func (s *SQLite) ListAgents(ctx context.Context) ([]AgentRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, pubkey, enabled, last_seen, version FROM agents ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRecord
	for rows.Next() {
		var a AgentRecord
		var pub []byte
		var lastSeen sql.NullInt64
		var version sql.NullString
		if err := rows.Scan(&a.ID, &a.Name, &pub, &a.Enabled, &lastSeen, &version); err != nil {
			return nil, err
		}
		a.Version = version.String
		if len(pub) > 0 {
			a.PubKey = ed25519.PublicKey(pub)
		}
		a.LastSeen = lastSeen.Int64
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentNames returns every enrolled agent's name keyed by id, including the
// built-in local agent (id 0). It exists for callers, like the alert
// manager, that only need the name and must not import AgentRecord's package
// to get it (internal/alert cannot import internal/store: store already
// imports alert for the rule types it persists).
func (s *SQLite) AgentNames(ctx context.Context) (map[int64]string, error) {
	records, err := s.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(records))
	for _, a := range records {
		out[a.ID] = a.Name
	}
	return out, nil
}

// AddAgent enrols an agent by name and public key directly. This is the
// manual path used by `smokeng agent add`; the token flow
// (RedeemEnrolmentToken, in enrol.go) inserts the agent itself, in the same
// transaction as spending the token, rather than calling this.
func (s *SQLite) AddAgent(ctx context.Context, name string, pub ed25519.PublicKey) (AgentRecord, error) {
	if name == "" || name == LocalAgentName {
		return AgentRecord{}, fmt.Errorf("store: %q is not a usable agent name", name)
	}
	if len(pub) != ed25519.PublicKeySize {
		return AgentRecord{}, fmt.Errorf("store: public key is %d bytes, want %d",
			len(pub), ed25519.PublicKeySize)
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO agents (name, pubkey, enabled) VALUES (?, ?, 1)", name, []byte(pub))
	if err != nil {
		return AgentRecord{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AgentRecord{}, err
	}
	return AgentRecord{ID: id, Name: name, PubKey: pub, Enabled: true}, nil
}

// SetAgentEnabled disables or re-enables an agent. Disabling is the reversible
// alternative to removal: measurements it already submitted stay.
func (s *SQLite) SetAgentEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, "UPDATE agents SET enabled = ? WHERE id = ?", enabled, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: no agent with id %d", id)
	}
	return nil
}

// RemoveAgent deletes an agent. Its measurements are kept, as target deletion
// keeps them: history outlives the thing that produced it.
func (s *SQLite) RemoveAgent(ctx context.Context, id int64) error {
	if id == LocalAgentID {
		return fmt.Errorf("store: the local agent cannot be removed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Measurements are labelled by agent, so removing one that has reported
	// would leave a series nothing can name. This project disables targets
	// rather than deleting them for the same reason: the measurement was true
	// when it was taken and the history is the product.
	var measurements int
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM measurements WHERE agent_id = ?)", id).Scan(&measurements); err != nil {
		return err
	}
	if measurements != 0 {
		return fmt.Errorf("%w: agent %d has reported measurements", ErrAgentHasHistory, id)
	}
	// A spent enrolment token records how an agent came to exist; keep the row
	// but drop the reference, so removing the agent is not blocked by its own
	// paper trail.
	if _, err := tx.ExecContext(ctx,
		"UPDATE enrolment_tokens SET agent_id = NULL WHERE agent_id = ?", id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM agents WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: no agent with id %d", id)
	}
	return tx.Commit()
}

// ErrAgentHasHistory reports an agent that cannot be removed because its
// measurements would be orphaned. Disabling it is the reversible equivalent.
var ErrAgentHasHistory = errors.New("store: agent has measurement history")

// TouchAgent records that an agent was heard from — so an operator can tell a
// silent agent from a busy one — and what it says it is running.
//
// The version arrives in an unsigned header. That is deliberate and worth
// knowing: the request it rides on is signed, so nobody can inject
// measurements this way, but the string itself is not covered by the
// signature. It is display metadata about an agent that has already proven
// who it is — not something to make a trust decision on.
func (s *SQLite) TouchAgent(ctx context.Context, id, at int64, version string) error {
	if version == "" {
		_, err := s.db.ExecContext(ctx, "UPDATE agents SET last_seen = ? WHERE id = ?", at, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE agents SET last_seen = ?, version = ? WHERE id = ?", at, version, id)
	return err
}

// PendingMeasurements returns buffered measurements an agent has not yet had
// confirmed by the master, oldest first so a backlog drains in order.
func (s *SQLite) PendingMeasurements(ctx context.Context, limit int) ([]Measurement, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_id, agent_id, ts, sent, received, flags, samples, icmp_error
		FROM measurements WHERE submitted = 0 ORDER BY ts LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Measurement
	for rows.Next() {
		var m Measurement
		var blob []byte
		var icmpErr sql.NullInt64
		if err := rows.Scan(&m.TargetID, &m.AgentID, &m.TS, &m.Sent, &m.Received,
			&m.Flags, &blob, &icmpErr); err != nil {
			return nil, err
		}
		if m.Samples, err = enc.Decode(blob); err != nil {
			return nil, err
		}
		if icmpErr.Valid {
			v := uint16(icmpErr.Int64)
			m.ICMPErr = &v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DropSubmitted forgets measurements the master has confirmed. It runs only
// after a confirmed response: forgetting them any earlier would lose data
// whenever a push failed halfway.
//
// They are deleted rather than marked. This database is an outbox, not an
// archive — the master holds the history — and keeping every row forever left
// a long-running agent scanning an ever-growing table every fifteen seconds to
// find the handful that were still pending.
func (s *SQLite) DropSubmitted(ctx context.Context, ms []Measurement) error {
	if len(ms) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		"DELETE FROM measurements WHERE target_id = ? AND agent_id = ? AND ts = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range ms {
		if _, err := stmt.ExecContext(ctx, m.TargetID, m.AgentID, m.TS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PathChange is a recorded route to a target at a moment in time.
type PathChange struct {
	TargetID, AgentID, TS int64
	Hops                  string
}

// LastPath returns the most recently recorded path, or "" if none.
func (s *SQLite) LastPath(ctx context.Context, targetID, agentID int64) (string, error) {
	var hops string
	err := s.db.QueryRowContext(ctx,
		"SELECT hops FROM paths WHERE target_id = ? AND agent_id = ? ORDER BY ts DESC LIMIT 1",
		targetID, agentID).Scan(&hops)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hops, err
}

// RecordPath appends a path change. Callers write only when the path differs
// from the last one: a route is stable for days and then is not, so storing
// every run would be the same list thousands of times over.
func (s *SQLite) RecordPath(ctx context.Context, targetID, agentID, ts int64, hops string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO paths (target_id, agent_id, ts, hops) VALUES (?, ?, ?, ?)",
		targetID, agentID, ts, hops)
	return err
}

// PathChanges returns the changes for one series over [from, to), plus the
// one in force when the window opened — without it a window that contains no
// change would look as though the path were unknown.
func (s *SQLite) PathChanges(ctx context.Context, targetID, agentID, from, to int64) ([]PathChange, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, hops FROM paths
		WHERE target_id = ? AND agent_id = ? AND ts < ?
		  AND ts >= COALESCE((SELECT MAX(ts) FROM paths
		                      WHERE target_id = ? AND agent_id = ? AND ts <= ?), 0)
		ORDER BY ts`, targetID, agentID, to, targetID, agentID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PathChange
	for rows.Next() {
		c := PathChange{TargetID: targetID, AgentID: agentID}
		if err := rows.Scan(&c.TS, &c.Hops); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
