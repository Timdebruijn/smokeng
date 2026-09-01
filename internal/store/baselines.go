package store

import (
	"context"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store/enc"
)

// ListAlertBaselines returns every captured reference distribution, keyed for
// the manager by rule id.
func (s *SQLite) ListAlertBaselines(ctx context.Context) ([]alert.Baselined, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rule_id, target_id, agent_id, from_ts, to_ts, intervals, samples, captured_at, captured_by
		FROM alert_baselines`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alert.Baselined
	for rows.Next() {
		var b alert.Baselined
		var blob []byte
		if err := rows.Scan(&b.RuleID, &b.TargetID, &b.AgentID, &b.FromTS, &b.ToTS,
			&b.Intervals, &blob, &b.CapturedAt, &b.CapturedBy); err != nil {
			return nil, err
		}
		if b.Samples, err = enc.Decode(blob); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SaveAlertBaseline stores (or replaces) a rule's captured reference.
func (s *SQLite) SaveAlertBaseline(ctx context.Context, b *alert.Baselined) error {
	blob, err := enc.Encode(b.Samples)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO alert_baselines
			(rule_id, target_id, agent_id, from_ts, to_ts, intervals, samples, captured_at, captured_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(rule_id) DO UPDATE SET
			target_id = excluded.target_id, agent_id = excluded.agent_id,
			from_ts = excluded.from_ts, to_ts = excluded.to_ts,
			intervals = excluded.intervals, samples = excluded.samples,
			captured_at = excluded.captured_at, captured_by = excluded.captured_by`,
		b.RuleID, b.TargetID, b.AgentID, b.FromTS, b.ToTS, b.Intervals, blob,
		b.CapturedAt, b.CapturedBy)
	return err
}

// DeleteAlertBaseline removes a rule's reference, reporting whether one existed.
func (s *SQLite) DeleteAlertBaseline(ctx context.Context, ruleID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM alert_baselines WHERE rule_id = ?", ruleID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
