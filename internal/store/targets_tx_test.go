package store

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Two writers must not read the tree at the same time. Each used to validate
// against the same snapshot, both passed, and both wrote: moving A under B and
// B under A are each fine alone and a cycle together.
//
// This holds the first writer inside its callback and starts a second. The
// second must not get into its own callback until the first has committed, and
// must then see what the first wrote. It is deterministic: nothing here depends
// on which goroutine happens to run first.
func TestChangeTargetsSerialisesWriters(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	entered, release := make(chan struct{}), make(chan struct{})
	var secondRan atomic.Bool

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- s.ChangeTargets(ctx, func([]tree.Target) (TargetChange, error) {
			close(entered)
			<-release
			root := int64(1)
			return TargetChange{Upsert: []*tree.Target{{ParentID: &root, Name: "first", Enabled: true}}}, nil
		})
	}()
	<-entered

	secondSaw := make(chan []string, 1)
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- s.ChangeTargets(ctx, func(cur []tree.Target) (TargetChange, error) {
			secondRan.Store(true)
			var names []string
			for _, n := range cur {
				names = append(names, n.Name)
			}
			secondSaw <- names
			return TargetChange{}, nil
		})
	}()

	time.Sleep(300 * time.Millisecond)
	if secondRan.Load() {
		t.Fatal("a second writer read the tree while the first was still inside its transaction")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	var sawFirst bool
	for _, n := range <-secondSaw {
		if n == "first" {
			sawFirst = true
		}
	}
	if !sawFirst {
		t.Error("the second writer validated against a tree that did not include the first writer's change")
	}
}

// Nothing is applied if anything fails: a write that creates one target and
// deletes another must not leave the first behind when the second is refused.
func TestChangeTargetsAppliesNothingWhenAWriteFails(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	parent := newTarget(t, s, "parent")
	root := int64(1)
	child := tree.Target{ParentID: &parent, Name: "child", Enabled: true}
	if err := s.UpsertTarget(ctx, &child); err != nil {
		t.Fatal(err)
	}
	err := s.ChangeTargets(ctx, func([]tree.Target) (TargetChange, error) {
		return TargetChange{
			Upsert: []*tree.Target{{ParentID: &root, Name: "ghost", Enabled: true}},
			// The parent while its child still names it: refused by the foreign key.
			Delete: []int64{parent},
		}, nil
	})
	if err == nil {
		t.Fatal("deleting a target that still has a child succeeded")
	}
	// Refused for the reason it should be, not because the write waited on a lock
	// it should have been holding: a create done outside the transaction blocks on
	// the transaction's own write lock and fails the same way for the wrong reason.
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("the write failed, but not on the foreign key: %v", err)
	}
	all, _ := s.ListTargets(ctx)
	for _, n := range all {
		if n.Name == "ghost" {
			t.Fatal("the created target survived a failed write")
		}
	}
}

func TestChangeTargetsReturnsTheCallbacksErrorUnchangedAndWritesNothing(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	root := int64(1)
	err := s.ChangeTargets(ctx, func([]tree.Target) (TargetChange, error) {
		return TargetChange{Upsert: []*tree.Target{{ParentID: &root, Name: "never", Enabled: true}}}, ErrAbort
	})
	if !errors.Is(err, ErrAbort) {
		t.Fatalf("err = %v, want ErrAbort", err)
	}
	all, _ := s.ListTargets(ctx)
	for _, n := range all {
		if n.Name == "never" {
			t.Fatal("a change was applied after the callback abandoned it")
		}
	}
}

// Rules are defined on a node and go with it. alert_rules has a foreign key to
// targets with no cascade, so deleting a node with a rule failed, and a
// recursive delete failed part-way: the children already gone, the node refused,
// the handler answering 500.
func TestDeletingATargetTakesItsRulesAndTheirStateAlong(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	target := newTarget(t, s, "withrule")
	rule := alert.Rule{TargetID: target, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 3, ClearFor: 3, Enabled: true}
	if err := s.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO alert_state (rule_id, target_id, agent_id, firing) VALUES (?, ?, 0, 1)`, rule.ID, target); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMeasurements(ctx, []Measurement{{TargetID: target, TS: 1000, Sent: 1, Received: 1, Samples: []uint32{5}}}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteTarget(ctx, target); err != nil {
		t.Fatalf("deleting a target that has a rule: %v", err)
	}
	var rules, states int
	s.db.QueryRow("SELECT COUNT(*) FROM alert_rules WHERE target_id = ?", target).Scan(&rules)
	s.db.QueryRow("SELECT COUNT(*) FROM alert_state WHERE rule_id = ?", rule.ID).Scan(&states)
	if rules != 0 || states != 0 {
		t.Errorf("%d rule(s) and %d state row(s) outlived their target", rules, states)
	}
	// History outlives the target, as it always has.
	got, err := s.QueryRange(ctx, target, 0, 0, 1<<40)
	if err != nil || len(got) != 1 {
		t.Errorf("the target's measurements went with it: %d rows, %v", len(got), err)
	}
}

// A rule on an ancestor keeps its state per target it applies to, keyed by the
// leaf. Deleting the leaf must take that row too, or a firing state for a target
// that no longer exists stays behind; the ancestor's state for other targets
// stays.
func TestDeletingATargetTakesTheStateOfInheritedRulesToo(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	parent := newTarget(t, s, "parent")
	leaf, other := newTarget(t, s, "leaf"), newTarget(t, s, "other")
	rule := alert.Rule{TargetID: parent, Name: "loss", Metric: alert.MetricLoss, Op: alert.OpGreater,
		Threshold: 20, For: 3, ClearFor: 3, Enabled: true}
	if err := s.UpsertAlertRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	for _, tgt := range []int64{leaf, other} {
		if _, err := s.db.Exec(`INSERT INTO alert_state (rule_id, target_id, agent_id, firing) VALUES (?, ?, 0, 1)`, rule.ID, tgt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteTarget(ctx, leaf); err != nil {
		t.Fatal(err)
	}
	var gone, kept int
	s.db.QueryRow("SELECT COUNT(*) FROM alert_state WHERE target_id = ?", leaf).Scan(&gone)
	s.db.QueryRow("SELECT COUNT(*) FROM alert_state WHERE target_id = ?", other).Scan(&kept)
	if gone != 0 {
		t.Errorf("%d state row(s) for a deleted target remain", gone)
	}
	if kept != 1 {
		t.Errorf("another target's state was deleted: %d rows remain, want 1", kept)
	}
}

// The callback reads through the pool while the transaction holds a connection.
// With more writers than connections, every connection used to be held by a
// writer waiting for the write lock, and the one that held it could not get a
// connection to read grants or agents with: they waited out the busy timeout
// and failed with SQLITE_BUSY.
func TestChangeTargetsDoesNotStarveItsOwnCallback(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	const writers = 12 // more than the pool holds
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.ChangeTargets(ctx, func([]tree.Target) (TargetChange, error) {
				_, err := s.ListTargets(ctx) // as scopeFor and checkAgentNames do
				return TargetChange{}, err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a writer failed: %v", err)
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("%d writers took %v; they waited on each other's connections", writers, d)
	}
}
