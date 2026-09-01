package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/timdebruijn/smokeng/internal/report"
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
	// v12: what a target is measured with, as opposed to when (DESIGN.md
	// §3.2b). Inheritable like every other setting, so the columns are NULL
	// where a node inherits. Per-type settings live beside it as columns
	// rather than in a blob, because a value the UI shows has to be able to
	// say which node it came from.
	`
ALTER TABLE targets ADD COLUMN probe_type TEXT;
ALTER TABLE targets ADD COLUMN probe_port INTEGER;
ALTER TABLE targets ADD COLUMN dns_query TEXT;
ALTER TABLE targets ADD COLUMN dns_rr_type TEXT;
ALTER TABLE targets ADD COLUMN http_path TEXT;
UPDATE targets SET probe_type = 'icmp' WHERE parent_id IS NULL AND probe_type IS NULL;
`,
	// v13: whether an https probe verifies the far end's certificate.
	// Inheritable, and false at the root, so turning it off is a decision
	// written on one node rather than a property of the installation.
	`
ALTER TABLE targets ADD COLUMN tls_skip_verify INTEGER;
UPDATE targets SET tls_skip_verify = 0 WHERE parent_id IS NULL AND tls_skip_verify IS NULL;
`,
	// v14: acknowledgement of a firing alert. acked_since records the firing
	// episode (its `since`) the acknowledgement applies to, so an ack mutes
	// only this episode: when the alert resolves and later re-fires with a new
	// since, the ack no longer matches and it demands attention again. acked_at
	// and acked_by are for display and audit.
	`
ALTER TABLE alert_state ADD COLUMN acked_since INTEGER;
ALTER TABLE alert_state ADD COLUMN acked_at INTEGER;
ALTER TABLE alert_state ADD COLUMN acked_by TEXT;
`,
	// v15: how long to keep a target's raw measurements, in seconds. Inheritable
	// like every other setting, and 0 at the root — meaning keep forever, the
	// default that honours the promise of full resolution kept for good. A
	// positive value is the operator's own bound: measurements older than it are
	// deleted whole, never consolidated, so history before the horizon reads as
	// absent rather than as the coarsened average this project refuses to invent.
	`
ALTER TABLE targets ADD COLUMN retention_s INTEGER;
UPDATE targets SET retention_s = 0 WHERE parent_id IS NULL AND retention_s IS NULL;
`,
	// v16: silences. A silence suppresses delivery and attention for matching
	// alerts over a time window — a maintenance window booked ahead, or a known
	// issue muted for a few hours — without touching the rule. A NULL scope
	// column is a wildcard; target_id matches a node and its subtree, and falls
	// away with the node (ON DELETE CASCADE). The window is [starts_at, ends_at).
	`
CREATE TABLE silences (
  id         INTEGER PRIMARY KEY,
  target_id  INTEGER REFERENCES targets(id) ON DELETE CASCADE,
  agent_id   INTEGER,
  rule_id    INTEGER,
  starts_at  INTEGER NOT NULL,
  ends_at    INTEGER NOT NULL,
  reason     TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX silences_window ON silences (ends_at);
`,
	// v17: distribution-shape alert metrics. The metric CHECK has to widen to
	// admit 'shape' and 'bimodality', and two columns (mode, baseline) join the
	// rule — both need rebuilding alert_rules, which alert_state references by a
	// foreign key. Rather than toggle foreign_keys (a no-op inside this
	// transaction), the live alert_state is set aside, the rebuild runs with
	// nothing referencing the old table, and the state is restored against the
	// new one, whose ids are unchanged. Firing and acknowledgement state survives.
	`
CREATE TABLE alert_state_bak AS SELECT * FROM alert_state;
DELETE FROM alert_state;
CREATE TABLE alert_rules_new (
  id              INTEGER PRIMARY KEY,
  target_id       INTEGER NOT NULL REFERENCES targets(id),
  name            TEXT NOT NULL,
  metric          TEXT NOT NULL CHECK (metric IN ('loss','median','p95','spread','shape','bimodality')),
  op              TEXT NOT NULL CHECK (op IN ('>','<')),
  threshold       REAL NOT NULL,
  for_intervals   INTEGER NOT NULL DEFAULT 3,
  clear_intervals INTEGER NOT NULL DEFAULT 3,
  enabled         INTEGER NOT NULL DEFAULT 1,
  mode            TEXT NOT NULL DEFAULT '',
  baseline        TEXT NOT NULL DEFAULT '',
  UNIQUE (target_id, name)
);
INSERT INTO alert_rules_new
  (id, target_id, name, metric, op, threshold, for_intervals, clear_intervals, enabled)
  SELECT id, target_id, name, metric, op, threshold, for_intervals, clear_intervals, enabled
  FROM alert_rules;
DROP TABLE alert_rules;
ALTER TABLE alert_rules_new RENAME TO alert_rules;
INSERT INTO alert_state SELECT * FROM alert_state_bak;
DROP TABLE alert_state_bak;
`,
	// v18: captured reference distributions for golden-baseline shape rules. One
	// per rule, replaced when recaptured, and gone with the rule. The samples are
	// stored in the same encoding measurements use, and what they were taken from
	// (the window, the series) is kept alongside so the UI can say what the
	// reference actually is rather than presenting an anonymous curve.
	`
CREATE TABLE alert_baselines (
  rule_id     INTEGER PRIMARY KEY REFERENCES alert_rules(id) ON DELETE CASCADE,
  target_id   INTEGER NOT NULL,
  agent_id    INTEGER NOT NULL,
  from_ts     INTEGER NOT NULL,
  to_ts       INTEGER NOT NULL,
  intervals   INTEGER NOT NULL,
  samples     BLOB NOT NULL,
  captured_at INTEGER NOT NULL,
  captured_by TEXT NOT NULL DEFAULT ''
);
`,
	// v19: the extra per-packet series a probe can measure beside the round
	// trip, one row per (measurement, series). A side table rather than more
	// columns on measurements: only irtt produces any of these, so columns
	// would be null for every other probe, and the set is open — irtt alone
	// offers one-way delay and server processing time beside the two kept here,
	// and adding one should not rewrite the table every measurement lives in.
	//
	// Absence is meaningful and is the only way this is recorded: a series that
	// the far end gave no timestamps for has no row, which reads as "not
	// measured" rather than as zero jitter.
	`
CREATE TABLE measurement_series (
  target_id INTEGER NOT NULL,
  agent_id  INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  series    TEXT NOT NULL,
  samples   BLOB NOT NULL,
  PRIMARY KEY (target_id, agent_id, ts, series)
) WITHOUT ROWID;
`,
	// v20: which of those series a target graphs. Display only — every series
	// measured is stored whatever this says, so switching one on shows the
	// history it already has rather than starting it from the moment somebody
	// thought to ask. The root defaults to "all": a measurement taken and not
	// shown is one nobody knows to look for.
	`
ALTER TABLE targets ADD COLUMN graph_series TEXT;
UPDATE targets SET graph_series = 'all' WHERE parent_id IS NULL AND graph_series IS NULL;
`,
	// v21: why the local side could not send. FlagSendFailed said that it
	// happened and nothing said why, so a target failing to send one probe an
	// interval was indistinguishable between a far end refusing the packets and
	// this prober never getting them out — a network fault and a bug here,
	// reported identically. Null for every interval that sent everything, and
	// for every measurement taken before this column existed.
	`ALTER TABLE measurements ADD COLUMN send_error INTEGER`,
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
		INSERT OR REPLACE INTO measurements (target_id, agent_id, ts, sent, received, flags, samples, icmp_error, send_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	// A measurement is replaced wholesale, so its old series rows have to go
	// with it. Without the delete, a target whose peer stopped returning
	// timestamps would keep serving the jitter it measured the last time it
	// could — the stalest possible reading, presented as current.
	delSeries, err := tx.PrepareContext(ctx, `
		DELETE FROM measurement_series WHERE target_id = ? AND agent_id = ? AND ts = ?`)
	if err != nil {
		return err
	}
	defer delSeries.Close()
	putSeries, err := tx.PrepareContext(ctx, `
		INSERT INTO measurement_series (target_id, agent_id, ts, series, samples)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer putSeries.Close()
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
			m.Flags, blob, ptrOrNil(m.ICMPErr), ptrOrNil(m.SendErr)); err != nil {
			return err
		}
		if _, err := delSeries.ExecContext(ctx, m.TargetID, m.AgentID, m.TS); err != nil {
			return err
		}
		for _, name := range sortedSeries(m.Series) {
			if !ValidSeries(name) {
				return fmt.Errorf("store: measurement (%d,%d,%d): unknown series %q",
					m.TargetID, m.AgentID, m.TS, name)
			}
			sblob, err := enc.EncodeSigned(m.Series[name])
			if err != nil {
				return fmt.Errorf("store: measurement (%d,%d,%d) series %q: %w",
					m.TargetID, m.AgentID, m.TS, name, err)
			}
			if _, err := putSeries.ExecContext(ctx, m.TargetID, m.AgentID, m.TS, name, sblob); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// sortedSeries returns the series names in a stable order, so a written blob
// set does not depend on map iteration order.
func sortedSeries(m map[string][]int32) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *SQLite) QueryRange(ctx context.Context, targetID, agentID, from, to int64) ([]Measurement, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, sent, received, flags, samples, icmp_error, send_error FROM measurements
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
		var icmpErr, sendErr sql.NullInt64
		if err := rows.Scan(&m.TS, &m.Sent, &m.Received, &m.Flags, &blob, &icmpErr, &sendErr); err != nil {
			return nil, err
		}
		if icmpErr.Valid {
			v := uint16(icmpErr.Int64)
			m.ICMPErr = &v
		}
		if sendErr.Valid {
			v := uint8(sendErr.Int64)
			m.SendErr = &v
		}
		if m.Samples, err = enc.Decode(blob); err != nil {
			return nil, fmt.Errorf("store: measurement (%d,%d,%d): %w", targetID, agentID, m.TS, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachSeries(ctx, targetID, agentID, from, to, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachSeries fills in the extra per-packet distributions for an already-read
// range, in one query rather than one per interval.
func (s *SQLite) attachSeries(ctx context.Context, targetID, agentID, from, to int64, ms []Measurement) error {
	// Deliberately not skipped when the range holds no measurements. An orphan
	// arises from a measurement being deleted, so a window with nothing left in
	// it is exactly where one would sit — returning early here pointed the
	// detector away from its own most likely cause.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, series, samples FROM measurement_series
		WHERE target_id = ? AND agent_id = ? AND ts >= ? AND ts < ?`,
		targetID, agentID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	byTS := make(map[int64]*Measurement, len(ms))
	for i := range ms {
		byTS[ms[i].TS] = &ms[i]
	}
	for rows.Next() {
		var ts int64
		var name string
		var blob []byte
		if err := rows.Scan(&ts, &name, &blob); err != nil {
			return err
		}
		m := byTS[ts]
		if m == nil {
			// A series row with no measurement is a broken invariant, not a
			// row to skip: it means a measurement was deleted without its
			// series, and the next prune would leave it behind forever.
			return fmt.Errorf("store: series %q at (%d,%d,%d) has no measurement",
				name, targetID, agentID, ts)
		}
		vals, err := enc.DecodeSigned(blob)
		if err != nil {
			// Not fatal, unlike an orphan. These series are optional, and
			// failing the read took the whole window with them — the latency
			// graph, the loss rail, the quality flags and baseline capture all
			// returned 500 because one byte of an optional jitter distribution
			// was wrong, with no way to repair it short of sqlite3 against the
			// production database. Absence is what an undecodable distribution
			// honestly is, and it is already how the graph renders "not
			// measured". Say so in the log and keep the interval readable.
			log.Printf("store: measurement (%d,%d,%d) series %q will not decode (%v); "+
				"serving the interval without it", targetID, agentID, ts, name, err)
			continue
		}
		if m.Series == nil {
			m.Series = make(map[string][]int32, 2)
		}
		m.Series[name] = vals
	}
	return rows.Err()
}

// AvailabilitySeries returns just the reachability of each interval in the
// window — sent and received, no samples — for an availability report. It skips
// the per-sample decode QueryRange does, so a month-long window costs a scan of
// small integer columns rather than a decode of every distribution.
func (s *SQLite) AvailabilitySeries(ctx context.Context, targetID, agentID, from, to int64) ([]report.Point, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, sent, received FROM measurements
		WHERE target_id = ? AND agent_id = ? AND ts >= ? AND ts < ?
		ORDER BY ts`, targetID, agentID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []report.Point
	for rows.Next() {
		var p report.Point
		if err := rows.Scan(&p.TS, &p.Sent, &p.Received); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const targetCols = `id, parent_id, name, host, address_family, title, notes,
	hidden, enabled, sort_order, interval_s, pings_per_interval, probe_mode,
	burst_gap_ms, timeout_ms, packet_size, dscp, agents, trace_interval_s, probe_type, probe_port, dns_query, dns_rr_type, http_path, tls_skip_verify, retention_s, graph_series`

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
	var probeType, dnsQuery, dnsRRType, httpPath, graphSeries sql.NullString
	var intervalS, pings, burstGap, timeout, packetSize, dscp, traceInterval sql.NullInt64
	var probePort, tlsSkipVerify, retention sql.NullInt64
	err := rows.Scan(&t.ID, &parentID, &t.Name, &host, &af, &title, &notes,
		&t.Hidden, &t.Enabled, &t.SortOrder, &intervalS, &pings, &probeMode,
		&burstGap, &timeout, &packetSize, &dscp, &agents, &traceInterval,
		&probeType, &probePort, &dnsQuery, &dnsRRType, &httpPath, &tlsSkipVerify, &retention,
		&graphSeries)
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
		ProbeType:        nullStr(probeType),
		ProbePort:        nullInt(probePort),
		DNSQuery:         nullStr(dnsQuery),
		DNSRRType:        nullStr(dnsRRType),
		HTTPPath:         nullStr(httpPath),
		TLSSkipVerify:    nullBool(tlsSkipVerify),
		RetentionS:       nullInt(retention),
		GraphSeries:      nullStr(graphSeries),
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
			burst_gap_ms, timeout_ms, packet_size, dscp, agents, trace_interval_s,
			probe_type, probe_port, dns_query, dns_rr_type, http_path, tls_skip_verify, retention_s,
			graph_series)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			trace_interval_s = excluded.trace_interval_s,
			probe_type = excluded.probe_type,
			probe_port = excluded.probe_port,
			dns_query = excluded.dns_query,
			dns_rr_type = excluded.dns_rr_type,
			http_path = excluded.http_path,
			tls_skip_verify = excluded.tls_skip_verify,
			retention_s = excluded.retention_s,
			graph_series = excluded.graph_series`,
		id, ptrOrNil(t.ParentID), t.Name, ptrOrNil(t.Host), ptrOrNil(t.AddressFamily),
		ptrOrNil(t.Title), ptrOrNil(t.Notes), t.Hidden, t.Enabled, t.SortOrder,
		ptrOrNil(t.Settings.IntervalS), ptrOrNil(t.Settings.PingsPerInterval),
		ptrOrNil(t.Settings.ProbeMode), ptrOrNil(t.Settings.BurstGapMS),
		ptrOrNil(t.Settings.TimeoutMS), ptrOrNil(t.Settings.PacketSize),
		ptrOrNil(t.Settings.DSCP), ptrOrNil(t.Settings.Agents),
		ptrOrNil(t.Settings.TraceIntervalS),
		ptrOrNil(t.Settings.ProbeType), ptrOrNil(t.Settings.ProbePort),
		ptrOrNil(t.Settings.DNSQuery), ptrOrNil(t.Settings.DNSRRType),
		ptrOrNil(t.Settings.HTTPPath), ptrOrNil(t.Settings.TLSSkipVerify),
		ptrOrNil(t.Settings.RetentionS), ptrOrNil(t.Settings.GraphSeries))
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

// PruneMeasurements deletes one target's measurements older than cutoff (a Unix
// second), across every agent that measured it, and returns how many rows went.
//
// It deletes in time slices of sliceS seconds, oldest first, rather than in one
// statement: a target that has accumulated months of data before retention is
// first switched on would otherwise hold a single write lock for the length of
// a delete of hundreds of thousands of rows, and this project will not let
// housekeeping stall the prober. Each slice is a short lock; the loop yields the
// database between them and stops as soon as the oldest surviving row is within
// the horizon. Deletion is by whole interval — a pruned measurement is gone, not
// summarised, so history before the horizon reads as absent, never as an average.
func (s *SQLite) PruneMeasurements(ctx context.Context, targetID, cutoff, sliceS int64) (int64, error) {
	if sliceS <= 0 {
		sliceS = 6 * 3600
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var oldest sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			"SELECT MIN(ts) FROM measurements WHERE target_id = ?", targetID).Scan(&oldest); err != nil {
			return total, err
		}
		if !oldest.Valid || oldest.Int64 >= cutoff {
			return total, nil
		}
		end := oldest.Int64 + sliceS
		if end > cutoff {
			end = cutoff
		}
		// The measurement and its extra series go in one transaction. Deleting
		// them separately and dying in between would leave series rows whose
		// measurement is gone, which is not a tidiness problem: reading a range
		// containing one is a hard error, so a crash mid-prune would make that
		// window unreadable rather than merely untidy.
		n, err := func() (int64, error) {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return 0, err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM measurement_series WHERE target_id = ? AND ts < ?", targetID, end); err != nil {
				return 0, err
			}
			res, err := tx.ExecContext(ctx,
				"DELETE FROM measurements WHERE target_id = ? AND ts < ?", targetID, end)
			if err != nil {
				return 0, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, err
			}
			return n, tx.Commit()
		}()
		if err != nil {
			return total, err
		}
		total += n
		// end is strictly greater than the oldest row, so a slice always removes
		// at least that row; a zero here would mean the count is untrustworthy,
		// and looping on it would spin. Stop rather than risk that.
		if n == 0 {
			return total, nil
		}
	}
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

// nullBool reads a boolean stored the way SQLite stores one: as an integer.
// Scanning into sql.NullBool would work with today's driver, but going through
// the integer the column actually holds keeps the mapping explicit and does
// not depend on a driver's willingness to convert.
func nullBool(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

// ptrOrNil converts a *T into a driver value: the pointee, or SQL NULL.
func ptrOrNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
