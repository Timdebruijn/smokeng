package store

import (
	"context"

	"github.com/timdebruijn/smokeng/internal/alert"
)

func (s *SQLite) ListSilences(ctx context.Context) ([]alert.Silence, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, target_id, agent_id, rule_id, starts_at, ends_at, reason, created_by, created_at
		FROM silences ORDER BY starts_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alert.Silence
	for rows.Next() {
		var sil alert.Silence
		if err := rows.Scan(&sil.ID, &sil.TargetID, &sil.AgentID, &sil.RuleID,
			&sil.StartsAt, &sil.EndsAt, &sil.Reason, &sil.CreatedBy, &sil.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sil)
	}
	return out, rows.Err()
}

// CreateSilence inserts a silence and assigns its ID.
func (s *SQLite) CreateSilence(ctx context.Context, sil *alert.Silence) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO silences (target_id, agent_id, rule_id, starts_at, ends_at, reason, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sil.TargetID, sil.AgentID, sil.RuleID,
		sil.StartsAt, sil.EndsAt, sil.Reason, sil.CreatedBy, sil.CreatedAt)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	sil.ID = id
	return nil
}

// DeleteSilence removes a silence, reporting whether one existed to remove so
// the caller can answer 404 rather than pretend it deleted something.
func (s *SQLite) DeleteSilence(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM silences WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
