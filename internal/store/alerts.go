package store

import (
	"context"
	"database/sql"

	"smokeng/internal/alert"
)

// ListAlertRules returns every rule, in tree order by the node that defines
// it. Resolving which rules apply to a target is the tree's job, not the
// store's.
func (s *SQLite) ListAlertRules(ctx context.Context) ([]alert.Rule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, target_id, name, metric, op, threshold, for_intervals, clear_intervals, enabled
		FROM alert_rules ORDER BY target_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alert.Rule
	for rows.Next() {
		var r alert.Rule
		if err := rows.Scan(&r.ID, &r.TargetID, &r.Name, &r.Metric, &r.Op, &r.Threshold,
			&r.For, &r.ClearFor, &r.Enabled); err != nil {
			return nil, err
		}
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
			for_intervals, clear_intervals, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			target_id = excluded.target_id, name = excluded.name,
			metric = excluded.metric, op = excluded.op, threshold = excluded.threshold,
			for_intervals = excluded.for_intervals,
			clear_intervals = excluded.clear_intervals, enabled = excluded.enabled`,
		id, r.TargetID, r.Name, string(r.Metric), string(r.Op), r.Threshold,
		r.For, r.ClearFor, r.Enabled)
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
		SELECT rule_id, target_id, agent_id, firing, since, streak, last_ts, value
		FROM alert_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alert.State
	for rows.Next() {
		var st alert.State
		var since, lastTS sql.NullInt64
		if err := rows.Scan(&st.RuleID, &st.TargetID, &st.AgentID, &st.Firing,
			&since, &st.Streak, &lastTS, &st.Value); err != nil {
			return nil, err
		}
		st.Since, st.LastTS = since.Int64, lastTS.Int64
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
			(rule_id, target_id, agent_id, firing, since, streak, last_ts, value)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, st := range states {
		var since, lastTS any
		if st.Since != 0 {
			since = st.Since
		}
		if st.LastTS != 0 {
			lastTS = st.LastTS
		}
		if _, err := stmt.ExecContext(ctx, st.RuleID, st.TargetID, st.AgentID,
			st.Firing, since, st.Streak, lastTS, st.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}
