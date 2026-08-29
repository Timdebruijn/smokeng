package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/timdebruijn/smokeng/internal/store/enc"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Schema version 1 (DESIGN.md §3.1). The root target row and the 'local'
// agent are seeded here so a fresh database is immediately valid: the root
// carries the complete set of inheritable defaults that tree.New requires.
const schemaV1 = `
CREATE TABLE targets (
  id             INTEGER PRIMARY KEY,
  parent_id      INTEGER REFERENCES targets(id),
  name           TEXT NOT NULL,
  host           TEXT,
  address_family TEXT CHECK (address_family IN ('v4','v6')),
  title          TEXT,
  notes          TEXT,
  hidden         INTEGER NOT NULL DEFAULT 0,
  enabled        INTEGER NOT NULL DEFAULT 1,
  sort_order     INTEGER NOT NULL DEFAULT 0,
  interval_s         INTEGER,
  pings_per_interval INTEGER,
  probe_mode         TEXT CHECK (probe_mode IN ('burst','spread')),
  burst_gap_ms       INTEGER,
  timeout_ms         INTEGER,
  packet_size        INTEGER,
  dscp               INTEGER,
  agents             TEXT,
  UNIQUE (parent_id, name)
);

CREATE TABLE measurements (
  target_id INTEGER NOT NULL,
  agent_id  INTEGER NOT NULL DEFAULT 0,
  ts        INTEGER NOT NULL,
  sent      INTEGER NOT NULL,
  received  INTEGER NOT NULL,
  flags     INTEGER NOT NULL DEFAULT 0,
  samples   BLOB NOT NULL,
  PRIMARY KEY (target_id, agent_id, ts)
) WITHOUT ROWID;

CREATE TABLE agents (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL UNIQUE,
  pubkey    BLOB,
  enabled   INTEGER NOT NULL DEFAULT 1,
  last_seen INTEGER
);

CREATE TABLE resolutions (
  target_id INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  address   TEXT NOT NULL,
  PRIMARY KEY (target_id, ts)
);

INSERT INTO targets (id, parent_id, name, interval_s, pings_per_interval,
                     probe_mode, burst_gap_ms, timeout_ms, packet_size, dscp, agents)
VALUES (1, NULL, '', 60, 20, 'burst', 10, 1000, 56, 0, 'local');


INSERT INTO agents (id, name) VALUES (0, 'local');
`

