package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert/replay"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

var replayNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// Protects the flag contract of `alerts replay`: exactly one window form, a rule
// unless a rules file stands in for it, and the --since units. A command people
// run in a hurry during an incident should refuse a half-specified window rather
// than guess one.
func TestAlertsReplayFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		err  string // substring; empty means valid
	}{
		{"since days", "--rule r --since 30d", ""},
		{"since hours", "--rule r --since 36h", ""},
		{"since minutes", "--rule r --since 90m", ""},
		{"from and to", "--rule r --from 2026-11-01T00:00:00Z --to 2026-11-02T00:00:00+01:00", ""},
		{"rules file alone", "--rules-file f.toml --since 1d", ""},
		{"rules file and rule", "--rules-file f.toml --rule r --since 1d", ""},
		{"no rule", "--since 1d", "--rule is required"},
		{"no window", "--rule r", "give a window"},
		{"only from", "--rule r --from 2026-11-01T00:00:00Z", "give a window"},
		{"both forms", "--rule r --since 1d --from 2026-11-01T00:00:00Z --to 2026-11-02T00:00:00Z", "not both"},
		{"since and to", "--rule r --since 1d --to 2026-11-02T00:00:00Z", "not both"},
		{"bad unit", "--rule r --since 30x", "--since"},
		{"zero", "--rule r --since 0d", "--since"},
		{"negative", "--rule r --since -5d", "--since"},
		{"no number", "--rule r --since d", "--since"},
		{"overflows a duration", "--rule r --since 213504d", "--since"},
		{"overflows in minutes", "--rule r --since 307445734562m", "--since"},
		{"not rfc3339", "--rule r --from 2026-11-01 --to 2026-11-02", "RFC 3339"},
		{"to before from", "--rule r --from 2026-11-02T00:00:00Z --to 2026-11-01T00:00:00Z", "after --from"},
		{"bad silences", "--rule r --since 1d --silences maybe", "--silences"},
		{"bad format", "--rule r --since 1d --format xml", "--format"},
		{"unknown flag", "--rule r --since 1d --bogus", "bogus"},
		{"positional", "--rule r --since 1d stray", "stray"},
	} {
		_, err := parseReplayArgs(strings.Fields(tc.args), replayNow)
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.err != "" && err == nil:
			t.Errorf("%s: accepted", tc.name)
		case tc.err != "" && (!strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "usage: smokeng alerts replay")):
			t.Errorf("%s: error %q should contain %q and a usage line", tc.name, err, tc.err)
		}
	}

	a, err := parseReplayArgs(strings.Fields("--rule r --since 30d --target /P --vantage ams --silences respect --format csv --db x.db"), replayNow)
	if err != nil {
		t.Fatal(err)
	}
	want := replay.Options{RuleName: "r", From: replayNow.Add(-30 * 24 * time.Hour).Unix(), To: replayNow.Unix(),
		Target: "/P", Vantage: "ams", Silences: replay.SilenceRespect}
	if a.opts.RuleName != want.RuleName || a.opts.From != want.From || a.opts.To != want.To || a.opts.Target != "/P" ||
		a.opts.Vantage != "ams" || a.opts.Silences != replay.SilenceRespect || a.format != "csv" || a.db != "x.db" {
		t.Errorf("parsed %+v, want %+v", a, want)
	}
	if a, _ = parseReplayArgs(strings.Fields("--rule r --since 1d"), replayNow); a.opts.Silences != replay.SilenceReport || a.format != "text" || a.db != "smokeng.db" {
		t.Errorf("defaults: %+v", a)
	}
}

func sampleReport() *replay.Report {
	t1 := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)
	t2 := t1.Add(10 * time.Minute)
	t3 := t1.Add(48 * time.Hour)
	return &replay.Report{
		Schema: 1, From: t1.Add(-time.Hour), To: t3.Add(time.Hour), Silences: replay.SilenceReport,
		Series: []replay.SeriesReport{{
			Rule: "loss", RuleNode: "/Production", Condition: "loss > 5% for 3 intervals",
			Target: "/Production/web", TargetID: 3, Vantage: "local", AgentID: 0, Intervals: 100,
			Fired: 2, Resolved: 1, FiringSeconds: 600 + 3600, LongestSeconds: 3600,
			Episodes: []replay.Episode{
				{Start: t1, End: &t2, Seconds: 600, CrossedAt: 7.5, Peak: 31, FireSilenced: true},
				{Start: t3, Open: true, Seconds: 3600, CrossedAt: 6, Peak: 6},
			},
			Warnings: []string{},
		}},
		Totals:   replay.Totals{Series: 1, SeriesFiring: 1, Episodes: 2, EpisodesPerDay: 1, Suppressed: 1},
		Warnings: []string{"w"}, Notes: []string{"n"},
	}
}

