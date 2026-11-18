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
	// RecordAlertEvents appends the transition log.
	RecordAlertEvents(ctx context.Context, events []Event) error
}

// stateKey is a series' rule standing: the series is (target, agent, family).
type stateKey struct {
	ruleID, targetID, agentID int64
	family                    tree.Family
}

// Manager owns rule resolution, evaluation state and notification. It is fed
// finalized measurements and emits notifications on state changes only —
// edge-triggered, per the design — plus a periodic repeat of what is still
// firing, which is what keeps an Alertmanager from expiring the alert.
type Manager struct {
	st       Store
	notifier Notifier

	// observing admits one Observe at a time. The prober's writer was the only
	// caller; the ingest handler is a second, on its own goroutines. Evaluation
	// was already under mu, but each call saves its snapshot of the states and
	// records its transitions after releasing it, and two calls could do that
	// in the other order: the older snapshot written last, so a restart read
	// back an alert as firing that had resolved.
	observing sync.Mutex

	mu         sync.Mutex
	byTarget   map[int64]Applicable
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
	// rules, keyed by rule id, with what each was taken from so the API can say
	// which window and series it came from. Loaded in Reload from the store.
	golden map[int64]Baselined
}

// NewManager evaluates rules and, when notifier is non-nil, delivers the
// transitions. A nil notifier is a supported configuration: the firing state
// and the transition log are worth having on their own.
func NewManager(st Store, notifier Notifier) *Manager {
	return &Manager{
		st:         st,
		notifier:   notifier,
		byTarget:   map[int64]Applicable{},
		states:     map[stateKey]*State{},
		rules:      map[int64]*Rule{},
		agentNames: map[int64]string{},
		ancestors:  map[int64]map[int64]bool{},
		shapes:     map[stateKey]*shapeState{},
		golden:     map[int64]Baselined{},
	}
}

