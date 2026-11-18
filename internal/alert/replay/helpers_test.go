package replay_test

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/alert/replay"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

const t0 = int64(1_756_400_040) // a multiple of 60

func ptr[T any](v T) *T { return &v }

func at(i int) int64 { return t0 + int64(i)*60 }

// env is a store with a small tree: Production/{web,db} and Lab/gw, one
// enrolled agent besides local, all at 60 s.
type env struct {
	t             *testing.T
	path          string
	st            *store.SQLite
	prod, web, db int64
	lab, gw       int64
	ams           int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "replay.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	e := &env{t: t, path: path, st: st}
	pub, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.AddAgent(ctx, "ams", pub)
	if err != nil {
		t.Fatal(err)
	}
	e.ams = rec.ID
	node := func(parent int64, name string, leaf bool, s tree.Settings) int64 {
		n := tree.Target{ParentID: &parent, Name: name, Enabled: true, Settings: s}
		if leaf {
			n.Host, n.AddressFamily = ptr("10.0.0."+fmt.Sprint(len(name))), ptr("v4")
		}
		if err := st.UpsertTarget(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	e.prod = node(1, "Production", false, tree.Settings{IntervalS: ptr(60)})
	e.web = node(e.prod, "web", true, tree.Settings{})
	e.db = node(e.prod, "db", true, tree.Settings{})
	e.lab = node(1, "Lab", false, tree.Settings{IntervalS: ptr(60)})
	e.gw = node(e.lab, "gw", true, tree.Settings{})
	return e
}

func (e *env) rule(node int64, name string, m alert.Metric, op alert.Op, thr float64, f, c int) alert.Rule {
	e.t.Helper()
	r := alert.Rule{TargetID: node, Name: name, Metric: m, Op: op, Threshold: thr,
		For: f, ClearFor: c, Enabled: true, Severity: alert.SeverityWarning}
	if m.IsShape() {
		r.Mode = alert.ModeTunable
	}
	if m == alert.MetricShape {
		r.Baseline = alert.BaselineRolling
	}
	return r
}

func (e *env) save(r alert.Rule) alert.Rule {
	e.t.Helper()
	if err := e.st.UpsertAlertRule(context.Background(), &r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// spec is what one interval looks like.
type spec struct {
	mean, mean2 float64 // ms; mean2 > 0 splits the interval into two clusters
	sd          float64
	lossPct     int
	skip        bool
	flags       uint8
}

func base(mean float64) spec { return spec{mean: mean, sd: 1.5} }

type row struct {
	ts      int64
	sent    int
	samples []uint32
	flags   uint8
}

func gen(seed uint64, n int, f func(i int) spec) map[int64]row {
	rng := rand.New(rand.NewPCG(seed, 7))
	out := map[int64]row{}
	for i := 0; i < n; i++ {
		s := f(i)
		if s.skip {
			continue
		}
		const sent = 20
		recv := sent - s.lossPct*sent/100
		sm := make([]uint32, recv)
		for k := range sm {
			mean := s.mean
			if s.mean2 > 0 && k%2 == 1 {
				mean = s.mean2
			}
			v := max(mean+s.sd*rng.NormFloat64(), 1)
			sm[k] = uint32(v * 1000)
		}
		slices.Sort(sm)
		out[at(i)] = row{ts: at(i), sent: sent, samples: sm, flags: s.flags}
	}
	return out
}

func (e *env) put(target, agent int64, rows map[int64]row) {
	e.t.Helper()
	var ms []store.Measurement
	for _, r := range rows {
		ms = append(ms, store.Measurement{TargetID: target, AgentID: agent, Family: tree.FamilyV4, TS: r.ts,
			Sent: r.sent, Received: len(r.samples), Flags: r.flags, Samples: r.samples})
	}
	if err := e.st.WriteMeasurements(context.Background(), ms); err != nil {
		e.t.Fatal(err)
	}
}

// trans is one transition as both engines can state it.
type trans struct {
	Rule, Target string
	Agent        int64
	Fired        bool
	TS           int64
	Value        float64 // the crossing value; fires only
}

func sortTrans(ts []trans) {
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.TS < b.TS
	})
}

func transitions(r *replay.Report) []trans {
	var out []trans
	for _, s := range r.Series {
		for _, ep := range s.Episodes {
			out = append(out, trans{s.Rule, s.Target, s.AgentID, true, ep.Start.Unix(), ep.CrossedAt})
			if ep.End != nil {
				out = append(out, trans{s.Rule, s.Target, s.AgentID, false, ep.End.Unix(), 0})
			}
		}
	}
	sortTrans(out)
	return out
}

func before(ts []trans, limit int64) []trans {
	var out []trans
	for _, x := range ts {
		if x.TS < limit {
			out = append(out, x)
		}
	}
	return out
}

func run(t *testing.T, src replay.Source, o replay.Options) *replay.Report {
	t.Helper()
	rep, err := replay.Run(context.Background(), src, o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

type qrange struct{ target, agent, from, to int64 }

// spy records every range asked of the store.
type spy struct {
	replay.Source
	mu     sync.Mutex
	ranges []qrange
	// tooLarge, when positive, refuses any range wider than this, as the store
	// does for one holding too many samples.
	tooLarge int64
}

func (s *spy) QueryRange(ctx context.Context, tg, ag int64, fam tree.Family, from, to int64) ([]store.Measurement, error) {
	s.mu.Lock()
	s.ranges = append(s.ranges, qrange{tg, ag, from, to})
	s.mu.Unlock()
	if s.tooLarge > 0 && to-from > s.tooLarge {
		return nil, fmt.Errorf("%w: test limit", store.ErrRangeTooLarge)
	}
	return s.Source.QueryRange(ctx, tg, ag, fam, from, to)
}

func (r *recorder) Notify(_ context.Context, as []alert.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range as {
		x := trans{Rule: a.Rule.Name, Target: a.TargetPath, Agent: a.AgentID, Fired: a.Firing}
		if a.Firing {
			x.TS, x.Value = a.Since.Unix(), a.Value
		} else {
			x.TS = a.Ended.Unix()
		}
		r.got = append(r.got, x)
	}
	return nil
}

type recorder struct {
	mu  sync.Mutex
	got []trans
}

func slicesSort(x []uint32) { slices.Sort(x) }
