package alert

import (
	"context"
	"fmt"
	stdlog "log"
	"slices"
	"sync"
	"time"

	"github.com/timdebruijn/smokeng/internal/tree"
)

// Store is the slice of persistence the manager needs.
type Store interface {
	ListAlertRules(ctx context.Context) ([]Rule, error)
	ListAlertStates(ctx context.Context) ([]State, error)
	SaveAlertStates(ctx context.Context, states []State) error
	ListTargets(ctx context.Context) ([]tree.Target, error)
	// AgentNames names every enrolled agent by id, local (0) included, so a
	// notification can say which agent an alert came from instead of
	// assuming it was always the local one.
	AgentNames(ctx context.Context) (map[int64]string, error)
	ListSilences(ctx context.Context) ([]Silence, error)
	CreateSilence(ctx context.Context, s *Silence) error
	DeleteSilence(ctx context.Context, id int64) (bool, error)
	// Captured reference distributions for golden-baseline shape rules.
	ListAlertBaselines(ctx context.Context) ([]Baselined, error)
	SaveAlertBaseline(ctx context.Context, b *Baselined) error
	DeleteAlertBaseline(ctx context.Context, ruleID int64) (bool, error)
}

// EventLog records transitions. It is optional so a store that cannot keep
// history still alerts; the manager simply has nothing to write to.
type EventLog interface {
	RecordAlertEvents(ctx context.Context, events []Event) error
}

type stateKey struct {
	ruleID, targetID, agentID int64
}

// applicable is a leaf target's resolved alerting context.
type applicable struct {
	rules     []*Rule
	intervalS int
	path      string
	host      string
}

// Manager owns rule resolution, evaluation state and notification. It is fed
// finalized measurements and emits notifications on state changes only —
// edge-triggered, per the design — plus a periodic repeat of what is still
// firing, which is what keeps an Alertmanager from expiring the alert.
type Manager struct {
	st       Store
	notifier Notifier

	mu         sync.Mutex
	byTarget   map[int64]applicable
	states     map[stateKey]*State
	rules      map[int64]*Rule
	agentNames map[int64]string
	silences   []Silence
	// ancestors maps a target id to the set of its ancestor ids, itself
	// included, so a silence scoped to a group can be matched against a leaf
	// under it without walking the tree on every alert.
	ancestors map[int64]map[int64]bool
	// shapes is the per-series memory the distribution-shape metrics need — a
	// rolling baseline and the divergence history they calibrate against. It is
	// not persisted: a restart re-warms it, which is honest, since a baseline
	// smokeng has not observed since restart is not one it can judge against.
	shapes map[stateKey]*shapeState
	// golden holds captured reference distributions for golden-baseline shape
	// rules, keyed by rule id. Loaded in Reload from the store.
	golden map[int64][]uint32
	// goldenMeta is what each reference was taken from, so the API can say which
	// window and series a golden baseline came from rather than showing an
	// anonymous curve.
	goldenMeta map[int64]Baselined
}

// NewManager evaluates rules and, when notifier is non-nil, delivers the
// transitions. A nil notifier is a supported configuration: the firing state
// and the transition log are worth having on their own.
func NewManager(st Store, notifier Notifier) *Manager {
	return &Manager{
		st:         st,
		notifier:   notifier,
		byTarget:   map[int64]applicable{},
		states:     map[stateKey]*State{},
		rules:      map[int64]*Rule{},
		agentNames: map[int64]string{},
		ancestors:  map[int64]map[int64]bool{},
		shapes:     map[stateKey]*shapeState{},
		golden:     map[int64][]uint32{},
		goldenMeta: map[int64]Baselined{},
	}
}

