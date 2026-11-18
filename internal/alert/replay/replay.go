// Package replay runs alert rules over stored history to show when they would
// have fired and resolved. It evaluates with the same step the live manager
// uses, one measurement at a time and oldest first, so what it reports is what
// the rule would have done and not a second implementation's opinion. It reads
// and never writes; flag parsing and printing belong to the caller.
package replay

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Source is what a replay reads. It has no writing method on purpose.
type Source interface {
	ListTargets(ctx context.Context) ([]tree.Target, error)
	ListAlertRules(ctx context.Context) ([]alert.Rule, error)
	ListAlertBaselines(ctx context.Context) ([]alert.Baselined, error)
	ListSilences(ctx context.Context) ([]alert.Silence, error)
	AgentNames(ctx context.Context) (map[int64]string, error)
	QueryRange(ctx context.Context, targetID, agentID, from, to int64) ([]store.Measurement, error)
}

// Mode says what a replay does with silences.
type Mode string

const (
	// SilenceReport lists every episode and marks the silenced ones. The default.
	SilenceReport Mode = "report"
	// SilenceRespect leaves out episodes whose fire was silenced, counting them
	// only in the suppressed total.
	SilenceRespect Mode = "respect"
	// SilenceIgnore does not consult silences at all.
	SilenceIgnore Mode = "ignore"
)

// Options say what to replay and over what.
type Options struct {
	RuleName  string       // optional filter; required unless FileRules is set
	FileRules []alert.Rule // rules from a file that is not imported, ids 0; nil replays the stored rules
	From, To  int64        // unix seconds, [From, To)
	Target    string       // optional subtree, as tree paths are written ("/Production/web")
	Vantage   string       // optional agent name; "local" is the master's own prober
	Silences  Mode         // empty means SilenceReport
}

// chunkIntervals is how many intervals of one series are read at a time. A
// series is never held whole: 500 intervals of even a 1 s, 100-ping target is
// 50,000 samples, far below the limit one QueryRange enforces.
const chunkIntervals = 500

// maxParallel is how many series are replayed at once, at most.
const maxParallel = 4

// Report is the result of a replay. The JSON form is a stable schema.
type Report struct {
	Schema   int            `json:"schema"`
	From     time.Time      `json:"from"`
	To       time.Time      `json:"to"`
	Silences Mode           `json:"silences"`
	Series   []SeriesReport `json:"series"`
	Totals   Totals         `json:"totals"`
	Warnings []string       `json:"warnings"`
	Notes    []string       `json:"notes"`
}

// SeriesReport is one rule on one (target, agent) series.
type SeriesReport struct {
	Rule           string    `json:"rule"`
	RuleNode       string    `json:"rule_node"`
	Condition      string    `json:"condition"`
	Target         string    `json:"target"`
	TargetID       int64     `json:"target_id"`
	Vantage        string    `json:"vantage"`
	AgentID        int64     `json:"agent_id"`
	Intervals      int       `json:"intervals"`
	Fired          int       `json:"fired"`
	Resolved       int       `json:"resolved"`
	FiringSeconds  int64     `json:"firing_seconds"`
	LongestSeconds int64     `json:"longest_seconds"`
	Episodes       []Episode `json:"episodes"`
	Warnings       []string  `json:"warnings"`
}

// Episode is one firing period.
type Episode struct {
	Start           time.Time  `json:"start"`
	End             *time.Time `json:"end,omitempty"` // absent while still firing at the end of the window
	Open            bool       `json:"open"`
	Seconds         int64      `json:"seconds"`
	CrossedAt       float64    `json:"crossed_at"`
	Peak            float64    `json:"peak"`
	FireSilenced    bool       `json:"fire_silenced"`
	ResolveSilenced bool       `json:"resolve_silenced"`
}

// Totals sum a report.
type Totals struct {
	Series         int     `json:"series"`
	SeriesFiring   int     `json:"series_firing"`
	Episodes       int     `json:"episodes"`
	EpisodesPerDay float64 `json:"episodes_per_day"`
	Suppressed     int     `json:"suppressed"`
}