// SQLite implements Store on a single SQLite database file.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// Open opens (creating and migrating if needed) the database at path.
func Open(path string) (*SQLite, error) {
	// modernc.org/sqlite takes a URI DSN; percent-encode the characters that
	// would otherwise break URI parsing of a filesystem path.
	esc := strings.ReplaceAll(strings.ReplaceAll(path, "%", "%25"), " ", "%20")
	dsn := "file:" + esc +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrations are applied in order; index i takes the schema from version i to
// i+1. Existing databases only run what they have not seen.
var migrations = []string{
	schemaV1,
	// v2: record the ICMP error that explains a failed ping, so refusal is
	// distinguishable from silence.
	`ALTER TABLE measurements ADD COLUMN icmp_error INTEGER`,
	// v3: alert rules, attached to tree nodes and inherited by subtree, plus
	// the per-series state that makes hysteresis survive a restart.
	`
CREATE TABLE alert_rules (
  id              INTEGER PRIMARY KEY,
  target_id       INTEGER NOT NULL REFERENCES targets(id),
  name            TEXT NOT NULL,
  metric          TEXT NOT NULL CHECK (metric IN ('loss','median','p95','spread')),
  op              TEXT NOT NULL CHECK (op IN ('>','<')),
  threshold       REAL NOT NULL,
  for_intervals   INTEGER NOT NULL DEFAULT 3,
  clear_intervals INTEGER NOT NULL DEFAULT 3,
  enabled         INTEGER NOT NULL DEFAULT 1,
  UNIQUE (target_id, name)
);

CREATE TABLE alert_state (
  rule_id   INTEGER NOT NULL REFERENCES alert_rules(id),
  target_id INTEGER NOT NULL,
  agent_id  INTEGER NOT NULL,
  firing    INTEGER NOT NULL DEFAULT 0,
  since     INTEGER,
  streak    INTEGER NOT NULL DEFAULT 0,
  last_ts   INTEGER,
  value     REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (rule_id, target_id, agent_id)
) WITHOUT ROWID;
`,
	// v4: server-side secrets that must outlive a restart, starting with the
	// session signing key — a fresh key on every start would log everyone out.
	`CREATE TABLE settings (key TEXT PRIMARY KEY, value BLOB NOT NULL) WITHOUT ROWID`,
	// v5: an agent's outbox marker. A remote agent writes measurements to its
	// own database first and only forgets them once the master has confirmed
	// receipt, so an unreachable master costs latency rather than data. The
	// column is unused on a master, where nothing ever reads it.
	`ALTER TABLE measurements ADD COLUMN submitted INTEGER NOT NULL DEFAULT 0`,
	// v6: path correlation. Paths are stable for days and then are not, so a
	// row is written only when one differs from the last — the same
	// change-only shape the resolutions log uses, and for the same reason.
	`
CREATE TABLE paths (
  target_id INTEGER NOT NULL,
  agent_id  INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  hops      TEXT NOT NULL,
  PRIMARY KEY (target_id, agent_id, ts)
) WITHOUT ROWID;

ALTER TABLE targets ADD COLUMN trace_interval_s INTEGER;
UPDATE targets SET trace_interval_s = 300 WHERE parent_id IS NULL;
`,
	// v7: enrolment tokens (DESIGN.md §9b). Only the hash is stored: a stolen
	// database must not yield a usable credential. The name is fixed when the
	// token is minted, so an agent cannot choose what to call itself.
	`
CREATE TABLE enrolment_tokens (
  id         INTEGER PRIMARY KEY,
  token_hash BLOB NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  agent_id   INTEGER REFERENCES agents(id)
);
`,
	// v8: scoped authorisation (DESIGN.md §7.4). A grant is (OIDC group, node,
	// role) and applies to that node and its whole subtree, the way every other
	// setting on this tree inherits. Keyed on group, never on a person: one
	// person is a group of one in the provider.
	`
CREATE TABLE grants (
  id         INTEGER PRIMARY KEY,
  group_name TEXT NOT NULL,
  target_id  INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('viewer','editor')),
  UNIQUE (group_name, target_id)
);
`,
	// v9: what alerting has done, as opposed to what it is doing now.
	// alert_state holds only the present, so a transition was delivered to the
	// webhook and then forgotten: nobody could answer "when did this last
	// fire" from smokeng itself. The rule's name and description are copied in
	// rather than referenced, so history survives the rule being renamed,
	// re-thresholded or deleted — an entry that changes meaning afterwards is
	// not a record of anything.
	`
CREATE TABLE alert_events (
  id          INTEGER PRIMARY KEY,
  ts          INTEGER NOT NULL,
  rule_id     INTEGER NOT NULL,
  target_id   INTEGER NOT NULL,
  agent_id    INTEGER NOT NULL,
  firing      INTEGER NOT NULL,
  rule_name   TEXT NOT NULL,
  describes   TEXT NOT NULL,
  value       REAL NOT NULL
);
CREATE INDEX alert_events_ts ON alert_events (ts DESC);
`,
	// v10: an agent drains its outbox oldest-first every fifteen seconds, and
	// without this the query was a full scan plus a sort of a table that only
	// grows. Partial, so it costs nothing on a master, where no row is ever
	// pending.
	`CREATE INDEX measurements_pending ON measurements (ts) WHERE submitted = 0`,
	// v11: what each agent is running. A fleet upgrades one host at a time,
	// and the version an agent reports is how you tell which of them still
	// predates a fix to the measurement path — which is not a cosmetic
	// question when the fix was to the timestamps themselves.
	`ALTER TABLE agents ADD COLUMN version TEXT`,
}

func (s *SQLite) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version >= len(migrations) {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for v := version; v < len(migrations); v++ {
		if _, err := tx.Exec(migrations[v]); err != nil {
			return fmt.Errorf("store: apply schema v%d: %w", v+1, err)
		}
	}
	// PRAGMA does not take a bound parameter.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) WriteMeasurements(ctx context.Context, ms []Measurement) error {
	if len(ms) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO measurements (target_id, agent_id, ts, sent, received, flags, samples, icmp_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range ms {
		if m.Received != len(m.Samples) {
			return fmt.Errorf("store: measurement (%d,%d,%d): received=%d but %d samples",
				m.TargetID, m.AgentID, m.TS, m.Received, len(m.Samples))
		}
		blob, err := enc.Encode(m.Samples)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, m.TargetID, m.AgentID, m.TS, m.Sent, m.Received,
			m.Flags, blob, ptrOrNil(m.ICMPErr)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLite) QueryRange(ctx context.Context, targetID, agentID, from, to int64) ([]Measurement, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, sent, received, flags, samples, icmp_error FROM measurements
		WHERE target_id = ? AND agent_id = ? AND ts >= ? AND ts < ?
		ORDER BY ts`, targetID, agentID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Measurement
	for rows.Next() {
		m := Measurement{TargetID: targetID, AgentID: agentID}
		var blob []byte
		var icmpErr sql.NullInt64
		if err := rows.Scan(&m.TS, &m.Sent, &m.Received, &m.Flags, &blob, &icmpErr); err != nil {
			return nil, err
		}
		if icmpErr.Valid {
			v := uint16(icmpErr.Int64)
			m.ICMPErr = &v
		}
		if m.Samples, err = enc.Decode(blob); err != nil {
			return nil, fmt.Errorf("store: measurement (%d,%d,%d): %w", targetID, agentID, m.TS, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const targetCols = `id, parent_id, name, host, address_family, title, notes,
	hidden, enabled, sort_order, interval_s, pings_per_interval, probe_mode,
	burst_gap_ms, timeout_ms, packet_size, dscp, agents, trace_interval_s`

func (s *SQLite) ListTargets(ctx context.Context) ([]tree.Target, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+targetCols+" FROM targets ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tree.Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanTarget(rows *sql.Rows) (tree.Target, error) {
	var t tree.Target
	var parentID sql.NullInt64
	var host, af, title, notes, probeMode, agents sql.NullString
	var intervalS, pings, burstGap, timeout, packetSize, dscp, traceInterval sql.NullInt64
	err := rows.Scan(&t.ID, &parentID, &t.Name, &host, &af, &title, &notes,
		&t.Hidden, &t.Enabled, &t.SortOrder, &intervalS, &pings, &probeMode,
		&burstGap, &timeout, &packetSize, &dscp, &agents, &traceInterval)
	if err != nil {
		return t, err
	}
	if parentID.Valid {
		t.ParentID = &parentID.Int64
	}
	t.Host = nullStr(host)
	t.AddressFamily = nullStr(af)
	t.Title = nullStr(title)
	t.Notes = nullStr(notes)
	t.Settings = tree.Settings{
		IntervalS:        nullInt(intervalS),
		PingsPerInterval: nullInt(pings),
		ProbeMode:        nullStr(probeMode),
		BurstGapMS:       nullInt(burstGap),
		TimeoutMS:        nullInt(timeout),
		PacketSize:       nullInt(packetSize),
		DSCP:             nullInt(dscp),
		Agents:           nullStr(agents),
		TraceIntervalS:   nullInt(traceInterval),
	}
	return t, nil
}

func (s *SQLite) UpsertTarget(ctx context.Context, t *tree.Target) error {
	var id any // NULL lets SQLite assign the next INTEGER PRIMARY KEY
	if t.ID != 0 {
		id = t.ID
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO targets (id, parent_id, name, host, address_family, title, notes,
			hidden, enabled, sort_order, interval_s, pings_per_interval, probe_mode,
			burst_gap_ms, timeout_ms, packet_size, dscp, agents, trace_interval_s)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			parent_id = excluded.parent_id, name = excluded.name,
			host = excluded.host, address_family = excluded.address_family,
			title = excluded.title, notes = excluded.notes,
			hidden = excluded.hidden, enabled = excluded.enabled,
			sort_order = excluded.sort_order,
			interval_s = excluded.interval_s,
			pings_per_interval = excluded.pings_per_interval,
			probe_mode = excluded.probe_mode,
			burst_gap_ms = excluded.burst_gap_ms,
			timeout_ms = excluded.timeout_ms,
			packet_size = excluded.packet_size,
			dscp = excluded.dscp,
			agents = excluded.agents,
			trace_interval_s = excluded.trace_interval_s`,
		id, ptrOrNil(t.ParentID), t.Name, ptrOrNil(t.Host), ptrOrNil(t.AddressFamily),
		ptrOrNil(t.Title), ptrOrNil(t.Notes), t.Hidden, t.Enabled, t.SortOrder,
		ptrOrNil(t.Settings.IntervalS), ptrOrNil(t.Settings.PingsPerInterval),
		ptrOrNil(t.Settings.ProbeMode), ptrOrNil(t.Settings.BurstGapMS),
		ptrOrNil(t.Settings.TimeoutMS), ptrOrNil(t.Settings.PacketSize),
		ptrOrNil(t.Settings.DSCP), ptrOrNil(t.Settings.Agents),
		ptrOrNil(t.Settings.TraceIntervalS))
	if err != nil {
		return err
	}
	if t.ID == 0 {
		newID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		t.ID = newID
	}
	return nil
}

func (s *SQLite) DeleteTarget(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM targets WHERE id = ?", id)
	return err
}

func (s *SQLite) RecordResolution(ctx context.Context, targetID, ts int64, address string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO resolutions (target_id, ts, address) VALUES (?, ?, ?)",
		targetID, ts, address)
	return err
}

func (s *SQLite) LastResolution(ctx context.Context, targetID int64) (string, error) {
	var addr string
	err := s.db.QueryRowContext(ctx,
		"SELECT address FROM resolutions WHERE target_id = ? ORDER BY ts DESC LIMIT 1",
		targetID).Scan(&addr)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return addr, err
}

func nullStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

// ptrOrNil converts a *T into a driver value: the pointee, or SQL NULL.
func ptrOrNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
