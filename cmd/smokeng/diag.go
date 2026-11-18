package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/timdebruijn/smokeng/internal/losspattern"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

func diagCmd(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "loss-pattern":
		return diagLossPattern(args[1:], time.Now(), os.Stdout)
	default:
		return fmt.Errorf("unknown diag subcommand %q", args[0])
	}
}

const lossPatternUsage = `usage: smokeng diag loss-pattern [--db path] --target PATH [--vantage NAME]
                                 (--since 24h | --from RFC3339 --to RFC3339)
                                 [--family v4|v6|unknown] [--format text|json]`

type lossPatternArgs struct {
	db, target, vantage, format string
	from, to                    int64
	// family is the series to read; named is false when --family was not given
	// and the target's current family is read.
	family tree.Family
	named  bool
}

// parseLossPatternArgs validates the flags. now is passed in so that --since
// means a fixed window in a test.
func parseLossPatternArgs(args []string, now time.Time) (lossPatternArgs, error) {
	var a lossPatternArgs
	fs := flag.NewFlagSet("diag loss-pattern", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.db, "db", "smokeng.db", "path to the SQLite database")
	fs.StringVar(&a.target, "target", "", "the target to look at, a tree path such as /Production/web (required)")
	fs.StringVar(&a.vantage, "vantage", "", "only this agent's measurements (local is the master's own prober); default is every agent with data")
	since := fs.String("since", "", "window ending now, as a number with d, h or m: 24h, 7d, 90m")
	from := fs.String("from", "", "window start, RFC 3339")
	to := fs.String("to", "", "window end, RFC 3339")
	fs.StringVar(&a.format, "format", "text", "text or json")
	family := fs.String("family", "", "the address family to read: v4, v6 or unknown (history whose family could not be established); default is the target's current family")
	fail := func(format string, v ...any) (lossPatternArgs, error) {
		return a, fmt.Errorf("%s\n%s", fmt.Sprintf(format, v...), lossPatternUsage)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// As in alerts replay: the flag set prints nowhere, so help is
			// assembled here.
			var flags strings.Builder
			fs.SetOutput(&flags)
			fs.PrintDefaults()
			return a, fmt.Errorf("%s\n\n%s", lossPatternUsage, flags.String())
		}
		return fail("%v", err)
	}
	if fs.NArg() != 0 {
		return fail("unexpected argument %q", fs.Arg(0))
	}
	if a.target == "" {
		return fail("--target is required")
	}
	var err error
	if a.from, a.to, err = parseWindow(*since, *from, *to, now); err != nil {
		return fail("%v", err)
	}
	// The window is walked a day at a time whether or not it holds anything,
	// so one given in centuries by mistake would take that many reads to say so.
	if a.to-a.from > maxLossPatternDays*86400 {
		return fail("the window is at most %d days", maxLossPatternDays)
	}
	if a.format != "text" && a.format != "json" {
		return fail("--format is text or json, not %q", a.format)
	}
	// Whether the flag was given, not whether it is empty: --family "$F" with F
	// unset must be refused, as the API refuses family=, not read as the default.
	fs.Visit(func(f *flag.Flag) { a.named = a.named || f.Name == "family" })
	if a.named {
		var ok bool
		if a.family, ok = tree.ParseFamily(*family); !ok {
			return fail("--family is v4, v6 or unknown, not %q", *family)
		}
	}
	return a, nil
}

// The JSON form. Its field names are read by scripts; renaming or dropping one
// is a bump of Schema and of the pinned test. A field added, as family was, is
// pinned in the test too but leaves Schema alone: a script that ignores what it
// does not know still reads it.
type lossPatternReport struct {
	Schema int                 `json:"schema"`
	Target string              `json:"target"`
	Family string              `json:"family"`
	From   time.Time           `json:"from"`
	To     time.Time           `json:"to"`
	Series []lossPatternSeries `json:"series"`
	// other is set when Family is not the target's current one, which the text
	// form then says; the JSON always carries Family.
	other bool
}

