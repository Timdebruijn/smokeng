package store

import (
	"context"
	"fmt"
	"strings"
)

// Grant gives an OIDC group a role on a target node and everything beneath it
// (DESIGN.md §7.4).
type Grant struct {
	ID       int64
	Group    string
	TargetID int64
	Role     string // "viewer" or "editor"
}

func (g Grant) validate() error {
	if strings.TrimSpace(g.Group) == "" {
		return fmt.Errorf("store: a grant needs a group name")
	}
	if g.Role != "viewer" && g.Role != "editor" {
		return fmt.Errorf("store: grant role must be viewer or editor, got %q", g.Role)
	}
	if g.TargetID <= 0 {
		return fmt.Errorf("store: a grant needs a target")
	}
	return nil
}

func (s *SQLite) ListGrants(ctx context.Context) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, group_name, target_id, role FROM grants ORDER BY group_name, target_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.Group, &g.TargetID, &g.Role); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UpsertGrant creates or re-roles a grant. A group holding two roles on the
// same node is not a state worth representing, so the pair is unique and the
// role is simply replaced.
func (s *SQLite) UpsertGrant(ctx context.Context, g *Grant) error {
	g.Group = strings.TrimSpace(g.Group)
	if err := g.validate(); err != nil {
		return err
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO grants (group_name, target_id, role) VALUES (?, ?, ?)
		ON CONFLICT (group_name, target_id) DO UPDATE SET role = excluded.role
		RETURNING id`, g.Group, g.TargetID, g.Role).Scan(&g.ID)
	return err
}

func (s *SQLite) DeleteGrant(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM grants WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: no grant with id %d", id)
	}
	return nil
}
