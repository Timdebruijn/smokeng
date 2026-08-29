package store

import (
	"context"
	"fmt"

	"github.com/timdebruijn/smokeng/internal/alert"
)

// RecordAlertEvents appends transitions. It is called with whatever the
// manager just delivered, so the log and the webhook cannot disagree.
func (s *SQLite) RecordAlertEvents(ctx context.Context, events []alert.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO alert_events (ts, rule_id, target_id, agent_id, firing, rule_name, describes, value)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		if _, err := stmt.ExecContext(ctx, e.TS, e.RuleID, e.TargetID, e.AgentID,
			e.Firing, e.RuleName, e.Describes, e.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListAlertEvents returns the most recent transitions, newest first.
func (s *SQLite) ListAlertEvents(ctx context.Context, limit int) ([]alert.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, rule_id, target_id, agent_id, firing, rule_name, describes, value
		FROM alert_events ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alert.Event
	for rows.Next() {
		var e alert.Event
		if err := rows.Scan(&e.ID, &e.TS, &e.RuleID, &e.TargetID, &e.AgentID,
			&e.Firing, &e.RuleName, &e.Describes, &e.Value); err != nil {
			return nil, fmt.Errorf("store: read alert event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