type lossPatternSeries struct {
	Vantage    string              `json:"vantage"`
	AgentID    int64               `json:"agent_id"`
	Intervals  int                 `json:"intervals"`
	WithLoss   int                 `json:"with_loss"`
	Recorded   int                 `json:"recorded"`
	Verdict    lossPatternVerdict  `json:"verdict"`
	Recent     []lossPatternRecent `json:"recent"`
	Conclusion string              `json:"conclusion"`
	// history is the newest recorded intervals, as many as the detector can
	// use; everything older has been counted and let go.
	history []losspattern.Interval
}

type lossPatternVerdict struct {
	Tested          bool       `json:"tested"`
	Why             string     `json:"why"`
	Flagged         bool       `json:"flagged"`
	P               *float64   `json:"p"` // null when the window was not tested
	PAtLeast        bool       `json:"p_at_least"`
	Probes          int        `json:"probes"`
	Mode            string     `json:"mode"`
	WindowIntervals int        `json:"window_intervals"`
	Lost            int        `json:"lost"`
	Counts          []int      `json:"counts"`
	WindowFrom      *time.Time `json:"window_from"` // null when no interval was judged
	WindowTo        *time.Time `json:"window_to"`
	Where           string     `json:"where"`
}

type lossPatternRecent struct {
	TS      time.Time `json:"ts"`
	Pattern string    `json:"pattern"`
}

const (
	recentShown        = 20
	maxLossPatternDays = 400
)

func diagLossPattern(args []string, now time.Time, stdout io.Writer) error {
	a, err := parseLossPatternArgs(args, now)
	if err != nil {
		return err
	}
	// Read-only, so this is safe next to a running server, and refuses a
	// database from another version rather than upgrading it.
	st, err := store.OpenReadOnly(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	targets, err := st.ListTargets(ctx)
	if err != nil {
		return err
	}
	tr, err := tree.New(targets)
	if err != nil {
		return err
	}
	p := a.target
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p != "/" {
		p = strings.TrimSuffix(p, "/")
	}
	var leaf *tree.Target
	known := false
	for i := range targets {
		if tp, err := tr.Path(targets[i].ID); err == nil && tp == p {
			known, leaf = true, &targets[i]
			break
		}
	}
	switch {
	case !known:
		return fmt.Errorf("no target %q\n%s", a.target, lossPatternUsage)
	case leaf.Host == nil:
		return fmt.Errorf("%s is a group, not a target with a host; name one of the targets under it\n%s", p, lossPatternUsage)
	}

	names, err := st.AgentNames(ctx)
	if err != nil {
		return err
	}
	var agents []int64
	for id, n := range names {
		if a.vantage == "" || n == a.vantage {
			agents = append(agents, id)
		}
	}
	if a.vantage != "" && len(agents) == 0 {
		return fmt.Errorf("no agent named %q\n%s", a.vantage, lossPatternUsage)
	}
	slices.SortFunc(agents, func(x, y int64) int { return strings.Compare(names[x], names[y]) })

	family := leaf.Family()
	if a.named {
		family = a.family
	}
	rep := lossPatternReport{Schema: 1, Target: p, Family: family.String(), other: family != leaf.Family(),
		From: time.Unix(a.from, 0).UTC(), To: time.Unix(a.to, 0).UTC(), Series: []lossPatternSeries{}}
	for _, id := range agents {
		acc := lossPatternSeries{Vantage: names[id], AgentID: id}
		if err := eachRow(ctx, st, leaf.ID, id, family, a.from, a.to, acc.add); err != nil {
			return err
		}
		if acc.Intervals == 0 {
			continue
		}
		acc.judge()
		rep.Series = append(rep.Series, acc)
	}

	if a.format == "json" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", b)
		return err
	}
	return writeLossPatternText(stdout, rep)
}

