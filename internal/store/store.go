// Package store persists targets and measurements (DESIGN.md §3, §6).
package store

import (
	"context"

	"smokeng/internal/tree"
)

// Measurement is one finalized interval for one (target, agent) series.
type Measurement struct {
	TargetID int64
	AgentID  int64
	TS       int64 // interval start, unix seconds, UTC-aligned
	Sent     int
	Received int      // invariant: equals len(Samples); the blob is authoritative
	Flags    uint8    // timestamp-degradation flags, see below
	Samples  []uint32 // RTTs in µs, sorted ascending
}

// Timestamp-degradation flags (DESIGN.md §3.4). Zero means full kernel
// timestamping over a datagram ICMP socket.
const (
	FlagUserspaceTX uint8 = 1 << 0 // TX timestamp taken in userspace
	FlagUserspaceRX uint8 = 1 << 1 // RX timestamp taken in userspace
	FlagRawSocket   uint8 = 1 << 2 // raw-socket fallback in use
)

// LocalAgentID is the master's built-in prober (DESIGN.md §2).
const LocalAgentID int64 = 0

// Store is the narrow persistence interface (DESIGN.md §6). It is the
// migration seam to a different backend; keep it exactly as small as its
// callers require.
type Store interface {
	// WriteMeasurements writes a batch in one transaction. Writes are
	// idempotent: rewriting an existing (target, agent, ts) row is a no-op
	// replacement, which is the real replay defense for ingest (DESIGN.md §9).
	WriteMeasurements(ctx context.Context, ms []Measurement) error
	// QueryRange returns one series over [from, to), ordered by ts.
	QueryRange(ctx context.Context, targetID, agentID, from, to int64) ([]Measurement, error)
	ListTargets(ctx context.Context) ([]tree.Target, error)
	// UpsertTarget inserts (ID == 0, assigning t.ID) or updates a target.
	UpsertTarget(ctx context.Context, t *tree.Target) error
	Close() error
}