// Notes say what a replay is not. They are part of every report because the
// output looks like a notification log and is not one.
var notes = []string{
	"These are rule transitions, not notifications: Alertmanager grouping, repeats and routing are not simulated.",
	"Hysteresis starts clean (not firing) at the start of the window, so an alert that was already firing then shows up as firing a little later.",
	"The live engine re-warms shape baselines after every restart; the replay does not know about restarts.",
	"Gap detection uses each target's current interval_s, which may differ from what it was when the data was measured.",
	"Silences that were deleted are not known.",
	"Flagged intervals (positional loss) are the ones the master flagged when it stored them, read as no loss by loss rules under the target's current positional_loss setting.",
	"An interval that reached the master after a later one of its series was not evaluated live; the replay evaluates it in its place.",
}

// ruleRun is one rule's progress through one series.
type ruleRun struct {
	rule      alert.Rule
	golden    []uint32
	ser       alert.Series
	out       *SeriesReport
	node      int64 // the node the rule is defined on
	warmStart int64
	wanted    int
	found     int
}

// Run replays the selected rules over [From, To).
func Run(ctx context.Context, src Source, opts Options) (*Report, error) {
	if opts.To <= opts.From {
		return nil, errors.New("replay: the window must end after it starts")
	}
	mode := opts.Silences
	switch mode {
	case "":
		mode = SilenceReport
	case SilenceReport, SilenceRespect, SilenceIgnore:
	default:
		return nil, fmt.Errorf("replay: unknown silences mode %q", mode)
	}
	if opts.FileRules == nil && opts.RuleName == "" {
		return nil, errors.New("replay: a rule name is required when replaying stored rules")
	}

	targets, err := src.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	stored, err := src.ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	baselines, err := src.ListAlertBaselines(ctx)
	if err != nil {
		return nil, err
	}
	silences, err := src.ListSilences(ctx)
	if err != nil {
		return nil, err
	}
	names, err := src.AgentNames(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := tree.New(targets)
	if err != nil {
		return nil, err
	}
	paths := map[int64]string{}
	byPath := map[string]int64{}
	for i := range targets {
		p, err := tr.Path(targets[i].ID)
		if err != nil {
			return nil, err
		}
		paths[targets[i].ID] = p
		byPath[p] = targets[i].ID
	}

	// The rule set inheritance is worked out over: what is stored, with the
	// file's rules laid over it the way an import would.
	all := slices.Clone(stored)
	selected := map[int64]bool{}
	if opts.FileRules == nil {
		for _, r := range stored {
			if r.Name == opts.RuleName {
				selected[r.ID] = true
			}
		}
	} else {
		at := map[struct {
			node int64
			name string
		}]int{}
		for i, r := range all {
			at[struct {
				node int64
				name string
			}{r.TargetID, r.Name}] = i
		}
		for i, r := range opts.FileRules {
			key := struct {
				node int64
				name string
			}{r.TargetID, r.Name}
			if j, ok := at[key]; ok {
				// Taking over the id keeps the captured baseline and the
				// rule-scoped silences, which is what an import would do.
				r.ID = all[j].ID
				all[j] = r
			} else {
				r.ID = -int64(i + 1) // never collides with a stored id
				at[key] = len(all)
				all = append(all, r)
			}
			if opts.RuleName == "" || r.Name == opts.RuleName {
				selected[r.ID] = true
			}
		}
	}
	if len(selected) == 0 {
		var have []string
		src := stored
		if opts.FileRules != nil {
			src = opts.FileRules
		}
		for _, r := range src {
			if !slices.Contains(have, r.Name) {
				have = append(have, r.Name)
			}
		}
		slices.Sort(have)
		return nil, fmt.Errorf("replay: no rule named %q; rules that exist: %s",
			opts.RuleName, strings.Join(have, ", "))
	}

	resolved, err := alert.Resolve(targets, all)
	if err != nil {
		return nil, err
	}
	ancestors := alert.Ancestors(targets)

	scope := int64(-1) // node id the replay is restricted to
	if opts.Target != "" {
		p := opts.Target
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if p != "/" {
			p = strings.TrimSuffix(p, "/")
		}
		id, ok := byPath[p]
		if !ok {
			return nil, fmt.Errorf("replay: no target %q", opts.Target)
		}
		scope = id
	}

	agentIDs := make([]int64, 0, len(names))
	if opts.Vantage != "" {
		found := false
		for id, n := range names {
			if n == opts.Vantage {
				agentIDs, found = append(agentIDs, id), true
			}
		}
		if !found {
			return nil, fmt.Errorf("replay: no agent named %q", opts.Vantage)
		}
	} else {
		for id := range names {
			agentIDs = append(agentIDs, id)
		}
	}
	slices.SortFunc(agentIDs, func(a, b int64) int { return strings.Compare(names[a], names[b]) })

	goldenOf := map[int64]alert.Baselined{}
	for _, b := range baselines {
		goldenOf[b.RuleID] = b
	}

	warnings := map[string]bool{}
	rep := &Report{
		Schema: 1, From: time.Unix(opts.From, 0).UTC(), To: time.Unix(opts.To, 0).UTC(),
		Silences: mode, Series: []SeriesReport{}, Notes: notes,
	}

	// Which selected rules reach which leaves, and a warning for each selected
	// rule that is not replayed anywhere or cannot do what it says.
	leaves := make([]int64, 0, len(resolved))
	reached := map[int64]bool{}
	for id, app := range resolved {
		if scope >= 0 && !ancestors[id][scope] {
			continue
		}
		for _, r := range app.Rules {
			if selected[r.ID] {
				reached[r.ID] = true
			}
		}
		leaves = append(leaves, id)
	}
	slices.SortFunc(leaves, func(a, b int64) int {
		return cmpStr(resolved[a].Path, resolved[b].Path, a, b)
	})
	for i := range all {
		r := &all[i]
		if !selected[r.ID] {
			continue
		}
		label := fmt.Sprintf("rule %q on %s", r.Name, paths[r.TargetID])
		if !reached[r.ID] {
			warnings[label+" reaches no enabled target in scope (masked by rules on a descendant, or nothing under it)"] = true
		}
		if r.Metric == alert.MetricShape && r.Baseline == alert.BaselineGolden {
			g, ok := goldenOf[r.ID]
			switch {
			case !ok:
				warnings[label+" is a golden-baseline rule with no captured reference, so it cannot fire"] = true
			case g.FromTS < opts.To && g.ToTS > opts.From:
				warnings[label+": the captured reference window overlaps the replayed window, so this stretch is compared with itself"] = true
			}
		}
	}

	var jobs []*seriesRun
	for _, leaf := range leaves {
		app := resolved[leaf]
		var rules []*alert.Rule
		for _, r := range app.Rules {
			if selected[r.ID] {
				rules = append(rules, r)
			}
		}
		if len(rules) == 0 {
			continue
		}
		if app.IntervalS <= 0 {
			return nil, fmt.Errorf("replay: %s has interval %d", app.Path, app.IntervalS)
		}
		for _, agent := range agentIDs {
			jobs = append(jobs, &seriesRun{
				src: src, opts: opts, mode: mode, silences: silences, golden: goldenOf,
				ancestors: ancestors[leaf], leaf: leaf, agent: agent, app: app, rules: rules,
			})
		}
	}

	// A few series at a time. A series is independent of every other, and a
	// shape rule spends its time computing, not reading: measured on two cores,
	// thirty days of fifty series took a minute one after another. The bound is
	// what keeps memory to a handful of chunks, and no more than a read-only
	// handle has connections for.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, min(runtime.GOMAXPROCS(0), maxParallel))
	var wg sync.WaitGroup
	// The first series to fail, noted before the rest are cancelled: whatever
	// the cancellation then makes of the others' reads, this is the cause.
	var failed *seriesRun
	var once sync.Once
	for _, sr := range jobs {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if sr.out, sr.err = sr.run(ctx); sr.err != nil {
				once.Do(func() { failed = sr })
				cancel() // the rest would be thrown away
			}
		})
	}
	wg.Wait()
	if failed != nil {
		return nil, fmt.Errorf("replay: %s from %s: %w", failed.app.Path, names[failed.agent], failed.err)
	}
	for _, sr := range jobs {
		for _, o := range sr.out {
			o.rep.RuleNode = paths[o.ruleNode]
			o.rep.Target, o.rep.TargetID = sr.app.Path, sr.leaf
			o.rep.Vantage, o.rep.AgentID = names[sr.agent], sr.agent
			rep.Series = append(rep.Series, o.rep)
			rep.Totals.Suppressed += o.suppressed
		}
	}

	slices.SortStableFunc(rep.Series, func(a, b SeriesReport) int {
		return cmp.Or(cmp.Compare(a.RuleNode, b.RuleNode), cmp.Compare(a.Rule, b.Rule),
			cmp.Compare(a.Target, b.Target), cmp.Compare(a.Vantage, b.Vantage))
	})
	for _, s := range rep.Series {
		rep.Totals.Series++
		if len(s.Episodes) > 0 {
			rep.Totals.SeriesFiring++
		}
		rep.Totals.Episodes += len(s.Episodes)
		for _, w := range s.Warnings {
			warnings[fmt.Sprintf("%s on %s from %s: %s", s.Rule, s.Target, s.Vantage, w)] = true
		}
	}
	rep.Totals.EpisodesPerDay = float64(rep.Totals.Episodes) / (float64(opts.To-opts.From) / 86400)
	rep.Warnings = append([]string{}, slices.Sorted(maps.Keys(warnings))...)
	return rep, nil
}

