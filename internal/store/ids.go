package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Target, agent and alert-rule ids are never reused.
//
// Both tables used a bare INTEGER PRIMARY KEY, which SQLite hands out as one
// more than the largest id present. Delete the newest target and the next one
// created gets its id back, and everything keyed by target id that was
// deliberately kept on delete — measurements, traceroute hops, alert state,
// baselines — is now that new target's history, to whoever has a grant on it.
// Agents are the same: a removed agent's measurements, paths and alert state
// stay, and the next agent enrolled inherits them. And so are alert rules: a
// silence names a rule by id and has no foreign key to it, so a silence made for
// a rule that was then deleted muted whichever rule was created next, which may
// belong to someone else.
//
// Rebuilding either table to add AUTOINCREMENT would mean copying it while
// other tables hold foreign keys into it, inside the migration's transaction,
// where PRAGMA foreign_keys cannot be switched off. So the counter lives in a
// table of its own: allocating an id inserts a row into an AUTOINCREMENT table
// and reads back the id SQLite chose. SQLite keeps the high-water mark in
// sqlite_sequence whether or not the row is later deleted, which is the one
// property wanted, and an insert is a write, so concurrent allocations
// serialise on the database's own lock.

// idRunner is the part of *sql.DB and *sql.Tx that allocation needs, so an id
// can be allocated inside the transaction that uses it.
type idRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func allocTargetID(ctx context.Context, q idRunner) (int64, error) {
	return alloc(ctx, q,
		"INSERT INTO target_ids DEFAULT VALUES",
		"DELETE FROM target_ids WHERE id = ?",
		"SELECT 1 FROM targets WHERE id = ?")
}

func allocAgentID(ctx context.Context, q idRunner) (int64, error) {
	return alloc(ctx, q,
		"INSERT INTO agent_ids DEFAULT VALUES",
		"DELETE FROM agent_ids WHERE id = ?",
		"SELECT 1 FROM agents WHERE id = ?")
}

func allocRuleID(ctx context.Context, q idRunner) (int64, error) {
	return alloc(ctx, q,
		"INSERT INTO rule_ids DEFAULT VALUES",
		"DELETE FROM rule_ids WHERE id = ?",
		"SELECT 1 FROM alert_rules WHERE id = ?")
}

// alloc takes the next id and checks no row already holds it. The check is not
// there for the counter's sake, which only moves forward, but for a row
// inserted with an explicit id above it: the upsert that follows is ON CONFLICT
// DO UPDATE, and an id that is already taken would silently overwrite that row
// instead of creating a new one. Skipping forward is cheap and cannot loop long.
func alloc(ctx context.Context, q idRunner, insert, del, taken string) (int64, error) {
	for range 16 {
		res, err := q.ExecContext(ctx, insert)
		if err != nil {
			return 0, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		// The counter is kept by sqlite_sequence, not by this row; leaving
		// every allocated id in the table would only make it grow.
		if _, err := q.ExecContext(ctx, del, id); err != nil {
			return 0, err
		}
		var one int
		switch err := q.QueryRowContext(ctx, taken, id).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return id, nil
		case err != nil:
			return 0, err
		}
	}
	return 0, errors.New("store: could not find an unused id")
}

// seedIDsMigration creates the counters and starts them above every id that
// has ever been used, and that includes ids that no longer have a row in
// targets, agents or alert_rules: history, paths and alert state were kept when those were
// deleted, so a counter that began at the highest *live* id would hand a
// deleted one back out the first time it was asked.
//
// measurements is a WITHOUT ROWID table, so its primary key is the table and a
// scan reads every blob. MAX(target_id) is a seek, because target_id leads the
// key, but MAX(agent_id) is not, so agent ids are found by stepping through
// the distinct target ids (each step a seek) and taking the largest agent id
// under each. That is a few seeks per target rather than a pass over the
// history, which matters on the installation this is built for, where the
// history is the whole point and does not stop growing.
func seedIDsMigration() string {
	skip := func(cte, table string) string {
		return fmt.Sprintf(`%[1]s(id) AS (
  SELECT MIN(target_id) FROM %[2]s
  UNION ALL
  SELECT (SELECT MIN(target_id) FROM %[2]s WHERE target_id > %[1]s.id)
    FROM %[1]s WHERE id IS NOT NULL
)`, cte, table)
	}
	agentMax := func(cte, table string) string {
		return fmt.Sprintf(`SELECT MAX((SELECT MAX(agent_id) FROM %[2]s WHERE target_id = %[1]s.id))
    FROM %[1]s WHERE id IS NOT NULL`, cte, table)
	}
	return `
CREATE TABLE target_ids (id INTEGER PRIMARY KEY AUTOINCREMENT);
INSERT INTO target_ids (id)
SELECT COALESCE(MAX(m), 1) FROM (
  SELECT MAX(id)        AS m FROM targets
  UNION ALL SELECT MAX(target_id) FROM measurements
  UNION ALL SELECT MAX(target_id) FROM measurement_series
  UNION ALL SELECT MAX(target_id) FROM resolutions
  UNION ALL SELECT MAX(target_id) FROM paths
  UNION ALL SELECT MAX(target_id) FROM alert_state
  UNION ALL SELECT MAX(target_id) FROM alert_events
  UNION ALL SELECT MAX(target_id) FROM alert_baselines
  UNION ALL SELECT MAX(target_id) FROM alert_rules
  UNION ALL SELECT MAX(target_id) FROM silences
  UNION ALL SELECT MAX(target_id) FROM grants
);
DELETE FROM target_ids;

CREATE TABLE agent_ids (id INTEGER PRIMARY KEY AUTOINCREMENT);
WITH RECURSIVE
  ` + skip("tm", "measurements") + `,
  ` + skip("ts", "measurement_series") + `,
  ` + skip("tp", "paths") + `
INSERT INTO agent_ids (id)
SELECT COALESCE(MAX(m), 0) FROM (
  SELECT MAX(id)        AS m FROM agents
  UNION ALL ` + agentMax("tm", "measurements") + `
  UNION ALL ` + agentMax("ts", "measurement_series") + `
  UNION ALL ` + agentMax("tp", "paths") + `
  UNION ALL SELECT MAX(agent_id) FROM alert_state
  UNION ALL SELECT MAX(agent_id) FROM alert_events
  UNION ALL SELECT MAX(agent_id) FROM alert_baselines
  UNION ALL SELECT MAX(agent_id) FROM silences
  UNION ALL SELECT MAX(agent_id) FROM enrolment_tokens
);
DELETE FROM agent_ids;

CREATE TABLE rule_ids (id INTEGER PRIMARY KEY AUTOINCREMENT);
INSERT INTO rule_ids (id)
SELECT COALESCE(MAX(m), 0) FROM (
  SELECT MAX(id)        AS m FROM alert_rules
  UNION ALL SELECT MAX(rule_id) FROM alert_state
  UNION ALL SELECT MAX(rule_id) FROM alert_events
  UNION ALL SELECT MAX(rule_id) FROM alert_baselines
  UNION ALL SELECT MAX(rule_id) FROM silences
);
DELETE FROM rule_ids;
`
}
