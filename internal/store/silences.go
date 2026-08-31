package store

import (
	"context"
	"database/sql"

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
		var target, agent, rule sql.NullInt64
		if err := rows.Scan(&sil.ID, &target, &agent, &rule,
			&sil.StartsAt, &sil.EndsAt, &sil.Reason, &sil.CreatedBy, &sil.CreatedAt); err != nil {
			return nil, err
		}
		sil.TargetID, sil.AgentID, sil.RuleID = nullInt64(target), nullInt64(agent), nullInt64(rule)
		out = append(out, sil)
	}
	return out, rows.Err()
}

// CreateSilence inserts a silence and assigns its ID.
func (s *SQLite) CreateSilence(ctx context.Context, sil *alert.Silence) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO silences (target_id, agent_id, rule_id, starts_at, ends_at, reason, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ptrOrNil(sil.TargetID), ptrOrNil(sil.AgentID), ptrOrNil(sil.RuleID),
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

func nullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