// Reload rebuilds the resolved rule set from the tree. Rules inherit like
// every other setting and replace rather than accumulate: the nearest
// ancestor that defines any rules defines the whole set for its subtree, so
// a child's rules are a complete override, and there is no puzzle about how
// to remove an inherited one.
func (m *Manager) Reload(ctx context.Context) error {
	targets, err := m.st.ListTargets(ctx)
	if err != nil {
		return err
	}
	tr, err := tree.New(targets)
	if err != nil {
		return err
	}
	rules, err := m.st.ListAlertRules(ctx)
	if err != nil {
		return err
	}
	states, err := m.st.ListAlertStates(ctx)
	if err != nil {
		return err
	}
	agentNames, err := m.st.AgentNames(ctx)
	if err != nil {
		return err
	}
	silences, err := m.st.ListSilences(ctx)
	if err != nil {
		return err
	}
	baselines, err := m.st.ListAlertBaselines(ctx)
	if err != nil {
		return err
	}
	golden := map[int64][]uint32{}
	goldenMeta := map[int64]Baselined{}
	for _, b := range baselines {
		golden[b.RuleID] = b.Samples
		goldenMeta[b.RuleID] = b
	}

	// Ancestor sets, so a silence scoped to a group can be matched against any
	// leaf beneath it by a map lookup rather than a walk. Each target maps to
	// itself and every id above it.
	byNodeID := map[int64]*tree.Target{}
	for i := range targets {
		byNodeID[targets[i].ID] = &targets[i]
	}
	ancestors := map[int64]map[int64]bool{}
	for i := range targets {
		set := map[int64]bool{}
		for cur := &targets[i]; cur != nil; {
			set[cur.ID] = true
			if cur.ParentID == nil {
				break
			}
			cur = byNodeID[*cur.ParentID]
		}
		ancestors[targets[i].ID] = set
	}

	byNode := map[int64][]*Rule{}
	byID := map[int64]*Rule{}
	for i := range rules {
		r := &rules[i]
		byID[r.ID] = r
		byNode[r.TargetID] = append(byNode[r.TargetID], r)
	}

	resolved := map[int64]applicable{}
	for i := range targets {
		n := &targets[i]
		if n.Host == nil || !n.Enabled {
			continue
		}
		res, err := tr.Resolve(n.ID)
		if err != nil {
			return err
		}
		path, err := tr.Path(n.ID)
		if err != nil {
			return err
		}
		app := applicable{intervalS: res.IntervalS.Effective, path: path, host: *n.Host}
		// Walk up until a node defines rules; the first one wins outright.
		for cur := n; ; {
			if rs := byNode[cur.ID]; len(rs) > 0 {
				app.rules = rs
				break
			}
			if cur.ParentID == nil {
				break
			}
			parent, ok := tr.Get(*cur.ParentID)
			if !ok {
				break
			}
			cur = parent
		}
		resolved[n.ID] = app
	}

	kept := map[stateKey]*State{}
	for i := range states {
		st := states[i]
		// Drop state for rules that no longer exist.
		if _, ok := byID[st.RuleID]; !ok {
			continue
		}
		kept[stateKey{st.RuleID, st.TargetID, st.AgentID}] = &st
	}

	m.mu.Lock()
	// The states read from the database are a snapshot from before this
	// function started, and Observe has been advancing hysteresis in memory
	// the whole time. Overwriting wholesale rolled that progress back, so a
	// tree edit — or the periodic reload — silently reset the streak of every
	// rule that was part-way to firing or clearing. Keep what is already in
	// memory; the stored copy exists to survive a restart, not to win a race
	// against the live one.
	for key, live := range m.states {
		if _, ok := byID[key.ruleID]; ok {
			kept[key] = live
		}
	}
	// Drop the shape memory of rules that no longer exist, so it does not grow
	// without bound as rules come and go.
	for key := range m.shapes {
		if _, ok := byID[key.ruleID]; !ok {
			delete(m.shapes, key)
		}
	}
	m.byTarget, m.rules, m.states, m.agentNames = resolved, byID, kept, agentNames
	m.ancestors = ancestors
	m.golden, m.goldenMeta = golden, goldenMeta
	// Silences are owned by the API between reloads (AddSilence/RemoveSilence
	// keep the in-memory copy current the moment a change is made), so this
	// wholesale refresh is only for a restart or a change made straight to the
	// database. It is safe because a create or delete also writes through here.
	m.silences = silences
	m.mu.Unlock()
	return nil
}