// seriesRun is the replay of the selected rules over one (target, agent).
type seriesRun struct {
	src       Source
	opts      Options
	mode      Mode
	silences  []alert.Silence
	golden    map[int64]alert.Baselined
	ancestors map[int64]bool
	leaf      int64
	agent     int64
	app       alert.Applicable
	rules     []*alert.Rule

	out []seriesOut
	err error
}

type seriesOut struct {
	rep        SeriesReport
	ruleNode   int64
	suppressed int
}

// run reads the series once, in chunks, and steps every rule through it. It
// returns nothing for a series with no rows in the window.
func (sr *seriesRun) run(ctx context.Context) ([]seriesOut, error) {
	iv := int64(sr.app.IntervalS)
	rrs := make([]*ruleRun, len(sr.rules))
	lo := sr.opts.From
	for i, r := range sr.rules {
		rr := &ruleRun{rule: *r, node: r.TargetID, golden: sr.golden[r.ID].Samples, wanted: r.WarmupIntervals()}
		// A disabled rule is replayed: testing it before it is turned on is the
		// point. Disabled targets are already out of Resolve.
		rr.rule.Enabled = true
		rr.warmStart = max(sr.opts.From-int64(rr.wanted)*iv, 0)
		rr.out = &SeriesReport{
			Rule: r.Name, Condition: r.Describe(), Episodes: []Episode{}, Warnings: []string{},
		}
		lo = min(lo, rr.warmStart)
		rrs[i] = rr
	}

	intervals, started := 0, false
	row := func(m *store.Measurement) {
		in := m.AlertInput()
		if m.TS < sr.opts.From {
			// Fills shape buffers exactly as live rows would; whatever the
			// rule decides here is before the window and is discarded.
			for _, rr := range rrs {
				if m.TS >= rr.warmStart {
					rr.ser.Step(&rr.rule, &in, rr.golden, &sr.app)
					rr.found++
				}
			}
			return
		}
		if !started {
			// The shape memory stays; only the hysteresis starts clean.
			started = true
			for _, rr := range rrs {
				rr.ser.State = alert.State{}
			}
		}
		intervals++
		for _, rr := range rrs {
			sr.step(rr, &in)
		}
	}
	if err := sr.walk(ctx, lo, sr.opts.To, iv, row); err != nil {
		return nil, err
	}
	if intervals == 0 {
		return nil, nil
	}

	out := make([]seriesOut, 0, len(rrs))
	for _, rr := range rrs {
		o := seriesOut{rep: *rr.out, ruleNode: rr.node}
		o.rep.Intervals = intervals
		if rr.wanted > 0 && rr.found < rr.wanted {
			o.rep.Warnings = append(o.rep.Warnings, fmt.Sprintf(
				"only %d of %d warm-up intervals found before the window; the rule is quiet until its baseline fills, as after a restart",
				rr.found, rr.wanted))
		}
		eps := o.rep.Episodes[:0]
		for _, e := range o.rep.Episodes {
			if e.Open {
				e.Seconds = sr.opts.To - e.Start.Unix()
			} else {
				e.Seconds = e.End.Unix() - e.Start.Unix()
			}
			if e.FireSilenced {
				o.suppressed++
				if sr.mode == SilenceRespect {
					continue
				}
			}
			eps = append(eps, e)
		}
		o.rep.Episodes = eps
		for _, e := range eps {
			o.rep.Fired++
			if !e.Open {
				o.rep.Resolved++
			}
			o.rep.FiringSeconds += e.Seconds
			o.rep.LongestSeconds = max(o.rep.LongestSeconds, e.Seconds)
		}
		out = append(out, o)
	}
	return out, nil
}

