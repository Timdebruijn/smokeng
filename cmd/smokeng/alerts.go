package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert/replay"
	"github.com/timdebruijn/smokeng/internal/config"
	"github.com/timdebruijn/smokeng/internal/store"
)

func alertsCmd(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "replay":
		return alertsReplay(args[1:], time.Now(), os.Stdout, os.Stderr)
	default:
		return fmt.Errorf("unknown alerts subcommand %q", args[0])
	}
}

const replayUsage = `usage: smokeng alerts replay [--db path] (--rule NAME | --rules-file FILE [--rule NAME])
                             (--since 30d | --from RFC3339 --to RFC3339)
                             [--target PATH] [--vantage NAME]
                             [--silences respect|ignore|report] [--format text|json|csv]`

// replayArgs is what the flags of `alerts replay` came to, validated.
type replayArgs struct {
	db, rulesFile, format string
	opts                  replay.Options
}

// parseReplayArgs turns flags into replay options. now is passed in so that
// --since means a fixed window in a test.
func parseReplayArgs(args []string, now time.Time) (replayArgs, error) {
	var a replayArgs
	fs := flag.NewFlagSet("alerts replay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.db, "db", "smokeng.db", "path to the SQLite database")
	rule := fs.String("rule", "", "name of the rule to replay")
	fs.StringVar(&a.rulesFile, "rules-file", "", "replay the rules of this TOML file instead of the stored ones; nothing is imported")
	since := fs.String("since", "", "window ending now, as a number with d, h or m: 30d, 36h, 90m")
	from := fs.String("from", "", "window start, RFC 3339")
	to := fs.String("to", "", "window end, RFC 3339")
	target := fs.String("target", "", "only targets under this path, e.g. /Production/web")
	vantage := fs.String("vantage", "", "only this agent's measurements (local is the master's own prober)")
	silences := fs.String("silences", "report", "respect, ignore or report silences")
	fs.StringVar(&a.format, "format", "text", "text, json or csv")
	fail := func(format string, v ...any) (replayArgs, error) {
		return a, fmt.Errorf("%s\n%s", fmt.Sprintf(format, v...), replayUsage)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// The flag set prints nowhere, so that a mistyped flag gives one
			// error and not two; asking for help is the case that wants the list.
			var flags strings.Builder
			fs.SetOutput(&flags)
			fs.PrintDefaults()
			return a, fmt.Errorf("%s\n\n%s", replayUsage, flags.String())
		}
		return fail("%v", err)
	}
	if fs.NArg() != 0 {
		return fail("unexpected argument %q", fs.Arg(0))
	}
	if *rule == "" && a.rulesFile == "" {
		return fail("--rule is required unless --rules-file is given")
	}
	switch {
	case *since != "" && (*from != "" || *to != ""):
		return fail("give either --since or --from and --to, not both")
	case *since != "":
		d, err := parseSince(*since)
		if err != nil {
			return fail("%v", err)
		}
		a.opts.From, a.opts.To = now.Add(-d).Unix(), now.Unix()
	case *from != "" && *to != "":
		f, err1 := time.Parse(time.RFC3339, *from)
		t, err2 := time.Parse(time.RFC3339, *to)
		if err := errors.Join(err1, err2); err != nil {
			return fail("--from and --to are RFC 3339 times, like 2026-11-01T00:00:00Z: %v", err)
		}
		if !t.After(f) {
			return fail("--to must be after --from")
		}
		a.opts.From, a.opts.To = f.Unix(), t.Unix()
	default:
		return fail("give a window: --since, or both --from and --to")
	}
	switch replay.Mode(*silences) {
	case replay.SilenceReport, replay.SilenceRespect, replay.SilenceIgnore:
	default:
		return fail("--silences is respect, ignore or report, not %q", *silences)
	}
	switch a.format {
	case "text", "json", "csv":
	default:
		return fail("--format is text, json or csv, not %q", a.format)
	}
	a.opts.RuleName, a.opts.Target, a.opts.Vantage = *rule, *target, *vantage
	a.opts.Silences = replay.Mode(*silences)
	return a, nil
}

// parseSince reads a duration of days, hours or minutes. time.ParseDuration has
// no days, and a replay window is more often weeks than minutes.
func parseSince(s string) (time.Duration, error) {
	unit := map[byte]time.Duration{'d': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute}[s[len(s)-1]]
	n, err := strconv.Atoi(s[:len(s)-1])
	// The upper bound is where the multiplication below would wrap: unchecked,
	// --since 213504d came out as a window of 25 minutes.
	if unit == 0 || err != nil || n <= 0 || time.Duration(n) > math.MaxInt64/unit {
		return 0, fmt.Errorf("--since is a positive number with d, h or m (30d, 36h, 90m), not %q", s)
	}
	return time.Duration(n) * unit, nil
}

func alertsReplay(args []string, now time.Time, stdout, stderr io.Writer) error {
	a, err := parseReplayArgs(args, now)
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
	if a.rulesFile != "" {
		data, err := os.ReadFile(a.rulesFile)
		if err != nil {
			return err
		}
		f, err := config.Parse(data)
		if err != nil {
			return err
		}
		targets, err := st.ListTargets(ctx)
		if err != nil {
			return err
		}
		if a.opts.FileRules, err = config.AlertRules(f, targets); err != nil {
			return err
		}
		// Without this an empty file would fall back to the stored rules, and
		// the output would claim to be about the file.
		if len(a.opts.FileRules) == 0 {
			return fmt.Errorf("%s defines no alert rules", a.rulesFile)
		}
	}
	rep, err := replay.Run(ctx, st, a.opts)
	if err != nil {
		return err
	}
	// To stderr in every format. csv has nowhere to put a warning, and without
	// this a golden rule with no reference gave a header and no rows, which
	// reads as a rule that never fires rather than one that cannot.
	for _, w := range rep.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	switch a.format {
	case "json":
		return writeReplayJSON(stdout, rep)
	case "csv":
		return writeReplayCSV(stdout, rep)
	}
	return writeReplayText(stdout, rep)
}

