package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/json"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/losspattern"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// lossRow is one interval of n probes that lost the given positions (0-based).
// With mask false the row has the counts and no positions, as one measured
// before positions were recorded does.
func lossRow(ts int64, n int, lost []int, mask bool) store.Measurement {
	l := make([]bool, n)
	for _, i := range lost {
		l[i] = true
	}
	recv := n - len(lost)
	m := store.Measurement{TS: ts, Sent: n, Received: recv, Samples: make([]uint32, recv)}
	for i := range m.Samples {
		m.Samples[i] = 5000
	}
	if mask && len(lost) > 0 {
		m.LossMask = store.EncodeLossMask(false, l)
	}
	return m
}

// diagDB writes a tree (/Production/web, a host; /Production, a group) and the
// given rows for web, per agent name, and returns the database path. Agents
// other than local are enrolled first.
func diagDB(t *testing.T, series map[string][]store.Measurement) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "smokeng.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	group := tree.Target{ParentID: new(int64(1)), Name: "Production", Enabled: true}
	if err := st.UpsertTarget(ctx, &group); err != nil {
		t.Fatal(err)
	}
	leaf := tree.Target{ParentID: &group.ID, Name: "web", Enabled: true, Host: new("10.0.0.1"), AddressFamily: new("v4")}
	if err := st.UpsertTarget(ctx, &leaf); err != nil {
		t.Fatal(err)
	}
	for name, rows := range series {
		var id int64
		if name != "local" {
			pub, _, err := ed25519.GenerateKey(cryptorand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			rec, err := st.AddAgent(ctx, name, pub)
			if err != nil {
				t.Fatal(err)
			}
			id = rec.ID
		}
		for i := range rows {
			rows[i].TargetID, rows[i].AgentID = leaf.ID, id
		}
		if err := st.WriteMeasurements(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// tailSeries is count intervals a minute apart, ending just before replayNow,
// each losing probes 16-20 of 20.
func tailSeries(count int) []store.Measurement {
	var rows []store.Measurement
	for i := 0; i < count; i++ {
		rows = append(rows, lossRow(replayNow.Unix()-int64(count-i)*60, 20, []int{15, 16, 17, 18, 19}, true))
	}
	return rows
}

func runLossPattern(t *testing.T, db string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := diagLossPattern(append([]string{"--db", db, "--since", "24h"}, args...), replayNow, &out)
	return out.String(), err
}

// Protects the headline result: loss that always sits at probes 16-20 of 20 is
// flagged, named by position, and shown in the histogram and the pattern list,
// in both formats. This is the whole point of the command.
func TestLossPatternFlagsTailLoss(t *testing.T) {
	db := diagDB(t, map[string][]store.Measurement{"local": tailSeries(30)})
	out, err := runLossPattern(t, db, "--target", "Production/web")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/Production/web from local", "30 intervals, 30 with loss, 30 of those with recorded positions",
		"the last 30 intervals with recorded loss", "20 probes per interval, burst",
		"2026-11-17 11:59Z  ...............xxxxx",
		"Loss is concentrated at probes 16–20. That does not fit loss that falls where it happens to; it is consistent with rate limiting by the target, and is not proof of it.",
		"p = ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`(?m)^  16 +█+ +30$`).MatchString(out) {
		t.Errorf("no histogram row with a bar for position 16:\n%s", out)
	}
	if regexp.MustCompile(`(?m)^  15 +█`).MatchString(out) {
		t.Errorf("position 15 lost nothing but has a bar:\n%s", out)
	}

	out, err = runLossPattern(t, db, "--target", "/Production/web/", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Series []struct {
			Verdict struct {
				Flagged bool
				Where   string
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Series) != 1 || !rep.Series[0].Verdict.Flagged || rep.Series[0].Verdict.Where != "16–20" {
		t.Errorf("json: %v %+v\n%s", err, rep, out)
	}
}

// Protects against crying wolf: network-like loss (two random probes lost per
// interval, fixed seed) is tested and not flagged, and the conclusion says so.
func TestLossPatternScatteredLossIsNotFlagged(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	var rows []store.Measurement
	for i := 0; i < 40; i++ {
		a := rng.IntN(20)
		b := (a + 1 + rng.IntN(19)) % 20
		rows = append(rows, lossRow(replayNow.Unix()-int64(40-i)*60, 20, []int{a, b}, true))
	}
	out, err := runLossPattern(t, diagDB(t, map[string][]store.Measurement{"local": rows}), "--target", "/Production/web")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No positional pattern found (p = ") || !strings.Contains(out, "not lopsided enough to rule chance out") || !strings.Contains(out, "\np = ") {
		t.Errorf("want a tested, unflagged verdict:\n%s", out)
	}
	if strings.Contains(out, "concentrated") || strings.Contains(out, "leans") {
		t.Errorf("flagged scattered loss:\n%s", out)
	}
}

// Protects the refusal to judge on too little: five lossy intervals give the
// reason and no p-value, not a verdict either way.
func TestLossPatternTooLittleToJudge(t *testing.T) {
	out, err := runLossPattern(t, diagDB(t, map[string][]store.Measurement{"local": tailSeries(5)}), "--target", "/Production/web")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Not enough to judge: 5 intervals with lopsided loss in the window, fewer than 12.") {
		t.Errorf("no refusal:\n%s", out)
	}
	if strings.Contains(out, "p = ") || strings.Contains(out, "p ≥ ") || strings.Contains(out, "concentrated") {
		t.Errorf("a p-value or verdict on five intervals:\n%s", out)
	}
}

// Protects the honesty about old data: lossy rows with no positions are counted
// and explained, and are not an error, and do not count as recorded.
func TestLossPatternLossWithoutPositions(t *testing.T) {
	var rows []store.Measurement
	for i := 0; i < 12; i++ {
		rows = append(rows, lossRow(replayNow.Unix()-int64(12-i)*60, 20, []int{3}, false))
	}
	out, err := runLossPattern(t, diagDB(t, map[string][]store.Measurement{"local": rows}), "--target", "/Production/web")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"12 intervals, 12 with loss, 0 of those with recorded positions",
		"12 had loss without recorded positions: measured before positions were recorded, total loss, an irtt probe",
		"Not enough to judge: no interval recorded which probes were lost."} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Judged:") || strings.Contains(out, "Losses per send position") {
		t.Errorf("drew a histogram from nothing:\n%s", out)
	}
}

// Protects the empty case: no measurements in the window is said plainly and is
// a success, in json an empty series list.
func TestLossPatternNoMeasurements(t *testing.T) {
	db := diagDB(t, map[string][]store.Measurement{"local": {lossRow(replayNow.Unix()-10*86400, 20, []int{19}, true)}})
	out, err := runLossPattern(t, db, "--target", "/Production/web")
	if err != nil || !strings.Contains(out, "no measurements") {
		t.Errorf("err %v, out %q", err, out)
	}
	out, err = runLossPattern(t, db, "--target", "/Production/web", "--format", "json")
	if err != nil || !strings.Contains(out, `"series": []`) {
		t.Errorf("err %v, out %q", err, out)
	}
}

// Protects the vantage handling: without --vantage every agent with data gets a
// section, in name order whatever the order they were enrolled in; with it, one.
func TestLossPatternSectionsPerAgent(t *testing.T) {
	db := diagDB(t, map[string][]store.Measurement{"local": tailSeries(12), "ams": tailSeries(13), "zzz-unused": nil})
	out, err := runLossPattern(t, db, "--target", "/Production/web")
	if err != nil {
		t.Fatal(err)
	}
	ams, local := strings.Index(out, "web from ams"), strings.Index(out, "web from local")
	if ams < 0 || local < 0 || ams > local || strings.Contains(out, "zzz-unused") {
		t.Errorf("sections out of order or an empty agent listed:\n%s", out)
	}
	out, err = runLossPattern(t, db, "--target", "/Production/web", "--vantage", "local")
	if err != nil || strings.Contains(out, "from ams") || !strings.Contains(out, "from local") {
		t.Errorf("--vantage local: %v\n%s", err, out)
	}
}

// Protects the daily reads: a window of three days is read in three pieces, and
// intervals either side of a piece boundary must all be seen.
func TestLossPatternReadsAcrossDays(t *testing.T) {
	boundary := replayNow.Unix() - 86400 // from + 2 days for --since 72h
	var rows []store.Measurement
	for i := -12; i < 12; i++ {
		rows = append(rows, lossRow(boundary+int64(i)*60, 20, []int{15, 16, 17, 18, 19}, true))
	}
	var out bytes.Buffer
	if err := diagLossPattern([]string{"--db", diagDB(t, map[string][]store.Measurement{"local": rows}), "--since", "72h", "--target", "/Production/web"}, replayNow, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "24 intervals, 24 with loss") {
		t.Errorf("lost intervals across a day boundary:\n%s", out.String())
	}
}

// Protects the flag contract: every mistake is an error that carries the usage
// line, a group or unknown path says which, and -h lists the flags.
func TestLossPatternFlagValidation(t *testing.T) {
	db := diagDB(t, map[string][]store.Measurement{"local": tailSeries(12)})
	for _, tc := range []struct{ name, args, err string }{
		{"no target", "--since 1d", "--target is required"},
		{"group", "--target /Production --since 1d", "is a group"},
		{"root", "--target / --since 1d", "is a group"},
		{"unknown", "--target /Nope --since 1d", `no target "/Nope"`},
		{"unknown vantage", "--target /Production/web --since 1d --vantage nobody", `no agent named "nobody"`},
		{"both windows", "--target /Production/web --since 1d --from 2026-11-01T00:00:00Z --to 2026-11-02T00:00:00Z", "not both"},
		{"no window", "--target /Production/web", "give a window"},
		{"bad format", "--target /Production/web --since 1d --format csv", "--format is text or json"},
		{"unknown flag", "--target /Production/web --since 1d --bogus", "bogus"},
		{"positional", "--target /Production/web --since 1d stray", "stray"},
	} {
		var out bytes.Buffer
		err := diagLossPattern(append([]string{"--db", db}, strings.Fields(tc.args)...), replayNow, &out)
		if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "usage: smokeng diag loss-pattern") {
			t.Errorf("%s: error %v should contain %q and a usage line", tc.name, err, tc.err)
		}
	}
	_, err := parseLossPatternArgs([]string{"-h"}, replayNow)
	if err == nil {
		t.Fatal("-h returned no text")
	}
	for _, want := range []string{"usage: smokeng diag loss-pattern", "-target", "-vantage", "-format", "window ending now"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("help lacks %q:\n%v", want, err)
		}
	}
}