// step advances one rule by one in-window row and keeps its episodes.
func (sr *seriesRun) step(rr *ruleRun, in *alert.Input) {
	t := rr.ser.Step(&rr.rule, in, rr.golden, &sr.app)
	st := &rr.ser.State
	switch t {
	case alert.Fired:
		rr.out.Episodes = append(rr.out.Episodes, Episode{
			Start: time.Unix(in.TS, 0).UTC(), Open: true,
			CrossedAt: st.Value, Peak: st.Value,
			FireSilenced: sr.silencedAt(in.TS, rr.rule.ID),
		})
	case alert.Resolved:
		e := &rr.out.Episodes[len(rr.out.Episodes)-1]
		end := time.Unix(in.TS, 0).UTC()
		e.End, e.Open = &end, false
		e.ResolveSilenced = sr.silencedAt(in.TS, rr.rule.ID)
	default:
		if st.Firing {
			e := &rr.out.Episodes[len(rr.out.Episodes)-1]
			if rr.rule.Op == alert.OpLess {
				e.Peak = min(e.Peak, st.Value)
			} else {
				e.Peak = max(e.Peak, st.Value)
			}
		}
	}
}

// silencedAt asks whether a silence covers a transition at its own time, not
// the wall clock: the question is whether it would have been silenced then.
func (sr *seriesRun) silencedAt(ts, ruleID int64) bool {
	if sr.mode == SilenceIgnore {
		return false
	}
	for i := range sr.silences {
		if sr.silences[i].Covers(ts, ruleID, sr.agent, sr.ancestors) {
			return true
		}
	}
	return false
}

