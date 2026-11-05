package store

import (
	"context"
	"fmt"
	"math/bits"
	"slices"

	"github.com/timdebruijn/smokeng/internal/store/enc"
)

// ColumnQuantiles is how many values a pooled column carries when its pool
// holds more samples than that. It is about the height of a plot in pixels:
// the distribution is drawn as density along one pixel column, so more values
// than the column has pixels are not seen, and fewer would be.
const ColumnQuantiles = 256

// Column is one pixel column of a pooled read: every interval whose start
// falls in [From, To), pooled. Nothing is averaged. Sent and Received are
// sums, so loss over the column is 1 - Received/Sent over all its probes, and
// Quantiles are order statistics of the pooled samples themselves — never a
// median of medians.
type Column struct {
	From, To int64
	// Rounds is how many intervals were stored in the column: coverage is
	// Rounds against what the target's interval says there should be.
	Rounds   int
	Sent     int
	Received int
	// Flags is every quality flag any interval in the column carried, and
	// Flagged how many intervals carried one.
	Flags   uint8
	Flagged int
	// Quantiles holds the pooled samples, sorted ascending, when there are at
	// most ColumnQuantiles of them; otherwise ColumnQuantiles evenly spaced
	// order statistics (the sample at each mid-rank), each standing for an
	// equal share of the column's replies. Empty when nothing came back.
	Quantiles []uint32
}

// QueryColumns reads one series over [from, to) for a plot n pixel columns
// wide. When the window holds at most n intervals they come back as they are
// (raw, as QueryRange returns them): pooling would change nothing a plot can
// show, and keeps what it would lose — each interval's own errors and series.
// Otherwise every interval is pooled into the column its start falls in, and
// cols holds the columns that have any; a column with no interval is a gap,
// and stays one. Exactly one of raw and cols is non-nil unless the window is
// empty.
//
// The decision is made while reading, in one pass: rows are kept until there
// are more than n (or hold more samples than QueryRange returns), and then
// turned into columns. Counting first would read the
// window twice, since a count over this table walks the same pages.
func (s *SQLite) QueryColumns(ctx context.Context, targetID, agentID, from, to int64, n int) (raw []Measurement, cols []Column, err error) {
	if n < 1 {
		return nil, nil, fmt.Errorf("store: %d columns", n)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, sent, received, flags, samples, icmp_error, send_error FROM measurements
		WHERE target_id = ? AND agent_id = ? AND ts >= ? AND ts < ?
		ORDER BY ts`, targetID, agentID, from, to)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var p *pooler
	var samples, read int
	for rows.Next() {
		if read++; read > maxPooledRows {
			return nil, nil, tooLarge("intervals", maxPooledRows)
		}
		m := Measurement{TargetID: targetID, AgentID: agentID}
		var blob []byte
		if err := rows.Scan(&m.TS, &m.Sent, &m.Received, &m.Flags, &blob, &m.ICMPErr, &m.SendErr); err != nil {
			return nil, nil, err
		}
		if m.Samples, err = enc.Decode(blob); err != nil {
			return nil, nil, fmt.Errorf("store: measurement (%d,%d,%d): %w", targetID, agentID, m.TS, err)
		}
		if p != nil {
			if err := p.add(m); err != nil {
				return nil, nil, err
			}
			continue
		}
		raw = append(raw, m)
		// Too many intervals for the columns, or more samples than a raw read
		// returns (the bound QueryRange keeps): either way the answer is the
		// columns, whose size depends on n alone.
		if samples += m.Received; len(raw) > n || samples > maxSamplesPerRead {
			p = newPooler(from, to, n)
			for _, r := range raw {
				if err := p.add(r); err != nil {
					return nil, nil, err
				}
			}
			raw = nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if p != nil {
		return nil, p.finish(), nil
	}
	if err := s.attachSeries(ctx, targetID, agentID, from, to, raw); err != nil {
		return nil, nil, err
	}
	return raw, nil, nil
}

// pooler turns rows, in ts order, into columns. It holds one column's samples
// at a time.
type pooler struct {
	from int64
	span uint64
	n    uint64
	cur  *Column
	idx  uint64
	pool []uint32
	out  []Column
}

func newPooler(from, to int64, n int) *pooler {
	return &pooler{from: from, span: uint64(to) - uint64(from), n: uint64(n)}
}

// column is the index of the column ts falls in: floor((ts-from)·n/span),
// done in 128 bits because the product overflows 64 for a wide window.
func (p *pooler) column(ts int64) uint64 {
	hi, lo := bits.Mul64(uint64(ts)-uint64(p.from), p.n)
	q, _ := bits.Div64(hi, lo, p.span) // hi < span, since ts-from < span
	return q
}

// start is where column i begins: the first second whose column is i,
// ceil(i·span/n).
func (p *pooler) start(i uint64) int64 {
	hi, lo := bits.Mul64(i, p.span)
	q, r := bits.Div64(hi, lo, p.n) // hi < n, since i <= n
	if r != 0 {
		q++
	}
	return int64(uint64(p.from) + q)
}

func (p *pooler) add(m Measurement) error {
	i := p.column(m.TS)
	if p.cur == nil || i != p.idx {
		p.close()
		p.idx = i
		p.cur = &Column{From: p.start(i), To: p.start(i + 1)}
	}
	c := p.cur
	c.Rounds++
	c.Sent += m.Sent
	c.Received += m.Received
	if m.Flags != 0 {
		c.Flags |= m.Flags
		c.Flagged++
	}
	// One column's pool is what stays in memory, so this is the bound: a plot
	// asking for a year in one column is asking for every sample at once.
	if len(p.pool)+len(m.Samples) > maxSamplesPerRead {
		return tooLarge("samples in one column", maxSamplesPerRead)
	}
	p.pool = append(p.pool, m.Samples...)
	return nil
}

func (p *pooler) close() {
	if p.cur == nil {
		return
	}
	p.cur.Quantiles = quantiles(p.pool, ColumnQuantiles)
	p.out = append(p.out, *p.cur)
	p.cur = nil
	p.pool = p.pool[:0]
}

func (p *pooler) finish() []Column {
	p.close()
	return p.out
}

// quantiles sorts pool and returns it whole when it has at most q values, or q
// order statistics at the mid-ranks (k+½)/q otherwise. Each is a sample that
// was measured, not an interpolation between two.
func quantiles(pool []uint32, q int) []uint32 {
	slices.Sort(pool)
	if len(pool) <= q {
		return slices.Clone(pool)
	}
	out := make([]uint32, q)
	for k := range out {
		out[k] = pool[(2*k+1)*len(pool)/(2*q)]
	}
	return out
}
