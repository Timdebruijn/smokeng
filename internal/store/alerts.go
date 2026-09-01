package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"

	"github.com/timdebruijn/smokeng/internal/alert"
)

// SessionKey returns the persisted session signing key, generating one on
// first use. Persisting it is what lets sessions survive a restart; deleting
// the row is the way to invalidate every session at once.
func (s *SQLite) SessionKey(ctx context.Context) ([]byte, error) {
	const key = "session_key"
	var value []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == nil && len(value) >= 32 {
		return value, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	value = make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return nil, fmt.Errorf("store: generate session key: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", key, value); err != nil {
		return nil, err
	}
	return value, nil
}

// ListAlertRules returns every rule, in tree order by the node that defines
// it. Resolving which rules apply to a target is the tree's job, not the
// store's.
func (s *SQLite) ListAlertRules(ctx context.Context) ([]alert.Rule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, target_id, name, metric, op, threshold, for_intervals, clear_intervals, enabled, mode, baseline
		FROM alert_rules ORDER BY target_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alert.Rule
	for rows.Next() {
		var r alert.Rule
		var mode, baseline string
		if err := rows.Scan(&r.ID, &r.TargetID, &r.Name, &r.Metric, &r.Op, &r.Threshold,
			&r.For, &r.ClearFor, &r.Enabled, &mode, &baseline); err != nil {
			return nil, err
		}
		r.Mode, r.Baseline = alert.Mode(mode), alert.Baseline(baseline)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertAlertRule inserts (ID == 0, assigning r.ID) or updates a rule.
func (s *SQLite) UpsertAlertRule(ctx context.Context, r *alert.Rule) error {
	var id any
	if r.ID != 0 {
		id = r.ID
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO alert_rules (id, target_id, name, metric, op, threshold,
			for_intervals, clear_intervals, enabled, mode, baseline)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			target_id = excluded.target_id, name = excluded.name,
			metric = excluded.metric, op = excluded.op, threshold = excluded.threshold,
			for_intervals = excluded.for_intervals,
			clear_intervals = excluded.clear_intervals, enabled = excluded.enabled,
			mode = excluded.mode, baseline = excluded.baseline`,
		id, r.TargetID, r.Name, string(r.Metric), string(r.Op), r.Threshold,
		r.For, r.ClearFor, r.Enabled, string(r.Mode), string(r.Baseline))
	if err != nil {
		return err
	}
	if r.ID == 0 {
		newID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		r.ID = newID
	}
	return nil
}

// DeleteAlertRule removes a rule and any state it accumulated.
func (s *SQLite) DeleteAlertRule(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM alert_state WHERE rule_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM alert_rules WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAlertStates returns the standing of every rule that has been evaluated.
func (s *SQLite) ListAlertStates(ctx context.Context) ([]alert.State, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rule_id, target_id, agent_id, firing, since, streak, last_ts, value,
			acked_since, acked_at, acked_by
		FROM alert_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alert.State
	for rows.Next() {
		var st alert.State
		var since, lastTS, ackedSince, ackedAt sql.NullInt64
		var ackedBy sql.NullString
		if err := rows.Scan(&st.RuleID, &st.TargetID, &st.AgentID, &st.Firing,
			&since, &st.Streak, &lastTS, &st.Value,
			&ackedSince, &ackedAt, &ackedBy); err != nil {
			return nil, err
		}
		st.Since, st.LastTS = since.Int64, lastTS.Int64
		st.AckedSince, st.AckedAt, st.AckedBy = ackedSince.Int64, ackedAt.Int64, ackedBy.String
		out = append(out, st)
	}
	return out, rows.Err()
}

// SaveAlertStates persists evaluation state. Hysteresis is only meaningful if
// it outlives a restart: an alert that has been firing for an hour must not
// resolve and re-fire because smokeng was restarted.
func (s *SQLite) SaveAlertStates(ctx context.Context, states []alert.State) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO alert_state
			(rule_id, target_id, agent_id, firing, since, streak, last_ts, value,
			 acked_since, acked_at, acked_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, st := range states {
		var since, lastTS, ackedSince, ackedAt any
		var ackedBy any
		if st.Since != 0 {
			since = st.Since
		}
		if st.LastTS != 0 {
			lastTS = st.LastTS
		}
		if st.AckedSince != 0 {
			ackedSince, ackedAt, ackedBy = st.AckedSince, st.AckedAt, st.AckedBy
		}
		if _, err := stmt.ExecContext(ctx, st.RuleID, st.TargetID, st.AgentID,
			st.Firing, since, st.Streak, lastTS, st.Value,
			ackedSince, ackedAt, ackedBy); err != nil {
			return err
		}
	}
	return tx.Commit()
}