// Protects the JSON schema, as for the alerts replay: scripts read it, so a
// field is not renamed, dropped or added by accident.
func TestLossPatternJSONSchemaIsPinned(t *testing.T) {
	out, err := runLossPattern(t, diagDB(t, map[string][]store.Measurement{"local": tailSeries(30)}), "--target", "/Production/web", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(out), &top); err != nil {
		t.Fatal(err)
	}
	check := func(what string, m map[string]any, want string) {
		t.Helper()
		var k []string
		for x := range m {
			k = append(k, x)
		}
		slices.Sort(k)
		if got := strings.Join(k, " "); got != want {
			t.Errorf("%s keys = %s\nwant       %s", what, got, want)
		}
	}
	check("top level", top, "from schema series target to")
	if top["schema"] != float64(1) || top["target"] != "/Production/web" || top["to"] != "2026-11-17T12:00:00Z" {
		t.Errorf("schema/target/to = %v %v %v", top["schema"], top["target"], top["to"])
	}
	s := top["series"].([]any)[0].(map[string]any)
	check("series", s, "agent_id conclusion intervals recent recorded vantage verdict with_loss")
	v := s["verdict"].(map[string]any)
	check("verdict", v, "counts flagged lost mode p p_at_least probes tested where why window_from window_intervals window_to")
	if v["mode"] != "burst" || v["window_from"] != "2026-11-17T11:30:00Z" || len(v["counts"].([]any)) != 20 {
		t.Errorf("verdict values: %v", v)
	}
	r := s["recent"].([]any)
	check("recent", r[0].(map[string]any), "pattern ts")
	if len(r) != 20 || r[19].(map[string]any)["ts"] != "2026-11-17T11:59:00Z" {
		t.Errorf("recent: %d entries, last %v", len(r), r[len(r)-1])
	}
}