func keysOf(m map[string]any) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	slices.Sort(k)
	return k
}

// Protects the JSON schema: it is read by scripts and, later, an HTTP endpoint,
// so a field must not be renamed, dropped or added by accident. A rename is a
// change to this test and a bump of "schema".
func TestReplayJSONSchemaIsPinned(t *testing.T) {
	var buf bytes.Buffer
	if err := writeReplayJSON(&buf, sampleReport()); err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	check := func(what string, m map[string]any, want string) {
		t.Helper()
		if got := strings.Join(keysOf(m), " "); got != want {
			t.Errorf("%s keys = %s\nwant       %s", what, got, want)
		}
	}
	check("top level", top, "from notes schema series silences to totals warnings")
	if top["schema"] != float64(1) || top["silences"] != "report" || top["from"] != "2026-11-03T03:00:00Z" {
		t.Errorf("schema/silences/from = %v %v %v", top["schema"], top["silences"], top["from"])
	}
	check("totals", top["totals"].(map[string]any), "episodes episodes_per_day series series_firing suppressed")
	s := top["series"].([]any)[0].(map[string]any)
	check("series", s, "agent_id condition episodes fired firing_seconds intervals longest_seconds resolved rule rule_node target target_id vantage warnings")
	eps := s["episodes"].([]any)
	check("closed episode", eps[0].(map[string]any), "crossed_at end fire_silenced open peak resolve_silenced seconds start")
	check("open episode", eps[1].(map[string]any), "crossed_at fire_silenced open peak resolve_silenced seconds start")
	if eps[0].(map[string]any)["end"] != "2026-11-03T04:10:00Z" {
		t.Errorf("end = %v, want an RFC 3339 UTC string", eps[0].(map[string]any)["end"])
	}
}

// Protects the CSV columns, for the same reason.
func TestReplayCSVColumnsArePinned(t *testing.T) {
	var buf bytes.Buffer
	if err := writeReplayCSV(&buf, sampleReport()); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rows[0], ","); got != "rule,rule_node,target,vantage,start,end,seconds,crossed_at,peak,open,fire_silenced,resolve_silenced" {
		t.Errorf("header = %s", got)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows, want header and two episodes", len(rows))
	}
	if got := strings.Join(rows[1], ","); got != "loss,/Production,/Production/web,local,2026-11-03T04:00:00Z,2026-11-03T04:10:00Z,600,7.5,31,false,true,false" {
		t.Errorf("closed row = %s", got)
	}
	if got := strings.Join(rows[2], ","); got != "loss,/Production,/Production/web,local,2026-11-05T04:00:00Z,,3600,6,6,true,false,false" {
		t.Errorf("open row = %s", got)
	}
}

