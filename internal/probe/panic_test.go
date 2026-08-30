package probe

import (
	"context"
	"io"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// The recover paths log a stack on purpose. Tests exercise them deliberately,
// so the noise is silenced rather than printed as though something went wrong.
func quietLog(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	// os.Stderr, not nil: the default logger writes to whatever it is given,
	// and nil makes every later log.Printf in the package panic — which is a
	// spectacular way for one test's cleanup to fail another test.
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// A panic in one probe used to end the process, taking the UI, the API and
// every other target's measurement with it.
func TestRecoverProbeContainsAPanic(t *testing.T) {
	quietLog(t)
	e := &Engine{}
	func() {
		defer e.recoverProbe(TargetSpec{TargetID: 1, Host: "example"}, "probe")
		panic("a malformed reply")
	}()
	if n := e.panics.Load(); n != 1 {
		t.Fatalf("panics counter is %d, want 1 — a contained panic that is not counted is invisible", n)
	}
}

// Containing the panic is only half of it. The loop has ended, and if the
// target stays in the running set reload will never start it again — so the
// target would simply stop being measured, quietly, forever.
func TestRecoverTargetDeschedulesSoReloadRestartsIt(t *testing.T) {
	quietLog(t)
	e := &Engine{running: map[int64]*runningTarget{}, warned: map[int64]string{}}
	spec := TargetSpec{TargetID: 7, Host: "example"}
	_, cancel := context.WithCancel(context.Background())
	e.running[spec.TargetID] = &runningTarget{spec: spec, cancel: cancel}

	func() {
		defer e.recoverTarget(spec)
		panic("boom")
	}()

	if _, still := e.running[spec.TargetID]; still {
		t.Fatal("the target is still registered as running, so reload will never restart it " +
			"and it stops being measured with no error anywhere")
	}
	if n := e.panics.Load(); n != 1 {
		t.Fatalf("panics counter is %d, want 1", n)
	}
}

// The reason the recovers are narrow, stated as a test: a panic taken while a
// mutex is held must still release it. A process that survives a panic into a
// deadlock is worse off than one that crashed — systemd restarts a crash and
// cannot see a wedge.
func TestCollectorPanicDoesNotWedgeFinalize(t *testing.T) {
	var late atomic.Int64
	col := newCollector(1, &late)

	func() {
		defer func() { _ = recover() }()
		// Out of range: this panics inside markSent, with the mutex held.
		col.markSent(5, 0, time.Now())
	}()

	done := make(chan struct{})
	go func() {
		col.finalize(TargetSpec{Pings: 1, TimeoutMS: 1000}, 0, conditions{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finalize never returned: the panic left the collector mutex held, " +
			"which wedges the prober instead of crashing it")
	}
}

// M2: a panic while parsing a packet off the wire or the error queue must be
// contained and counted, not allowed to end the process. The receive socket is
// shared by every target of its (family, DSCP) pair, so an uncontained panic
// here would stop measurement for all of them — the "one probe takes down the
// prober" outcome the design forbids, driven by untrusted network input.
func TestRecoverReceiveContainsAPanic(t *testing.T) {
	quietLog(t)
	var panics atomic.Int64
	c := &conn{family: "v4", panics: &panics}
	func() {
		defer c.recoverReceive("reply parsing")
		panic("a malformed reply")
	}()
	if n := panics.Load(); n != 1 {
		t.Fatalf("panics counter is %d, want 1 — a contained receive panic that is not counted "+
			"is invisible", n)
	}
}
