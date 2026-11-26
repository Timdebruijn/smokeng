package api

import (
	"strings"
	"time"

	"github.com/timdebruijn/smokeng/internal/ingest"
	"github.com/timdebruijn/smokeng/internal/probe"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// How far past its cadence an agent has to be before it is called late, and
// before it is called silent. Two cadences absorbs one slow push and one long
// probe; ten is the better part of an hour for a five-minute target, which is
// no longer a delay.
const (
	agentLateAfter   = 2
	agentSilentAfter = 10
)

// agentCadences returns, per agent name, how often that agent should be heard
// from: the shortest interval among the enabled host targets it measures, plus
// the push cadence. An agent reports when it has measurements, so this is the
// longest it should go quiet. A target the prober skips as unmeasurable (a tcp
// target without a port) does not count, since it never produces anything to
// report. An agent that measures nothing has no entry: its silence says nothing.
//
// Only the targets the caller can see count, so the figure tells a scoped
// caller nothing about targets outside their scope.
func agentCadences(sc *Scope, targets []tree.Target) (map[string]int64, error) {
	out := map[string]int64{}
	for i := range targets {
		n := &targets[i]
		if n.Host == nil || !n.Enabled || !sc.Visible(n.ID) {
			continue
		}
		res, err := sc.tr.Resolve(n.ID)
		if err != nil {
			return nil, err
		}
		if !probe.Measurable(res) {
			continue
		}
		every := int64(res.IntervalS.Effective) + int64(ingest.PushEvery/time.Second)
		for _, name := range strings.Fields(res.Agents.Effective) {
			if cur, ok := out[name]; !ok || every < cur {
				out[name] = every
			}
		}
	}
	return out, nil
}

// agentState says whether an agent with the given cadence has been heard from
// as often as it should have been, at now.
func agentState(lastSeen, every, now int64) string {
	switch {
	case lastSeen == 0:
		return "never"
	case now-lastSeen <= agentLateAfter*every:
		return "reporting"
	case now-lastSeen <= agentSilentAfter*every:
		return "late"
	default:
		return "silent"
	}
}