// walk feeds fn the rows of [lo, hi), oldest first, a chunk at a time. While
// nothing has turned up the chunk widens, so a (target, agent) that never
// measured anything is found out in a few reads rather than one per chunk.
func (sr *seriesRun) walk(ctx context.Context, lo, hi, iv int64, fn func(*store.Measurement)) error {
	step := chunkIntervals * iv
	span := step
	for from := lo; from < hi; {
		to := min(from+span, hi)
		n, err := sr.read(ctx, from, to, fn)
		if err != nil {
			return err
		}
		if n == 0 {
			span = min(span*2, 16*step)
		} else {
			span = step
		}
		from = to
	}
	return nil
}

// read reads one range, halving it if the store refuses it as too large.
func (sr *seriesRun) read(ctx context.Context, from, to int64, fn func(*store.Measurement)) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	ms, err := sr.src.QueryRange(ctx, sr.leaf, sr.agent, from, to)
	if errors.Is(err, store.ErrRangeTooLarge) && to-from > 1 {
		mid := from + (to-from)/2
		a, err := sr.read(ctx, from, mid, fn)
		if err != nil {
			return a, err
		}
		b, err := sr.read(ctx, mid, to, fn)
		return a + b, err
	}
	if err != nil {
		return 0, err
	}
	for i := range ms {
		fn(&ms[i])
	}
	return len(ms), nil
}

func cmpStr(a, b string, x, y int64) int {
	return cmp.Or(cmp.Compare(a, b), cmp.Compare(x, y))
}