// Protects the exit status: a report that could not be written is a failure.
func TestLossPatternReportsAFailedWrite(t *testing.T) {
	rep := lossPatternReport{Series: []lossPatternSeries{lossPatternOf("local", 0, tailSeries(12))}}
	if err := writeLossPatternText(brokenPipe{}, rep); err == nil {
		t.Error("writing to a closed pipe returned no error")
	}
	if err := writeLossPatternText(brokenPipe{}, lossPatternReport{Target: "/t"}); err == nil {
		t.Error("the no-measurements line swallowed a write error")
	}
}

// Protects the display limits: past 40 probes the histogram buckets positions
// and says so, and a pattern longer than 80 is cut with an ellipsis in the text
// but not in the json.
func TestLossPatternLongSeriesAreTruncated(t *testing.T) {
	var rows []store.Measurement
	for i := 0; i < 12; i++ {
		rows = append(rows, lossRow(replayNow.Unix()-int64(12-i)*60, 100, []int{95, 96, 97, 98, 99}, true))
	}
	rep := lossPatternReport{Target: "/t", Series: []lossPatternSeries{lossPatternOf("local", 0, rows)}}
	var buf bytes.Buffer
	if err := writeLossPatternText(&buf, rep); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "3 positions to a row") || !strings.Contains(out, "  34–36 ") {
		t.Errorf("histogram not bucketed:\n%s", out)
	}
	if n := strings.Count(out, "█"); n == 0 || strings.Count(out, "\n  1") > 60 {
		t.Errorf("unexpected histogram:\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat(".", 80)+"…") || strings.Contains(out, strings.Repeat(".", 81)) {
		t.Errorf("pattern not cut at 80:\n%s", out)
	}
	if got := len(rep.Series[0].Recent[0].Pattern); got != 100 {
		t.Errorf("json pattern is %d long, want the full 100", got)
	}
}