func TestReplayTextShowsWhatMatters(t *testing.T) {
	var buf bytes.Buffer
	writeReplayText(&buf, sampleReport())
	out := buf.String()
	for _, want := range []string{"rule loss on /Production: loss > 5% for 3 intervals", "still firing",
		"[fire silenced]", "crossed at 7.5, peak 31", "1 fired under a silence", "Notes:", "Warnings:"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
}

// Protects the wiring end to end on a real database: opened read-only, rules
// taken from a file that is not imported, all three formats, and that nothing
// was written (the file's rule must not exist in the database afterwards).
func TestAlertsReplayEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "smokeng.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	group := tree.Target{ParentID: new(int64(1)), Name: "Production", Enabled: true}
	if err := st.UpsertTarget(ctx, &group); err != nil {
		t.Fatal(err)
	}
	leaf := tree.Target{ParentID: &group.ID, Name: "web", Enabled: true, Host: new("10.0.0.1"), AddressFamily: new("v4")}
	if err := st.UpsertTarget(ctx, &leaf); err != nil {
		t.Fatal(err)
	}
	now := replayNow.Unix() / 60 * 60
	var ms []store.Measurement
	for i := 0; i < 60; i++ {
		recv := 10
		if i >= 20 && i < 30 {
			recv = 2
		}
		sm := make([]uint32, recv)
		for k := range sm {
			sm[k] = 5000
		}
		ms = append(ms, store.Measurement{TargetID: leaf.ID, TS: now - int64(60-i)*60, Sent: 10, Received: recv, Samples: sm})
	}
	if err := st.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	st.Close()

	rules := filepath.Join(dir, "rules.toml")
	if err := os.WriteFile(rules, []byte(`
[targets."Production".alerts.loss-test]
metric = "loss"
op = ">"
threshold = 50
for_intervals = 2
`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--db", db, "--rules-file", rules, "--since", "2h"}
	var out, errb bytes.Buffer
	if err := alertsReplay(append(base, "--format", "csv"), replayNow, &out, &errb); err != nil {
		t.Fatal(err)
	}
	rows, _ := csv.NewReader(&out).ReadAll()
	if len(rows) != 2 || rows[1][0] != "loss-test" || rows[1][2] != "/Production/web" || rows[1][9] != "false" {
		t.Errorf("csv rows = %v", rows)
	}
	out.Reset()
	if err := alertsReplay(append(base, "--format", "json"), replayNow, &out, &errb); err != nil {
		t.Fatal(err)
	}
	var rep replay.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.Totals.Episodes != 1 {
		t.Errorf("json: %v, totals %+v", err, rep.Totals)
	}
	out.Reset()
	if err := alertsReplay(append(base, "--format", "text"), replayNow, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rule loss-test on /Production") {
		t.Errorf("text:\n%s", out.String())
	}

	// A warning reaches stderr whatever the format. csv has no place for one,
	// and a golden rule with no reference would otherwise print a header and
	// nothing else, which reads as a rule that never fires.
	golden := filepath.Join(dir, "golden.toml")
	os.WriteFile(golden, []byte(`
[targets."Production".alerts.drift]
metric = "shape"
op = ">"
threshold = 5
mode = "tunable"
baseline = "golden"
`), 0o600)
	for _, format := range []string{"csv", "json", "text"} {
		out.Reset()
		errb.Reset()
		if err := alertsReplay([]string{"--db", db, "--rules-file", golden, "--since", "2h", "--format", format}, replayNow, &out, &errb); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errb.String(), "warning:") || !strings.Contains(errb.String(), "no captured reference") {
			t.Errorf("--format %s: stderr = %q, want the warning that the rule cannot fire", format, errb.String())
		}
	}

	// An empty file must not silently fall back to the stored rules.
	empty := filepath.Join(dir, "empty.toml")
	os.WriteFile(empty, []byte("[defaults]\ninterval_s = 60\n"), 0o600)
	if err := alertsReplay([]string{"--db", db, "--rules-file", empty, "--since", "2h"}, replayNow, &out, &errb); err == nil ||
		!strings.Contains(err.Error(), "no alert rules") {
		t.Errorf("empty rules file: %v", err)
	}

	ro, err := store.OpenReadOnly(db)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if stored, _ := ro.ListAlertRules(ctx); len(stored) != 0 {
		t.Errorf("the replay imported %d rules", len(stored))
	}
}

// Protects -h: the flag set prints nowhere, so help has to be put together by
// hand, and it once said only "help requested".
func TestAlertsReplayHelpListsTheFlags(t *testing.T) {
	_, err := parseReplayArgs([]string{"-h"}, replayNow)
	if err == nil {
		t.Fatal("-h returned no text")
	}
	for _, want := range []string{"usage: smokeng alerts replay", "-rules-file", "-silences", "window ending now"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("help lacks %q:\n%v", want, err)
		}
	}
}

// brokenPipe refuses every write, as a closed pipe does.
type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, os.ErrClosed }

// Protects the exit status: a text report that could not be written is a
// failure, as it already was for json and csv, not a truncated success.
func TestReplayTextReportsAFailedWrite(t *testing.T) {
	if err := writeReplayText(brokenPipe{}, sampleReport()); err == nil {
		t.Error("writing to a closed pipe returned no error")
	}
}
