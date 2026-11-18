package store

import "strconv"

// familyOfAddress is the family of a logged address, in SQL: an IPv6 literal
// holds a colon. So does an IPv4 address written as IPv4-mapped IPv6
// ("::ffff:192.0.2.1"), which a v4 target may have as its host and which is
// probed over IPv4; read as IPv6 it would make that target look switched and
// file its whole history as unknown.
const familyOfAddress = `CASE WHEN instr(address, ':') > 0 AND address NOT LIKE '::ffff:%.%' THEN 6 ELSE 4 END`

// familyBackfillSQL builds family_backfill, one row per target that says which
// family its existing history was measured over. It is its own constant so a
// later migration that needs the same answer can run it again; it creates the
// table and the caller drops it.
//
//   - family is the target's current address_family (0 for a group node or a
//     target that is gone: a gone target is simply absent from the table).
//   - switched is whether the resolutions log holds an address of the other
//     family, that is, whether the family was changed at some point.
//   - since is, for a switched target, the first resolution after the last
//     address of the other family: from then on the local prober measured
//     family. NULL when there is none, and for a target that never switched.
//
// A target switched more than once has only its last stretch placed: the log
// could place the earlier stretches of the local prober too, but not the
// interval in which each switch fell, and a target is switched seldom enough
// that the rule stays this short. What it cannot place is unknown, not lost.
const familyBackfillSQL = `
CREATE TABLE family_backfill AS
WITH t AS (
  SELECT id AS target_id,
         CASE address_family WHEN 'v4' THEN 4 WHEN 'v6' THEN 6 ELSE 0 END AS family
  FROM targets
), r AS (
  SELECT target_id, ts, ` + familyOfAddress + ` AS fam
  FROM resolutions
), last_other AS (
  SELECT t.target_id, MAX(r.ts) AS ts
  FROM t JOIN r ON r.target_id = t.target_id AND r.fam <> t.family
  GROUP BY t.target_id
)
SELECT t.target_id, t.family,
       lo.ts IS NOT NULL AS switched,
       (SELECT MIN(r.ts) FROM r WHERE r.target_id = t.target_id AND r.ts > lo.ts) AS since
FROM t LEFT JOIN last_other lo ON lo.target_id = t.target_id;
`

// familyOfRow is the family a pre-v28 row of measurements, measurement_series,
// positional_loss or paths gets, written against a row aliased m that is joined
// to family_backfill b. Where the family cannot be established it is 0
// (tree.FamilyUnknown) rather than a guess:
//
//   - no b row: the target no longer exists (DeleteTarget keeps its history);
//   - never switched: every row was measured over the current family;
//   - switched: only the local prober's rows from the first resolution of the
//     current family on are certain. A remote agent changes family when it next
//     syncs the target, which nothing recorded, so none of its rows are.
var familyOfRow = `COALESCE(CASE
    WHEN b.switched = 0 THEN b.family
    WHEN m.agent_id = ` + strconv.FormatInt(LocalAgentID, 10) + ` AND m.ts >= b.since THEN b.family
  END, 0)`

// familyMigration is schema v28. Each table is rebuilt (a key cannot be
// altered), with every column, default and CHECK it had, and the indexes
// dropped with the old table are created again.
func familyMigration() string {
	return familyBackfillSQL + `
CREATE TABLE measurements_new (
  target_id  INTEGER NOT NULL,
  agent_id   INTEGER NOT NULL DEFAULT 0,
  family     INTEGER NOT NULL,
  ts         INTEGER NOT NULL,
  sent       INTEGER NOT NULL,
  received   INTEGER NOT NULL,
  flags      INTEGER NOT NULL DEFAULT 0,
  samples    BLOB NOT NULL,
  icmp_error INTEGER,
  submitted  INTEGER NOT NULL DEFAULT 0,
  send_error INTEGER,
  loss_mask  BLOB,
  PRIMARY KEY (target_id, agent_id, family, ts)
) WITHOUT ROWID;
INSERT INTO measurements_new
  (target_id, agent_id, family, ts, sent, received, flags, samples, icmp_error, submitted, send_error, loss_mask)
  SELECT m.target_id, m.agent_id, ` + familyOfRow + `, m.ts, m.sent, m.received, m.flags, m.samples,
         m.icmp_error, m.submitted, m.send_error, m.loss_mask
  FROM measurements m LEFT JOIN family_backfill b ON b.target_id = m.target_id;
DROP TABLE measurements;
ALTER TABLE measurements_new RENAME TO measurements;
CREATE INDEX measurements_pending ON measurements (ts) WHERE submitted = 0;
CREATE INDEX measurements_masked ON measurements (target_id, agent_id, family, ts) WHERE loss_mask IS NOT NULL;

CREATE TABLE measurement_series_new (
  target_id INTEGER NOT NULL,
  agent_id  INTEGER NOT NULL,
  family    INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  series    TEXT NOT NULL,
  samples   BLOB NOT NULL,
  PRIMARY KEY (target_id, agent_id, family, ts, series)
) WITHOUT ROWID;
INSERT INTO measurement_series_new (target_id, agent_id, family, ts, series, samples)
  SELECT m.target_id, m.agent_id, ` + familyOfRow + `, m.ts, m.series, m.samples
  FROM measurement_series m LEFT JOIN family_backfill b ON b.target_id = m.target_id;
DROP TABLE measurement_series;
ALTER TABLE measurement_series_new RENAME TO measurement_series;

CREATE TABLE positional_loss_new (
  target_id INTEGER NOT NULL,
  agent_id  INTEGER NOT NULL,
  family    INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  p         REAL    NOT NULL,
  intervals INTEGER NOT NULL,
  lost      INTEGER NOT NULL,
  counts    BLOB    NOT NULL,
  PRIMARY KEY (target_id, agent_id, family, ts)
) WITHOUT ROWID;
INSERT INTO positional_loss_new (target_id, agent_id, family, ts, p, intervals, lost, counts)
  SELECT m.target_id, m.agent_id, ` + familyOfRow + `, m.ts, m.p, m.intervals, m.lost, m.counts
  FROM positional_loss m LEFT JOIN family_backfill b ON b.target_id = m.target_id;
DROP TABLE positional_loss;
ALTER TABLE positional_loss_new RENAME TO positional_loss;

CREATE TABLE paths_new (
  target_id INTEGER NOT NULL,
  agent_id  INTEGER NOT NULL,
  family    INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  hops      TEXT NOT NULL,
  submitted INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (target_id, agent_id, family, ts)
) WITHOUT ROWID;
INSERT INTO paths_new (target_id, agent_id, family, ts, hops, submitted)
  SELECT m.target_id, m.agent_id, ` + familyOfRow + `, m.ts, m.hops, m.submitted
  FROM paths m LEFT JOIN family_backfill b ON b.target_id = m.target_id;
DROP TABLE paths;
ALTER TABLE paths_new RENAME TO paths;
CREATE INDEX paths_pending ON paths (ts) WHERE submitted = 0;

CREATE TABLE resolutions_new (
  target_id INTEGER NOT NULL,
  family    INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  address   TEXT NOT NULL,
  PRIMARY KEY (target_id, family, ts)
);
INSERT INTO resolutions_new (target_id, family, ts, address)
  SELECT target_id, ` + familyOfAddress + `, ts, address
  FROM resolutions;
DROP TABLE resolutions;
ALTER TABLE resolutions_new RENAME TO resolutions;

DROP TABLE family_backfill;
`
}
