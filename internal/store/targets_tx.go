package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/timdebruijn/smokeng/internal/tree"
)

// queryer is what listTargets needs: anything that can query, and so also the
// *sql.Tx a write reads the tree through before it changes it.
type queryer interface {
	idRunner
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// TargetChange is what one write to the tree does, applied together or not at
// all.
type TargetChange struct {
	// Upsert creates a target (ID 0, which allocates) or updates one.
	Upsert []*tree.Target
	// Delete removes targets, deepest first: a parent cannot go while a child
	// still names it.
	Delete []int64
}

// ErrAbort is what a ChangeTargets callback returns to say it has nothing to
// write and has already told the caller why. It is never returned by
// ChangeTargets itself.
var ErrAbort = errors.New("store: change abandoned")

// ChangeTargets reads the tree, lets fn decide what to write against exactly
// what it read, and writes it, in one transaction that holds the database's
// write lock from the read to the commit.
//
// Reading, validating and writing as separate steps raced: two requests each
// validated their own change against the same snapshot, both passed, and both
// wrote. Moving A under B and B under A are each fine on their own and a cycle
// together, and a cycle makes tree.New fail, which every request and the prober
// call, so the whole installation answered 500 until someone edited the
// database by hand. Here the second request cannot read the tree until the first
// has committed, so it validates against the result.
//
// fn returns whatever it wants to change, or an error to abandon the write; the
// error is returned unchanged. The same goes for a failure anywhere in the
// writes: nothing is applied.
func (s *SQLite) ChangeTargets(ctx context.Context, fn func(current []tree.Target) (TargetChange, error)) error {
	tx, err := s.db.BeginTx(ctx, nil) // the DSN makes this IMMEDIATE
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := listTargets(ctx, tx)
	if err != nil {
		return err
	}
	ch, err := fn(current)
	if err != nil {
		return err
	}
	for _, t := range ch.Upsert {
		if err := upsertTarget(ctx, tx, t); err != nil {
			return err
		}
	}
	for _, id := range ch.Delete {
		if err := deleteTarget(ctx, tx, id); err != nil {
			return fmt.Errorf("store: delete target %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// DeleteTarget removes one target and what is defined on it.
func (s *SQLite) DeleteTarget(ctx context.Context, id int64) error {
	return s.ChangeTargets(ctx, func([]tree.Target) (TargetChange, error) {
		return TargetChange{Delete: []int64{id}}, nil
	})
}

// deleteTarget removes a target together with the alert rules defined on it and
// their state. alert_rules has a foreign key to targets with no cascade, so a
// target with a rule used to fail to delete, and a recursive delete failed
// part-way: the children already gone, the node itself refused, and the
// handler answering 500. Rules are defined on a node and go with it; the
// history they produced (events, measurements) stays, as it does for the target.
func deleteTarget(ctx context.Context, q idRunner, id int64) error {
	for _, stmt := range []string{
		"DELETE FROM alert_state WHERE rule_id IN (SELECT id FROM alert_rules WHERE target_id = ?)",
		"DELETE FROM alert_rules WHERE target_id = ?",
		"DELETE FROM targets WHERE id = ?",
	} {
		if _, err := q.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	return nil
}
