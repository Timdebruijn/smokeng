// Package store persists targets and measurements (DESIGN.md §3, §6).
package store

import (
	"context"

	"smokeng/internal/alert"
	"smokeng/internal/tree"
)

// Measurement is one finalized interval for one (target, agent) series.
type Measurement struct {
	TargetID int64
	AgentID  int64
	TS       int64 // interval start, unix seconds, UTC-aligned
	// Sent counts probes attempted, including any the local stack refused to
	// transmit, so loss is measured against what was asked for.
	Sent     int
	Received int      // invariant: equals len(Samples); the blob is authoritative
	Flags    uint8    // measurement-quality flags, see below
	Samples  []uint32 // RTTs in µs, sorted ascending
	// ICMPErr is the ICMP type and code most often reported in this interval
	// (see ICMPError), or nil when no ping drew an error.
	ICMPErr *uint16
}

// Measurement-quality flags (DESIGN.md §3.4). Zero means a clean measurement:
// full kernel timestamping over a datagram ICMP socket, no dropped packets,
// no clock disturbance. Anything that could make the numbers mean less than
// they appear to is recorded here rather than left to be inferred.
const (
	FlagUserspaceTX uint8 = 1 << 0 // TX timestamp taken in userspace
	FlagUserspaceRX uint8 = 1 << 1 // RX timestamp taken in userspace
	FlagRawSocket   uint8 = 1 << 2 // raw-socket fallback in use
	// FlagSocketOverflow means the socket's receive queue overflowed during
	// this interval: replies were dropped by the kernel before we could read
	// them, so some of the loss recorded here is measurement error, not the
	// network. Without this, a busy host looks like a lossy network.
	FlagSocketOverflow uint8 = 1 << 3
	// FlagClockStep means the wall clock jumped during this interval. Kernel
	// timestamps are CLOCK_REALTIME, so a step lands directly in the RTTs and
	// would otherwise be drawn as a latency spike that never happened.
	FlagClockStep uint8 = 1 << 5
	// FlagICMPError means at least one ping in this interval was answered
	// with an ICMP error rather than going unanswered. The loss is refusal,
	// not silence — a firewall or a dead route, not a black hole — and
	// ICMPError names which.
	FlagICMPError uint8 = 1 << 4
	// FlagSendFailed means the local stack refused to transmit some pings at
	// all (no route, or a local firewall rule). They count as attempted and
	// lost: a target we cannot reach is a failing target, not an unmeasured
	// one, and must not render as an empty graph.
	FlagSendFailed uint8 = 1 << 6
)

// AlertInput narrows a measurement to what alerting reads, translating the
// quality flags into whether each kind of question can be answered honestly.
// The mapping lives here, next to the flags themselves, so there is one place
// that decides what a flag means.
func (m *Measurement) AlertInput() alert.Input {
	return alert.Input{
		TargetID:    m.TargetID,
		AgentID:     m.AgentID,
		TS:          m.TS,
		Sent:        m.Sent,
		Received:    m.Received,
		Samples:     m.Samples,
		LossTrusted: m.Flags&FlagSocketOverflow == 0,
		RTTTrusted:  m.Flags&FlagClockStep == 0,
	}
}

// ICMPError packs an ICMP type and code into one value for storage.
func ICMPError(icmpType, code uint8) uint16 { return uint16(icmpType)<<8 | uint16(code) }

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
	// DeleteTarget removes a target row. Measurements are left untouched —
	// history is only ever destroyed by an explicit operator action. Callers
	// must delete children before their parent (foreign key).
	DeleteTarget(ctx context.Context, id int64) error
	// RecordResolution appends a row to the DNS change log (DESIGN.md §5.4).
	RecordResolution(ctx context.Context, targetID, ts int64, address string) error
	// LastResolution returns the most recently logged address for the target,
	// or "" when none has been recorded.
	LastResolution(ctx context.Context, targetID int64) (string, error)
	// LastPath and RecordPath keep the route change log (DESIGN.md §9a).
	// Only changes are written: a route is stable for days and then is not.
	LastPath(ctx context.Context, targetID, agentID int64) (string, error)
	RecordPath(ctx context.Context, targetID, agentID, ts int64, hops string) error
	Close() error
}