// Observe evaluates a batch of finalized measurements and delivers whatever
// changed. Evaluation happens whether or not anything is configured to receive
// the result: the firing state and the transition log are read from the UI.
// Notifications are best-effort: a webhook that is down must never stop
// measurement, so a failure is logged and the state still advances.
func (m *Manager) Observe(ctx context.Context, ms []Input) {
	var fired []Alert
	var dirty []State

	m.mu.Lock()
	for i := range ms {
		meas := &ms[i]
		app, ok := m.byTarget[meas.TargetID]
		if !ok || len(app.rules) == 0 {
			continue
		}
		for _, r := range app.rules {
			key := stateKey{r.ID, meas.TargetID, meas.AgentID}
			st, ok := m.states[key]
			if !ok {
				st = &State{RuleID: r.ID, TargetID: meas.TargetID, AgentID: meas.AgentID}
				m.states[key] = st
			}
			var t Transition
			if r.Metric.IsShape() {
				// Shape metrics are computed from history, not from one interval:
				// the manager owns the baseline and divergence buffers and feeds
				// the value into the same hysteresis every other rule uses.
				ss := m.shapes[key]
				if ss == nil {
					ss = &shapeState{}
					m.shapes[key] = ss
				}
				value, ok := ss.value(r, meas, m.golden[r.ID])
				t = EvaluateValue(r, st, value, ok, meas.RTTTrusted, meas.TS, app.intervalS)
			} else {
				t = Evaluate(r, st, meas, app.intervalS)
			}
			switch t {
			case Fired:
				fired = append(fired, m.alertLocked(r, st, app, true))
			case Resolved:
				fired = append(fired, m.alertLocked(r, st, app, false))
			}
			dirty = append(dirty, *st)
		}
	}
	m.mu.Unlock()

	if len(dirty) > 0 {
		if err := m.st.SaveAlertStates(ctx, dirty); err != nil {
			stdlog.Printf("alert: save state: %v", err)
		}
	}
	m.deliver(ctx, fired)
}

// Repeat re-sends everything still firing. Alertmanager expires an alert it
// stops hearing about, so silence would look like recovery.
func (m *Manager) Repeat(ctx context.Context) {
	if m.notifier == nil {
		return
	}
	var out []Alert
	m.mu.Lock()
	for key, st := range m.states {
		if !st.Firing {
			continue
		}
		r, ok := m.rules[key.ruleID]
		if !ok {
			continue
		}
		app, ok := m.byTarget[st.TargetID]
		if !ok {
			continue
		}
		out = append(out, m.alertLocked(r, st, app, true))
	}
	m.mu.Unlock()
	m.deliver(ctx, out)
}

// Firing returns the alerts currently firing, for the API. A silenced alert is
// still firing and still listed — the operator should see it — but marked so the
// UI can show it as muted rather than as demanding attention.
func (m *Manager) Firing() []Alert {
	var out []Alert
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	for key, st := range m.states {
		if !st.Firing {
			continue
		}
		r, ok := m.rules[key.ruleID]
		if !ok {
			continue
		}
		app, ok := m.byTarget[st.TargetID]
		if !ok {
			continue
		}
		a := m.alertLocked(r, st, app, true)
		if silenced, until := m.silencedLocked(now, a); silenced {
			a.Silenced = true
			a.SilencedUntil = time.Unix(until, 0)
		}
		out = append(out, a)
	}
	return out
}

// alertLocked builds a notification; the caller holds m.mu.
func (m *Manager) alertLocked(r *Rule, st *State, app applicable, firing bool) Alert {
	agentName := m.agentNames[st.AgentID]
	if agentName == "" {
		// Reload runs periodically, so a just-enrolled or just-removed agent
		// can be briefly missing from the cache. Naming it by id, rather than
		// silently mislabeling it "local", keeps the notification honest.
		agentName = fmt.Sprintf("agent %d", st.AgentID)
	}
	a := Alert{
		Rule: r, TargetID: st.TargetID, AgentID: st.AgentID,
		TargetPath: app.path, TargetHost: app.host,
		AgentName: agentName, Firing: firing, Value: st.Value,
	}
	if st.Since != 0 {
		a.Since = time.Unix(st.Since, 0)
	}
	if st.Acked() {
		a.Acked, a.AckedBy = true, st.AckedBy
		if st.AckedAt != 0 {
			a.AckedAt = time.Unix(st.AckedAt, 0)
		}
	}
	return a
}

