package probe

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
)

// maskCollector sends n probes and answers every one not in lostAt, 5 ms after
// it was sent.
func maskCollector(n int, lostAt ...int) *collector {
	var late atomic.Int64
	c := newCollector(n, &late)
	base := time.Now().Add(-10 * time.Second)
	skip := map[int]bool{}
	for _, i := range lostAt {
		skip[i] = true
	}
	for i := range n {
		c.markSent(i, uint16(i), base)
		if !skip[i] {
			c.onRX(i, base.Add(5*time.Millisecond), true)
		}
	}
	return c
}

func maskSpec(n int) TargetSpec {
	return TargetSpec{TargetID: 1, Host: "192.0.2.1", Pings: n, TimeoutMS: 1000, Mode: "burst", ProbeType: "icmp"}
}

// lostPositions decodes a measurement's mask and checks the invariant every
// recorded mask has to keep: as many bits as Sent - Received.
func lostPositions(t *testing.T, m store.Measurement) (spread bool, at []int) {
	t.Helper()
	if m.LossMask == nil {
		t.Fatal("no mask recorded")
	}
	spread, lost, err := store.DecodeLossMask(m.LossMask, m.Sent, m.Received)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range lost {
		if l {
			at = append(at, i)
		}
	}
	if len(at) != m.Sent-m.Received {
		t.Errorf("%d bits set, Sent-Received = %d", len(at), m.Sent-m.Received)
	}
	return spread, at
}

// The point of the mask: a burst whose tail was lost is told apart from one
// whose losses were scattered, after the RTTs have been sorted.
func TestLossMaskBurstTail(t *testing.T) {
	c := maskCollector(20, 15, 16, 17, 18, 19)
	m := c.finalize(maskSpec(20), 100, conditions{})
	spread, at := lostPositions(t, m)
	if spread {
		t.Error("burst recorded as spread")
	}
	if len(at) != 5 || at[0] != 15 || at[4] != 19 {
		t.Errorf("lost = %v, want 15..19", at)
	}
}

// The header bit is what lets a reader know whether position means "later in a
// burst" or "later in the interval".
func TestLossMaskSpreadHeader(t *testing.T) {
	spec := maskSpec(8)
	spec.Mode = "spread"
	m := maskCollector(8, 2).finalize(spec, 100, conditions{})
	if spread, at := lostPositions(t, m); !spread || len(at) != 1 || at[0] != 2 {
		t.Errorf("spread=%v lost=%v, want true [2]", spread, at)
	}
}

// A late reply and an ICMP error both give no sample, so both are lost in the
// mask; the mask must agree with Received or the two disagree about the loss.
func TestLossMaskLateReplyAndICMPError(t *testing.T) {
	c := maskCollector(5, 3)
	c.pings[1].rx = c.pings[1].txUser.Add(1500 * time.Millisecond) // after the 1 s timeout
	c.pings[3].icmpErr, c.pings[3].icmpType, c.pings[3].icmpCode = true, 3, 1
	m := c.finalize(maskSpec(5), 100, conditions{})
	if _, at := lostPositions(t, m); len(at) != 2 || at[0] != 1 || at[1] != 3 {
		t.Errorf("lost = %v, want [1 3]", at)
	}
}

// Each case is an interval whose positions are unknown or whose mask says
// nothing; storing one would either mislead or cost bytes on every healthy
// series.
func TestLossMaskAbsent(t *testing.T) {
	cases := map[string]func() (*collector, TargetSpec, conditions){
		"no loss": func() (*collector, TargetSpec, conditions) {
			return maskCollector(4), maskSpec(4), conditions{}
		},
		"total loss": func() (*collector, TargetSpec, conditions) {
			return maskCollector(4, 0, 1, 2, 3), maskSpec(4), conditions{}
		},
		"send failure": func() (*collector, TargetSpec, conditions) {
			c := maskCollector(4, 2)
			c.markSendFailed(2, store.SendReasonRefused)
			return c, maskSpec(4), conditions{}
		},
		"truncated": func() (*collector, TargetSpec, conditions) {
			return maskCollector(4, 2), maskSpec(4), conditions{truncated: true}
		},
		"socket overflow": func() (*collector, TargetSpec, conditions) {
			return maskCollector(4, 2), maskSpec(4), conditions{overflowed: true}
		},
		"clock step": func() (*collector, TargetSpec, conditions) {
			c := maskCollector(4, 2)
			c.startWall = c.startWall.Add(time.Hour)
			return c, maskSpec(4), conditions{}
		},
		"unsent tail": func() (*collector, TargetSpec, conditions) {
			var late atomic.Int64
			c := newCollector(4, &late)
			base := time.Now().Add(-10 * time.Second)
			for i := range 3 {
				c.markSent(i, uint16(i), base)
				if i != 1 {
					c.onRX(i, base.Add(5*time.Millisecond), true)
				}
			}
			return c, maskSpec(4), conditions{}
		},
		"irtt": func() (*collector, TargetSpec, conditions) {
			spec := maskSpec(4)
			spec.ProbeType = "irtt"
			return maskCollector(4, 2), spec, conditions{}
		},
	}
	for name, mk := range cases {
		c, spec, cond := mk()
		if m := c.finalize(spec, 100, cond); m.LossMask != nil {
			t.Errorf("%s: mask recorded: %v", name, m.LossMask)
		}
	}
}

// The probe types that track probes by send index all record one; this
// protects the allowlist from being narrowed to icmp by accident.
func TestLossMaskProbeTypes(t *testing.T) {
	for _, typ := range []string{"", "icmp", "dns", "tcp", "http", "https"} {
		spec := maskSpec(4)
		spec.ProbeType = typ
		if m := maskCollector(4, 3).finalize(spec, 100, conditions{}); m.LossMask == nil {
			t.Errorf("probe type %q: no mask", typ)
		}
	}
}