// lossPatternOf gathers rows into a judged series, the way the command does
// for a series it reads.
func lossPatternOf(vantage string, agent int64, rows []store.Measurement) lossPatternSeries {
	s := lossPatternSeries{Vantage: vantage, AgentID: agent}
	for i := range rows {
		s.add(&rows[i])
	}
	s.judge()
	return s
}

// Protects the memory bound: however long the window, only as many recorded
// intervals are kept as the detector can use, and the verdict is the one the
// whole history would have given, since it is about the newest of them.
func TestLossPatternKeepsOnlyWhatTheDetectorUses(t *testing.T) {
	rows := tailSeries(500)
	s := lossPatternOf("local", 0, rows)
	if s.Recorded != 500 || len(s.history) != losspattern.Window {
		t.Fatalf("recorded %d, kept %d; want 500 counted and %d kept", s.Recorded, len(s.history), losspattern.Window)
	}
	var all []losspattern.Interval
	for _, m := range rows {
		spread, lost, err := store.DecodeLossMask(m.LossMask, m.Sent, m.Received)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, losspattern.Interval{TS: m.TS, Spread: spread, Lost: lost})
	}
	want := losspattern.Test(all, true)
	if s.Verdict.Flagged != want.Flagged || *s.Verdict.P != want.P || s.Verdict.WindowIntervals != want.Intervals ||
		!s.Verdict.WindowFrom.Equal(time.Unix(want.From, 0)) {
		t.Errorf("verdict from the kept intervals %+v differs from the whole history's %+v", s.Verdict, want)
	}
}

// Protects two things a script reading the json depends on: an untested window
// has no p at all, rather than a p of zero, and positions are named only for a
// window that was flagged.
func TestLossPatternJSONDoesNotInventAPValue(t *testing.T) {
	few := lossPatternOf("local", 0, tailSeries(5))
	b, err := json.Marshal(few.Verdict)
	if err != nil {
		t.Fatal(err)
	}
	if few.Verdict.Tested || !strings.Contains(string(b), `"p":null`) || few.Verdict.Where != "" {
		t.Errorf("an untested verdict carries a p-value or positions: %s", b)
	}
}

// Protects against a window given in centuries by mistake, which would be read
// a day at a time for as long as that takes.
func TestLossPatternRefusesAnEndlessWindow(t *testing.T) {
	_, err := parseLossPatternArgs(strings.Fields("--target /t --from 0001-01-01T00:00:00Z --to 9999-12-31T00:00:00Z"), replayNow)
	if err == nil || !strings.Contains(err.Error(), "at most 400 days") {
		t.Errorf("an endless window was accepted: %v", err)
	}
	if _, err := parseLossPatternArgs(strings.Fields("--target /t --since 400d"), replayNow); err != nil {
		t.Errorf("400 days refused: %v", err)
	}
}