// eachRow hands fn the series' rows oldest first, a day at a time, so that a
// long window is neither one read that crosses the store's limit nor a window
// held in memory; a day that still crosses the limit is halved.
func eachRow(ctx context.Context, st *store.SQLite, target, agent int64, family tree.Family, from, to int64, fn func(*store.Measurement)) error {
	var read func(from, to int64) error
	read = func(from, to int64) error {
		ms, err := st.QueryRange(ctx, target, agent, family, from, to)
		if errors.Is(err, store.ErrRangeTooLarge) && to-from > 1 {
			mid := from + (to-from)/2
			if err := read(from, mid); err != nil {
				return err
			}
			return read(mid, to)
		}
		for i := range ms {
			fn(&ms[i])
		}
		return err
	}
	for d := from; d < to; d += 86400 {
		if err := read(d, min(d+86400, to)); err != nil {
			return err
		}
	}
	return nil
}

// add counts one interval and, if it recorded which probes it lost, keeps it
// for the verdict.
func (s *lossPatternSeries) add(m *store.Measurement) {
	s.Intervals++
	if m.Received >= m.Sent {
		return
	}
	s.WithLoss++
	// A mask that does not decode is one that was not recorded: the counts are
	// the row's own, and nothing here may fail on one bad row. One on an
	// interval that may not carry a mask is treated the same, so that every
	// interval kept is one the detector counts.
	if m.LossMask == nil || !store.LossMaskRecordable(m.Flags, m.Sent, m.Received) {
		return
	}
	spread, lost, err := store.DecodeLossMask(m.LossMask, m.Sent, m.Received)
	if err != nil {
		return
	}
	s.Recorded++
	// The detector looks at no more than its window, so nothing older is kept.
	if len(s.history) == losspattern.Window {
		s.history = append(s.history[:0], s.history[1:]...)
	}
	s.history = append(s.history, losspattern.Interval{TS: m.TS, Spread: spread, Lost: lost})
}

// judge runs the detector over what add kept and words the result.
func (s *lossPatternSeries) judge() {
	v := losspattern.Test(s.history, true)
	s.Verdict = lossPatternVerdict{
		Tested: v.Tested, Why: v.Why, Flagged: v.Flagged, PAtLeast: v.AtLeast,
		Probes: v.Probes, Mode: "burst", WindowIntervals: v.Intervals, Lost: v.Lost,
		Counts: v.Counts,
	}
	if v.Tested {
		s.Verdict.P = &v.P
	}
	// Named only for a flagged window: on any other, the positions that lost
	// most are not a finding.
	if v.Flagged {
		s.Verdict.Where = v.Where()
	}
	if v.Spread {
		s.Verdict.Mode = "spread"
	}
	if s.Verdict.Counts == nil {
		s.Verdict.Counts = []int{}
	}
	if v.Intervals > 0 {
		f, t := time.Unix(v.From, 0).UTC(), time.Unix(v.To, 0).UTC()
		s.Verdict.WindowFrom, s.Verdict.WindowTo = &f, &t
	}
	s.Recent = []lossPatternRecent{}
	for _, iv := range s.history[max(0, len(s.history)-recentShown):] {
		var b strings.Builder
		for _, l := range iv.Lost {
			b.WriteByte(map[bool]byte{false: '.', true: 'x'}[l])
		}
		s.Recent = append(s.Recent, lossPatternRecent{TS: time.Unix(iv.TS, 0).UTC(), Pattern: b.String()})
	}
	s.Conclusion = lossPatternConclusion(s.Verdict)
}

// pText words the p-value of a tested window.
func pText(v lossPatternVerdict) string {
	if v.PAtLeast {
		return fmt.Sprintf("p ≥ %.4g", *v.P)
	}
	return fmt.Sprintf("p = %.4g", *v.P)
}

// lossPatternConclusion says no more than the detector does: a flag means the
// loss does not fit chance, which rate limiting explains but does not prove,
// and no flag means only that chance was not ruled out.
func lossPatternConclusion(v lossPatternVerdict) string {
	const fits = "That does not fit loss that falls where it happens to; it is consistent with rate limiting by the target, and is not proof of it."
	switch {
	case !v.Tested:
		return "Not enough to judge: " + v.Why + "."
	case !v.Flagged:
		return "No positional pattern found (" + pText(v) + "): the loss is not lopsided enough to rule chance out."
	case v.Where != "":
		return "Loss is concentrated at probes " + v.Where + ". " + fits
	}
	return "The loss leans to one end of the send order. " + fits
}