// Reload rebuilds the resolved rule set from the tree; Resolve says how rules
// inherit.
func (m *Manager) Reload(ctx context.Context) error {
	targets, err := m.st.ListTargets(ctx)
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
	golden := map[int64]Baselined{}
	for _, b := range baselines {
		golden[b.RuleID] = b
	}

	resolved, err := Resolve(targets, rules)
	if err != nil {
		return err
	}
	ancestors := Ancestors(targets)
	byID := map[int64]*Rule{}
	for i := range rules {
		byID[rules[i].ID] = &rules[i]
	}

	kept := map[stateKey]*State{}
	for i := range states {
		st := states[i]
		// Drop state for rules that no longer exist.
		if _, ok := byID[st.RuleID]; !ok {
			continue
		}
		kept[stateKey{st.RuleID, st.TargetID, st.AgentID, st.Family}] = &st
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
	// A state of a family its target is not measured over has no series left
	// to clear it: no interval of that family will arrive, so keyed on the
	// family the alert would fire, and be repeated to the webhook, for good.
	// Such a state is ended, and so is its shape memory. No resolve is sent,
	// since nothing recovered: Alertmanager expires an alert it stops hearing
	// about.
	//
	// This is a standing rule applied at every reload, the first after a start
	// included, not a reaction to a change: it needs no memory of what the
	// target was before, so a family dropped while the master was stopped is
	// ended too, and so is a state that comes back after it was ended. Two
	// things can bring one back: an Observe that took its snapshot before this
	// reload and saves it afterwards, and a save below that failed. Both leave a
	// firing row in the store, which the next reload reads and ends again.
	// Observe, for its part, does not evaluate an interval of such a family: the
	// prober's old loop is stopped only after this returns and can still
	// deliver one.
	//
	// A target that is not in the resolved set (a group, disabled, deleted) is
	// not judged here: it has no families to compare with.
	var ended []State
	for key, st := range kept {
		app, ok := resolved[key.targetID]
		if !ok || slices.Contains(app.Families, key.family) {
			continue
		}
		if st.Firing {
			e := *st
			e.Firing, e.Since, e.Streak = false, 0, 0
			e.AckedSince, e.AckedAt, e.AckedBy = 0, 0, ""
			ended = append(ended, e)
		}
		delete(kept, key)
		delete(m.shapes, key)
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
	m.golden = golden
	// Silences are owned by the API between reloads (AddSilence/RemoveSilence
	// keep the in-memory copy current the moment a change is made), so this
	// wholesale refresh is only for a restart or a change made straight to the
	// database. It is safe because a create or delete also writes through here.
	m.silences = silences
	m.mu.Unlock()
	// Stored as cleared rather than left as it was: a row still marked firing
	// would come back as a firing alert at the next restart. If this fails the
	// row is read and ended again by the next reload.
	if len(ended) > 0 {
		if err := m.st.SaveAlertStates(ctx, ended); err != nil {
			return err
		}
	}
	return nil
}

// Observe evaluates a batch of finalized measurements and delivers whatever
// changed. Evaluation happens whether or not anything is configured to receive
// the result: the firing state and the transition log are read from the UI.
// Notifications are best-effort: a webhook that is down must never stop
// measurement, so a failure is logged and the state still advances.
func (m *Manager) Observe(ctx context.Context, ms []Input) {
	m.observing.Lock()
	defer m.observing.Unlock()
	var fired []Alert
	var dirty []State

	m.mu.Lock()
	for i := range ms {
		meas := &ms[i]
		app, ok := m.byTarget[meas.TargetID]
		if !ok || len(app.Rules) == 0 {
			continue
		}
		// An interval of a family the target is not measured over, such as a
		// late one of the family it was changed away from. It would start a
		// state that nothing after it could clear.
		if !slices.Contains(app.Families, meas.Family) {
			continue
		}
		for _, r := range app.Rules {
			key := stateKey{r.ID, meas.TargetID, meas.AgentID, meas.Family}
			st, ok := m.states[key]
			if !ok {
				st = &State{RuleID: r.ID, TargetID: meas.TargetID, AgentID: meas.AgentID, Family: meas.Family}
				m.states[key] = st
			}
			// An interval at or before the last one this rule has seen is
			// history arriving late: an agent whose clock stepped back, or the
			// prober rewriting an interval it cut short. The rule counts
			// consecutive intervals forwards, so a step back would count as the
			// next one, and would move LastTS, which dates a resolve, into the
			// past. It is stored and drawn; it is not evaluated.
			if st.LastTS != 0 && meas.TS <= st.LastTS {
				continue
			}
			// The manager owns a shape rule's baseline and divergence buffers;
			// the other metrics have none.
			var ss *shapeState
			if r.Metric.IsShape() {
				if ss = m.shapes[key]; ss == nil {
					ss = &shapeState{}
					m.shapes[key] = ss
				}
			}
			switch step(r, st, ss, meas, m.golden[r.ID].Samples, &app) {
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
	m.deliver(ctx, m.Firing())
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
func (m *Manager) alertLocked(r *Rule, st *State, app Applicable, firing bool) Alert {
	agentName := m.agentNames[st.AgentID]
	if agentName == "" {
		// Reload runs periodically, so a just-enrolled or just-removed agent
		// can be briefly missing from the cache. Naming it by id, rather than
		// silently mislabeling it "local", keeps the notification honest.
		agentName = fmt.Sprintf("agent %d", st.AgentID)
	}
	a := Alert{
		Rule: r, TargetID: st.TargetID, AgentID: st.AgentID, Family: st.Family,
		TargetPath: app.Path, TargetHost: app.Host,
		AgentName: agentName, Firing: firing, Value: st.Value,
	}
	if st.Since != 0 {
		a.Since = time.Unix(st.Since, 0)
	}
	if !firing {
		// The interval that resolved it, as Since is the interval that fired it.
		// Processing and delivery can run long after: buffered agent batches,
		// a queue behind a slow receiver.
		if st.LastTS != 0 {
			a.Ended = time.Unix(st.LastTS, 0)
		} else {
			a.Ended = time.Now()
		}
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
// firing for that (rule, target, agent, family), so the caller can answer 404 rather
// than pretend it did something.
//
// The change is persisted at once rather than waiting for the next
// measurement's save, so an acknowledgement is not lost to a restart in the
// gap — and because firing state is itself persisted, the ack still matches its
// episode after a restart.
func (m *Manager) Acknowledge(ctx context.Context, ruleID, targetID, agentID int64, family tree.Family, ack bool, by string) (bool, error) {
	m.mu.Lock()
	st, ok := m.states[stateKey{ruleID, targetID, agentID, family}]
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
		if !s.Covers(now, ruleID, a.AgentID, a.Family, m.ancestors[a.TargetID]) {
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
	m.golden[b.RuleID] = b
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
	m.mu.Unlock()
	return true, nil
}

// Baselines reports the captured references, for the API to show what each
// golden rule is comparing against.
func (m *Manager) Baselines() []Baselined {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Baselined, 0, len(m.golden))
	for _, b := range m.golden {
		out = append(out, b)
	}
	return out
}

// NearestRuleNode returns the node whose rules apply to a target: the target
// itself or the nearest ancestor that defines any, which wins outright. Rules
// replace rather than accumulate, so a node that defines any hides every rule
// above it from everything beneath it. Reload applies this to evaluate, and the
// API asks it which rules reach a target, because "is an ancestor of" is not the
// same question: a rule on an ancestor that a nearer node overrides never fires
// for the target, and saying otherwise would show a rule that does not apply.
func NearestRuleNode(tr *tree.Tree, hasRules func(nodeID int64) bool, targetID int64) (int64, bool) {
	cur, ok := tr.Get(targetID)
	for ok {
		if hasRules(cur.ID) {
			return cur.ID, true
		}
		if cur.ParentID == nil {
			break
		}
		cur, ok = tr.Get(*cur.ParentID)
	}
	return 0, false
}

// ShapeRef is a shape rule's reference together with where it came from, read
// in one step. Two reads were two snapshots: a recapture or a clear between them
// let the source of one reference be applied to the samples of another.
type ShapeRef struct {
	Samples []uint32
	Kind    string
	OK      bool
	// Source is the series a golden reference was captured from (its target and
	// family); HasSource is false for a rolling reference, which is the series'
	// own history.
	Source       int64
	SourceFamily tree.Family
	HasSource    bool
}

// ShapeReferenceDetail returns the distribution a shape rule is comparing
// against for one series (captured for golden, pooled history for rolling) and,
// for golden, the series it was captured from, read under one lock. OK is false
// when there is nothing to compare against yet.
func (m *Manager) ShapeReferenceDetail(ruleID, targetID, agentID int64, family tree.Family) ShapeRef {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, exists := m.rules[ruleID]
	if !exists || r.Metric != MetricShape {
		return ShapeRef{}
	}
	if r.Baseline == BaselineGolden {
		b, ok := m.golden[ruleID]
		ref := ShapeRef{Samples: b.Samples, Kind: "golden", OK: len(b.Samples) > 0}
		if ok {
			ref.Source, ref.SourceFamily, ref.HasSource = b.TargetID, b.Family, true
		}
		return ref
	}
	ss := m.shapes[stateKey{ruleID, targetID, agentID, family}]
	if ss == nil {
		return ShapeRef{Kind: "rolling"}
	}
	s := ss.pooled()
	return ShapeRef{Samples: s, Kind: "rolling", OK: len(s) > 0}
}

// ListSilences returns every silence, past and future, for the API to show.
func (m *Manager) ListSilences() []Silence {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Silence, len(m.silences))
	copy(out, m.silences)
	return out
}

func (m *Manager) record(ctx context.Context, alerts []Alert) {
	now := time.Now().Unix()
	events := make([]Event, 0, len(alerts))
	for _, a := range alerts {
		events = append(events, Event{
			TS: transitionTS(a, now), RuleID: a.Rule.ID, TargetID: a.TargetID, AgentID: a.AgentID, Family: a.Family,
			Firing: a.Firing, RuleName: a.Rule.Name, Describes: a.Rule.Describe(), Value: a.Value,
		})
	}
	if err := m.st.RecordAlertEvents(ctx, events); err != nil {
		stdlog.Printf("alert: record %d transition(s): %v", len(events), err)
	}
}

// Delivering reports whether transitions go anywhere beyond the log.
func (m *Manager) Delivering() bool { return m.notifier != nil }

// transitionTS is when a transition happened: the interval that fired the
// rule, or the one that resolved it. Since is when the condition started
// holding and Evaluate clears it on the way out, so a resolved alert carries
// the zero time — whose Unix() is -62135596800, the year 1 — and is dated by
// Ended instead.
//
// Not by the clock: an agent that was cut off delivers its intervals hours
// later, and a resolve stamped on arrival beside a fire stamped by its interval
// recorded an episode as lasting until the link came back.
func transitionTS(a Alert, now int64) int64 {
	if a.Firing && !a.Since.IsZero() {
		return a.Since.Unix()
	}
	if !a.Firing && !a.Ended.IsZero() {
		return a.Ended.Unix()
	}
	return now
}
