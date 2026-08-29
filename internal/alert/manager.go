package alert

import (
	"context"
	stdlog "log"
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

	mu       sync.Mutex
	byTarget map[int64]applicable
	states   map[stateKey]*State
	rules    map[int64]*Rule
}

// NewManager evaluates rules and, when notifier is non-nil, delivers the
// transitions. A nil notifier is a supported configuration: the firing state
// and the transition log are worth having on their own.
func NewManager(st Store, notifier Notifier) *Manager {
	return &Manager{
		st:       st,
		notifier: notifier,
		byTarget: map[int64]applicable{},
		states:   map[stateKey]*State{},
		rules:    map[int64]*Rule{},
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
	m.byTarget, m.rules, m.states = resolved, byID, kept
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
			switch Evaluate(r, st, meas, app.intervalS) {
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

// Firing returns the alerts currently firing, for the API.
func (m *Manager) Firing() []Alert {
	var out []Alert
	m.mu.Lock()
	defer m.mu.Unlock()
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
	return out
}

// alertLocked builds a notification; the caller holds m.mu.
func (m *Manager) alertLocked(r *Rule, st *State, app applicable, firing bool) Alert {
	a := Alert{
		Rule: r, TargetID: st.TargetID, AgentID: st.AgentID,
		TargetPath: app.path, TargetHost: app.host,
		AgentName: "local", Firing: firing, Value: st.Value,
	}
	if st.Since != 0 {
		a.Since = time.Unix(st.Since, 0)
	}
	return a
}

func (m *Manager) deliver(ctx context.Context, alerts []Alert) {
	if len(alerts) == 0 {
		return
	}
	// Record before delivering. A transition that happened is a fact whether
	// or not the webhook was reachable, and the log is the only place anyone
	// can answer "when did this last fire" afterwards.
	m.record(ctx, alerts)
	if m.notifier == nil {
		return
	}
	if err := m.notifier.Notify(ctx, alerts); err != nil {
		stdlog.Printf("alert: deliver %d alert(s): %v", len(alerts), err)
	}
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