// Acknowledge marks a firing alert seen, or clears that mark when ack is false.
// It returns whether a firing alert was found to change — false when nothing is
// firing for that (rule, target, agent), so the caller can answer 404 rather
// than pretend it did something.
//
// The change is persisted at once rather than waiting for the next
// measurement's save, so an acknowledgement is not lost to a restart in the
// gap — and because firing state is itself persisted, the ack still matches its
// episode after a restart.
func (m *Manager) Acknowledge(ctx context.Context, ruleID, targetID, agentID int64, ack bool, by string) (bool, error) {
	m.mu.Lock()
	st, ok := m.states[stateKey{ruleID, targetID, agentID}]
	if !ok || !st.Firing {
		m.mu.Unlock()
		return false, nil
	}
	if ack {
		st.AckedSince, st.AckedAt, st.AckedBy = st.Since, time.Now().Unix(), by
	} else {
		st.AckedSince, st.AckedAt, st.AckedBy = 0, 0, ""
	}
	snapshot := *st
	m.mu.Unlock()

	if err := m.st.SaveAlertStates(ctx, []State{snapshot}); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) deliver(ctx context.Context, alerts []Alert) {
	if len(alerts) == 0 {
		return
	}
	// Record before delivering, and record everything — a silence suppresses
	// the notification, not the fact. A transition that happened is a fact
	// whether or not the webhook was reachable or the alert was silenced, and
	// the log is the only place anyone can answer "when did this last fire"
	// afterwards.
	m.record(ctx, alerts)
	if m.notifier == nil {
		return
	}
	// Drop what a silence covers right now. A maintenance window means "do not
	// page during this", so the fire and any resolve inside it are not posted;
	// when the window closes, Repeat re-announces whatever is still firing.
	now := time.Now().Unix()
	send := make([]Alert, 0, len(alerts))
	m.mu.Lock()
	for _, a := range alerts {
		if silenced, _ := m.silencedLocked(now, a); !silenced {
			send = append(send, a)
		}
	}
	m.mu.Unlock()
	if len(send) == 0 {
		return
	}
	if err := m.notifier.Notify(ctx, send); err != nil {
		stdlog.Printf("alert: deliver %d alert(s): %v", len(send), err)
	}
}

// silencedLocked reports whether an active silence covers this alert, and until
// when (the latest end among the silences that do, for the UI to show). The
// caller holds m.mu.
func (m *Manager) silencedLocked(now int64, a Alert) (bool, int64) {
	ruleID := int64(0)
	if a.Rule != nil {
		ruleID = a.Rule.ID
	}
	var until int64
	silenced := false
	for i := range m.silences {
		s := &m.silences[i]
		if !s.activeAt(now) {
			continue
		}
		if s.RuleID != nil && *s.RuleID != ruleID {
			continue
		}
		if s.AgentID != nil && *s.AgentID != a.AgentID {
			continue
		}
		if s.TargetID != nil && !m.ancestors[a.TargetID][*s.TargetID] {
			continue
		}
		silenced = true
		if s.EndsAt > until {
			until = s.EndsAt
		}
	}
	return silenced, until
}

// AddSilence stores a silence and applies it at once, returning it with its
// assigned id. Applying immediately, rather than at the next reload, is the
// point: an operator silencing a target before starting maintenance expects the
// paging to stop now, not on the next poll.
func (m *Manager) AddSilence(ctx context.Context, s Silence) (Silence, error) {
	if err := s.Validate(); err != nil {
		return Silence{}, err
	}
	s.CreatedAt = time.Now().Unix()
	if err := m.st.CreateSilence(ctx, &s); err != nil {
		return Silence{}, err
	}
	m.mu.Lock()
	m.silences = append(m.silences, s)
	m.mu.Unlock()
	return s, nil
}

// RemoveSilence deletes a silence and lifts it at once, reporting whether one
// existed to remove.
func (m *Manager) RemoveSilence(ctx context.Context, id int64) (bool, error) {
	ok, err := m.st.DeleteSilence(ctx, id)
	if err != nil || !ok {
		return ok, err
	}
	m.mu.Lock()
	m.silences = slices.DeleteFunc(m.silences, func(s Silence) bool { return s.ID == id })
	m.mu.Unlock()
	return true, nil
}

