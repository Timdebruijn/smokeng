package main

import (
	"context"
	"log"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// retentionSliceS bounds how much a single prune statement removes, so clearing
// a long backlog is a sequence of short write locks rather than one long one.
const retentionSliceS = 6 * 3600

// runRetention prunes each target's measurements past its effective retention on
// a timer. retention_s is 0 — keep forever — by default and per target, so on a
// typical install this resolves the tree once an interval and finds nothing to
// do. It sweeps once at startup so switching retention on takes effect without
// waiting a whole interval, then on every tick until ctx is cancelled.
func runRetention(ctx context.Context, st *store.SQLite, interval time.Duration) {
	sweepRetention(ctx, st)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepRetention(ctx, st)
		}
	}
}

func sweepRetention(ctx context.Context, st *store.SQLite) {
	targets, err := st.ListTargets(ctx)
	if err != nil {
		log.Printf("retention: list targets: %v", err)
		return
	}
	tr, err := tree.New(targets)
	if err != nil {
		// An unresolvable tree is the config layer's problem to surface; pruning
		// against a half-understood tree is worse than skipping this sweep.
		log.Printf("retention: %v", err)
		return
	}
	now := time.Now().Unix()
	var total int64
	for i := range targets {
		n := &targets[i]
		// Groups hold no measurements of their own. Disabled leaves still do, and
		// their history is still subject to the horizon, so they are not skipped.
		if n.Host == nil {
			continue
		}
		res, err := tr.Resolve(n.ID)
		if err != nil {
			log.Printf("retention: resolve %d: %v", n.ID, err)
			continue
		}
		keep := res.RetentionS.Effective
		if keep <= 0 {
			continue // 0 means keep forever
		}
		deleted, err := st.PruneMeasurements(ctx, n.ID, now-int64(keep), retentionSliceS)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("retention: prune target %d: %v", n.ID, err)
			continue
		}
		total += deleted
	}
	if total > 0 {
		log.Printf("retention: pruned %d measurement(s) past their horizon", total)
	}
}