const (
	histRows     = 40 // most rows the histogram has
	barWidth     = 40
	patternShown = 80 // most positions of a pattern the text form prints
)

func writeLossPatternText(out io.Writer, rep lossPatternReport) error {
	// Buffered so that one failed write is remembered and returned by Flush.
	w := bufio.NewWriter(out)
	day := "2006-01-02 15:04Z"
	fam := ""
	if rep.other {
		fam = " (" + rep.Family + " history, not the target's current family)"
	}
	if len(rep.Series) == 0 {
		fmt.Fprintf(w, "%s%s: no measurements from %s to %s.\n", rep.Target, fam, rep.From.Format(day), rep.To.Format(day))
		return w.Flush()
	}
	for i, s := range rep.Series {
		if i > 0 {
			fmt.Fprintln(w)
		}
		v := s.Verdict
		fmt.Fprintf(w, "%s%s from %s, %s to %s\n\n", rep.Target, fam, s.Vantage, rep.From.Format(day), rep.To.Format(day))
		fmt.Fprintf(w, "%d intervals, %d with loss, %d of those with recorded positions\n", s.Intervals, s.WithLoss, s.Recorded)
		if n := s.WithLoss - s.Recorded; n > 0 {
			fmt.Fprintf(w, "%d had loss without recorded positions: measured before positions were recorded, total loss, an irtt probe, or an interval flagged as partly smokeng's own doing\n", n)
		}
		if v.WindowIntervals > 0 {
			fmt.Fprintf(w, "\nJudged: the last %d intervals with recorded loss, from %s to %s, %d probes per interval, %s\n\n",
				v.WindowIntervals, v.WindowFrom.Format(day), v.WindowTo.Format(day), v.Probes, v.Mode)
			writeHistogram(w, v.Counts)
			if v.Tested {
				fmt.Fprintf(w, "\n%s\n", pText(v))
			}
		}
		if len(s.Recent) > 0 {
			fmt.Fprintf(w, "\nLast %d intervals with recorded loss, newest last (. answered, x lost, in send order):\n", len(s.Recent))
			for _, r := range s.Recent {
				p := r.Pattern
				if len(p) > patternShown {
					p = p[:patternShown] + "…"
				}
				fmt.Fprintf(w, "  %s  %s\n", r.TS.Format(day), p)
			}
		}
		fmt.Fprintf(w, "\n%s\n", s.Conclusion)
	}
	return w.Flush()
}

// writeHistogram draws losses per send position, numbered from 1. Past histRows
// probes neighbouring positions share a row.
func writeHistogram(w io.Writer, counts []int) {
	per := (len(counts) + histRows - 1) / histRows
	type row struct {
		label string
		n     int
	}
	var rows []row
	top := 0
	for i := 0; i < len(counts); i += per {
		j := min(i+per, len(counts))
		n := 0
		for _, c := range counts[i:j] {
			n += c
		}
		label := fmt.Sprint(i + 1)
		if j-i > 1 {
			label = fmt.Sprintf("%d–%d", i+1, j)
		}
		rows = append(rows, row{label, n})
		top = max(top, n)
	}
	width := 0
	for _, r := range rows {
		width = max(width, len([]rune(r.label)))
	}
	if per > 1 {
		fmt.Fprintf(w, "Losses per send position, %d positions to a row:\n", per)
	} else {
		fmt.Fprintln(w, "Losses per send position:")
	}
	for _, r := range rows {
		bar := 0
		if top > 0 && r.n > 0 {
			bar = max(1, r.n*barWidth/top)
		}
		fmt.Fprintf(w, "  %*s  %-*s  %d\n", width, r.label, barWidth, strings.Repeat("█", bar), r.n)
	}
}