// CaptureBaseline stores samples as a golden-baseline rule's reference and
// applies it at once. The caller supplies the samples and the window they came
// from; what this owns is making the rule use them without waiting for a reload.
func (m *Manager) CaptureBaseline(ctx context.Context, b Baselined) error {
	if len(b.Samples) == 0 {
		return fmt.Errorf("alert: a baseline needs samples: that window has no measurements to capture")
	}
	if err := m.st.SaveAlertBaseline(ctx, &b); err != nil {
		return err
	}
	m.mu.Lock()
	m.golden[b.RuleID] = b.Samples
	m.goldenMeta[b.RuleID] = b
	m.mu.Unlock()
	return nil
}

// ClearBaseline removes a rule's captured reference, reporting whether one
// existed. A golden rule without a reference simply does not fire — it has
// nothing to compare against, and inventing one would be worse.
func (m *Manager) ClearBaseline(ctx context.Context, ruleID int64) (bool, error) {
	ok, err := m.st.DeleteAlertBaseline(ctx, ruleID)
	if err != nil || !ok {
		return ok, err
	}
	m.mu.Lock()
	delete(m.golden, ruleID)
	delete(m.goldenMeta, ruleID)
	m.mu.Unlock()
	return true, nil
}

// Baselines reports the captured references, for the API to show what each
// golden rule is comparing against.
func (m *Manager) Baselines(context.Context) ([]Baselined, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Baselined, 0, len(m.goldenMeta))
	for _, b := range m.goldenMeta {
		out = append(out, b)
	}
	return out, nil
}

// ShapeReference returns the distribution a shape rule is currently comparing
// against for one series: the captured reference for a golden rule, or the
// pooled recent history for a rolling one. It is what the UI overlays against
// the current interval, so a fired shape alert can be seen rather than taken on
// the word of a z-score. ok is false when there is nothing to compare against
// yet — a golden rule with no capture, or a rolling one still warming up.
func (m *Manager) ShapeReference(ruleID, targetID, agentID int64) (samples []uint32, kind string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, exists := m.rules[ruleID]
	if !exists || r.Metric != MetricShape {
		return nil, "", false
	}
	if r.Baseline == BaselineGolden {
		s := m.golden[ruleID]
		return s, "golden", len(s) > 0
	}
	ss := m.shapes[stateKey{ruleID, targetID, agentID}]
	if ss == nil {
		return nil, "rolling", false
	}
	s := ss.pooled()
	return s, "rolling", len(s) > 0
}

// ListSilences returns every silence, past and future, for the API to show.
func (m *Manager) ListSilences(context.Context) ([]Silence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Silence, len(m.silences))
	copy(out, m.silences)
	return out, nil
}

func (m *Manager) record(ctx context.Context, alerts []Alert) {
	sink, ok := m.st.(EventLog)
	if !ok {
		return
	}
	now := time.Now().Unix()
	events := make([]Event, 0, len(alerts))
	for _, a := range alerts {
		// Since is when the condition started holding, and Evaluate clears it
		// on the way out — so a resolved alert carries the zero time, whose
		// Unix() is -62135596800. What the log wants is when the transition
		// happened, which for a resolve is now and for a fire is when it
		// started.
		events = append(events, Event{
			TS: transitionTS(a, now), RuleID: a.Rule.ID, TargetID: a.TargetID, AgentID: a.AgentID,
			Firing: a.Firing, RuleName: a.Rule.Name, Describes: a.Rule.Describe(), Value: a.Value,
		})
	}
	if err := sink.RecordAlertEvents(ctx, events); err != nil {
		stdlog.Printf("alert: record %d transition(s): %v", len(events), err)
	}
}

// Delivering reports whether transitions go anywhere beyond the log.
func (m *Manager) Delivering() bool { return m.notifier != nil }

// transitionTS is when a transition happened. Since is when the condition
// started holding and Evaluate clears it on the way out, so a resolved alert
// carries the zero time — whose Unix() is -62135596800, the year 1.
func transitionTS(a Alert, now int64) int64 {
	if a.Firing && !a.Since.IsZero() {
		return a.Since.Unix()
	}
	return now
}