func writeReplayJSON(w io.Writer, rep *replay.Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

var replayCSVHeader = []string{"rule", "rule_node", "target", "vantage", "start", "end", "seconds",
	"crossed_at", "peak", "open", "fire_silenced", "resolve_silenced"}

func writeReplayCSV(w io.Writer, rep *replay.Report) error {
	cw := csv.NewWriter(w)
	cw.Write(replayCSVHeader)
	num := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	for _, s := range rep.Series {
		for _, e := range s.Episodes {
			end := ""
			if e.End != nil {
				end = e.End.Format(time.RFC3339)
			}
			cw.Write([]string{s.Rule, s.RuleNode, s.Target, s.Vantage,
				e.Start.Format(time.RFC3339), end, strconv.FormatInt(e.Seconds, 10),
				num(e.CrossedAt), num(e.Peak), strconv.FormatBool(e.Open),
				strconv.FormatBool(e.FireSilenced), strconv.FormatBool(e.ResolveSilenced)})
		}
	}
	cw.Flush()
	return cw.Error()
}

// quietListed is how many series with no episodes are named before the rest are
// only counted: a replay over a whole tree is mostly silence, and the point is
// to see what fired.
const quietListed = 3

func writeReplayText(out io.Writer, rep *replay.Report) error {
	// Buffered so that one failed write, a closed pipe or a full disk, is
	// remembered and returned by Flush rather than checked on every line.
	w := bufio.NewWriter(out)
	day := "2006-01-02 15:04Z"
	fmt.Fprintf(w, "Replay %s to %s (silences: %s)\n", rep.From.Format(day), rep.To.Format(day), rep.Silences)
	for i := 0; i < len(rep.Series); {
		// One block per rule: the series come sorted by rule node and name.
		j := i
		for j < len(rep.Series) && rep.Series[j].Rule == rep.Series[i].Rule && rep.Series[j].RuleNode == rep.Series[i].RuleNode {
			j++
		}
		fmt.Fprintf(w, "\nrule %s on %s: %s\n", rep.Series[i].Rule, rep.Series[i].RuleNode, rep.Series[i].Condition)
		quiet := 0
		for _, s := range rep.Series[i:j] {
			if len(s.Episodes) == 0 {
				if quiet++; quiet <= quietListed {
					fmt.Fprintf(w, "  %s from %s: no episodes (%d intervals)\n", s.Target, s.Vantage, s.Intervals)
				}
				continue
			}
			fmt.Fprintf(w, "  %s from %s: %d episodes, firing %s, longest %s (%d intervals)\n",
				s.Target, s.Vantage, len(s.Episodes), dur(s.FiringSeconds), dur(s.LongestSeconds), s.Intervals)
			for _, e := range s.Episodes {
				end := "still firing"
				if e.End != nil {
					end = e.End.Format(day)
				}
				mark := ""
				switch {
				case e.FireSilenced && e.ResolveSilenced:
					mark = "  [silenced]"
				case e.FireSilenced:
					mark = "  [fire silenced]"
				case e.ResolveSilenced:
					mark = "  [resolve silenced]"
				}
				fmt.Fprintf(w, "    %s -> %s  %s  crossed at %g, peak %g%s\n",
					e.Start.Format(day), end, dur(e.Seconds), e.CrossedAt, e.Peak, mark)
			}
		}
		// Listed only up to quietListed, so say what was left out.
		if quiet > quietListed {
			fmt.Fprintf(w, "  %d more series with no episodes\n", quiet-quietListed)
		}
		i = j
	}
	t := rep.Totals
	// Under report those episodes are in the list above, so calling them
	// suppressed would say they are missing from it.
	silenced := fmt.Sprintf("%d fired under a silence", t.Suppressed)
	switch rep.Silences {
	case replay.SilenceRespect:
		silenced = fmt.Sprintf("%d more left out, suppressed by silences", t.Suppressed)
	case replay.SilenceIgnore:
		silenced = "silences not consulted"
	}
	fmt.Fprintf(w, "\n%d series evaluated, %d with episodes: %d episodes (%.2f per day), %s\n",
		t.Series, t.SeriesFiring, t.Episodes, t.EpisodesPerDay, silenced)
	if len(rep.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, x := range rep.Warnings {
			fmt.Fprintln(w, "  -", x)
		}
	}
	fmt.Fprintln(w, "\nNotes:")
	for _, x := range rep.Notes {
		fmt.Fprintln(w, "  -", x)
	}
	return w.Flush()
}

// dur renders seconds the way an operator reads a firing time.
func dur(s int64) string {
	switch {
	case s >= 86400:
		return fmt.Sprintf("%dd%dh", s/86400, s%86400/3600)
	case s >= 3600:
		return fmt.Sprintf("%dh%dm", s/3600, s%3600/60)
	case s >= 60:
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%ds", s)
}
