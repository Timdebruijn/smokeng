package probe

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/probe/dnscache"
	"github.com/timdebruijn/smokeng/internal/probe/trace"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

var errShuttingDown = errors.New("probe: engine is shutting down")

const (
	specRefresh    = 30 * time.Second
	writerFlush    = time.Second
	writerMaxBatch = 512 // measurements per transaction at most
	// Alertmanager expires an alert it stops hearing about, so a firing alert
	// is repeated rather than announced once.
	alertRepeat = time.Minute
)

// Alerter consumes finalized measurements and delivers alert notifications.
// The engine depends on the behaviour, not the implementation, so probing
// keeps working when alerting is not configured.
type Alerter interface {
	Reload(ctx context.Context) error
	Observe(ctx context.Context, ms []alert.Input)
	Repeat(ctx context.Context)
}

// Engine runs the local agent: it schedules every enabled leaf target whose
// effective agent list includes "local", probes it per bucket, and batches
// finalized measurements into the store (single writer, DESIGN.md §6).
type Engine struct {
	st      store.Store
	dns     *dnscache.Cache
	alerter Alerter

	results   chan store.Measurement
	late      atomic.Int64 // replies that arrived after their bucket finalized
	overflows atomic.Int64 // measurements taken while the receive queue dropped replies
	written   atomic.Int64
	writeErrs atomic.Int64
	dnsErrs   atomic.Int64
	traces    atomic.Int64
	pathHops  atomic.Int64
	dropped   atomic.Int64 // measurements lost because the results channel was full
	panics    atomic.Int64 // panics contained rather than allowed to end the process

	traceUnsupported sync.Once

	mu       sync.Mutex
	conns    map[connKey]*conn
	running  map[int64]*runningTarget
	targetWG sync.WaitGroup
	// warned remembers the last configuration problem reported per target, so
	// a misconfiguration is named once instead of twice a minute forever.
	warned map[int64]string
	// shuttingDown stops connFor opening a socket the teardown snapshot has
	// already moved past, which would leak it.
	shuttingDown bool
}

type connKey struct {
	family string
	dscp   int
}

type runningTarget struct {
	spec   TargetSpec
	cancel context.CancelFunc
}

// NewEngine builds the local prober. alerter may be nil, in which case
// measurements are still taken and stored, just never evaluated.
func NewEngine(st store.Store, alerter Alerter) (*Engine, error) {
	dns, err := dnscache.New()
	if err != nil {
		return nil, err
	}
	return &Engine{
		st:      st,
		dns:     dns,
		alerter: alerter,
		results: make(chan store.Measurement, 1024),
		conns:   map[connKey]*conn{},
		running: map[int64]*runningTarget{},
		warned:  map[int64]string{},
	}, nil
}

// Run blocks until ctx is cancelled. Target specs are re-read from the store
// periodically, so TOML imports (and later the admin UI) take effect without
// a restart.
func (e *Engine) Run(ctx context.Context) error {
	stopWriter := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		e.writer(stopWriter)
	}()

	e.reload(ctx)
	tick := time.NewTicker(specRefresh)
	defer tick.Stop()
	repeat := time.NewTicker(alertRepeat)
	defer repeat.Stop()
	for {
		select {
		case <-ctx.Done():
			// Orderly teardown: stop target loops first (so nothing sends on
			// a closing socket), then the sockets, then the writer — which
			// drains and flushes every measurement the loops produced.
			e.mu.Lock()
			e.shuttingDown = true
			for _, rt := range e.running {
				rt.cancel()
			}
			conns := e.conns
			e.conns = map[connKey]*conn{}
			e.mu.Unlock()
			e.targetWG.Wait()
			for _, c := range conns {
				c.close()
			}
			close(stopWriter)
			<-writerDone
			return ctx.Err()
		case <-tick.C:
			e.reload(ctx)
		case <-repeat.C:
			if e.alerter != nil {
				e.alerter.Repeat(ctx)
			}
		}
	}
}

// reload diffs the desired spec set against what is running and starts/stops
// per-target loops accordingly.
func (e *Engine) reload(ctx context.Context) {
	specs, err := e.loadSpecs(ctx)
	if err != nil {
		log.Printf("probe: reload targets: %v", err)
		return
	}
	// Rules and their inheritance follow the same tree, so they are refreshed
	// on the same tick: an edit to either takes effect without a restart.
	if e.alerter != nil {
		if err := e.alerter.Reload(ctx); err != nil {
			log.Printf("alert: reload rules: %v", err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, rt := range e.running {
		if spec, ok := specs[id]; !ok || spec != rt.spec {
			rt.cancel()
			delete(e.running, id)
		}
	}
	for id, spec := range specs {
		if _, ok := e.running[id]; ok {
			continue
		}
		tctx, cancel := context.WithCancel(ctx)
		e.running[id] = &runningTarget{spec: spec, cancel: cancel}
		e.targetWG.Add(1)
		go func(spec TargetSpec) {
			defer e.targetWG.Done()
			e.runTarget(tctx, spec)
		}(spec)
		if spec.TraceIntervalS > 0 {
			e.targetWG.Add(1)
			go func(spec TargetSpec) {
				defer e.targetWG.Done()
				e.runTracer(tctx, spec)
			}(spec)
		}
	}
}

func (e *Engine) loadSpecs(ctx context.Context) (map[int64]TargetSpec, error) {
	targets, err := e.st.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := tree.New(targets)
	if err != nil {
		return nil, err
	}
	specs := map[int64]TargetSpec{}
	problems := map[int64]string{}
	for i := range targets {
		n := &targets[i]
		if n.Host == nil || !n.Enabled {
			continue
		}
		res, err := tr.Resolve(n.ID)
		if err != nil {
			return nil, err
		}
		local := false
		for _, a := range strings.Fields(res.Agents.Effective) {
			if a == "local" {
				local = true
			}
		}
		if !local {
			continue
		}
		spec := TargetSpec{
			TargetID:       n.ID,
			Host:           *n.Host,
			Family:         *n.AddressFamily,
			IntervalS:      res.IntervalS.Effective,
			Pings:          res.PingsPerInterval.Effective,
			Mode:           res.ProbeMode.Effective,
			BurstGapMS:     res.BurstGapMS.Effective,
			TimeoutMS:      res.TimeoutMS.Effective,
			PacketSize:     res.PacketSize.Effective,
			DSCP:           res.DSCP.Effective,
			TraceIntervalS: res.TraceIntervalS.Effective,
			ProbeType:      res.ProbeType.Effective,
			ProbePort:      res.ProbePort.Effective,
			DNSQuery:       res.DNSQuery.Effective,
			DNSRRType:      res.DNSRRType.Effective,
			HTTPPath:       res.HTTPPath.Effective,
			TLSSkipVerify:  res.TLSSkipVerify.Effective,
		}
		// A target that cannot be measured as configured is skipped and said
		// out loud. Probing it anyway would mean guessing at the missing
		// piece, and a graph drawn from a guess is worse than no graph.
		if err := validateSpec(spec); err != nil {
			problems[n.ID] = err.Error()
			continue
		}
		specs[n.ID] = spec
	}
	e.reportSpecProblems(targets, problems)
	return specs, nil
}

// validateSpec checks the settings a probe type needs but the tree cannot
// enforce. tree.Validate sees one node's own settings; whether a port is set
// is only answerable once inheritance has been resolved, which is here.
func validateSpec(spec TargetSpec) error {
	switch spec.ProbeType {
	case "", "icmp", "dns", "http", "https":
		return nil
	case "tcp":
		if spec.ProbePort == 0 {
			return errors.New("probe_type tcp needs a probe_port, and there is no sensible port to guess")
		}
		return nil
	case "irtt":
		// A zero send interval makes irtt's own config validation reject the
		// session before a single packet leaves — so the target would be
		// scheduled to fail every interval, and finalize would record a
		// healthy server as total loss forever. burst mode with burst_gap_ms=0
		// is the way to reach it (tree.Validate allows a zero gap because it is
		// fine for icmp). Refuse it here, the way a portless tcp target is
		// refused, rather than measure a lie.
		if irttStep(spec) <= 0 {
			return errors.New("probe_type irtt needs a positive send interval; in burst mode " +
				"that means burst_gap_ms > 0")
		}
		return nil
	}
	return fmt.Errorf("probe_type %q has no prober behind it", spec.ProbeType)
}

// reportSpecProblems names each unmeasurable target once, and again only if
// the problem changes. The reload runs twice a minute and a misconfiguration
// lasts until somebody fixes it, so logging every pass would bury the event
// that matters under a thousand copies of itself.
func (e *Engine) reportSpecProblems(targets []tree.Target, problems map[int64]string) {
	host := func(id int64) string {
		for i := range targets {
			if targets[i].ID == id && targets[i].Host != nil {
				return *targets[i].Host
			}
		}
		return "?"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, msg := range problems {
		if e.warned[id] != msg {
			e.warned[id] = msg
			log.Printf("probe: target %d (%s) is not being measured: %s", id, host(id), msg)
		}
	}
	// Forget a target that is healthy again, so a problem that returns is
	// reported again rather than swallowed as already-known.
	for id := range e.warned {
		if _, still := problems[id]; !still {
			delete(e.warned, id)
		}
	}
}

// connFor returns the socket for a (family, DSCP) pair, opening one the first
// time it is asked for.
//
// Sockets are never closed. That is deliberate rather than overlooked: DSCP is
// six bits and there are two families, so the set is bounded at 128 sockets
// however often the tree is edited, and closing one safely would mean tracking
// its lifetime against every finalize goroutine still holding it — those call
// back into the conn after their bucket ends. A bounded 128 is cheaper than
// that machinery and cannot grow into a problem.
func (e *Engine) connFor(family string, dscp int) (*conn, error) {
	key := connKey{family, dscp}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.conns[key]; ok {
		return c, nil
	}
	// Once teardown has snapshotted the conn map, a socket opened here would go
	// into the fresh map the snapshot no longer covers and never be closed — a
	// leak that only bites at shutdown, but a leak. A target goroutine can
	// reach this between its last sleepUntil and returning, so decline rather
	// than open.
	if e.shuttingDown {
		return nil, errShuttingDown
	}
	c, err := openConn(family, dscp, &e.late, &e.panics)
	if err != nil {
		return nil, err
	}
	e.conns[key] = c
	return c, nil
}

// runTarget is the per-target loop: one iteration per interval bucket.
func (e *Engine) runTarget(ctx context.Context, spec TargetSpec) {
	defer e.recoverTarget(spec)
	lastAddr, err := e.st.LastResolution(ctx, spec.TargetID)
	if err != nil {
		log.Printf("probe: target %d: read last resolution: %v", spec.TargetID, err)
	}
	interval := time.Duration(spec.IntervalS) * time.Second
	timeout := time.Duration(spec.TimeoutMS) * time.Millisecond

	for {
		// Pick the next bucket whose first send is still in the future.
		bucket := bucketStart(time.Now().Unix(), spec.IntervalS)
		times := sendTimes(spec, bucket)
		if !time.Now().Before(times[0]) {
			bucket += int64(spec.IntervalS)
			times = sendTimes(spec, bucket)
		}

		if !sleepUntil(ctx, times[0].Add(-10*time.Millisecond)) {
			return
		}

		addr, err := e.dns.Lookup(ctx, spec.Host, spec.Family)
		if err != nil {
			// No address, no measurement: the gap in the data is the honest
			// representation (DESIGN.md §8.2).
			log.Printf("probe: target %d (%s): resolve: %v", spec.TargetID, spec.Host, err)
			e.dnsErrs.Add(1)
			if !sleepUntil(ctx, time.Unix(bucket+int64(spec.IntervalS), 0)) {
				return
			}
			continue
		}
		if a := addr.String(); a != lastAddr {
			if lastAddr != "" {
				log.Printf("probe: target %d (%s): address changed %s -> %s", spec.TargetID, spec.Host, lastAddr, a)
			}
			if err := e.st.RecordResolution(ctx, spec.TargetID, time.Now().Unix(), a); err != nil {
				log.Printf("probe: target %d: record resolution: %v", spec.TargetID, err)
			}
			lastAddr = a
		}

		// Only ICMP needs a shared socket; the other types open their own
		// connection per probe and are timed in userspace.
		var c *conn
		if spec.ProbeType == "" || spec.ProbeType == "icmp" {
			c, err = e.connFor(spec.Family, spec.DSCP)
			if err != nil {
				// Shutdown is the ordinary way to get here, not a fault: the
				// context is already cancelled, so return quietly rather than
				// log a socket error on every target as the process exits.
				if errors.Is(err, errShuttingDown) {
					return
				}
				log.Printf("probe: target %d: %v", spec.TargetID, err)
				if !sleepUntil(ctx, time.Unix(bucket+int64(spec.IntervalS), 0)) {
					return
				}
				continue
			}
		}

		col := newCollector(spec.Pings, &e.late)
		var dropsBefore uint64
		if c != nil {
			dropsBefore = c.drops()
		}
		aborted := false
		var probeWG sync.WaitGroup
		// Finalize asynchronously: the finalization wait (bucket end + timeout)
		// overlaps the next bucket's send window, so awaiting it inline would
		// skip buckets whenever the phase offset is shorter than the timeout.
		finalizeAt := time.Unix(bucket, 0).Add(interval).Add(timeout)

		if spec.ProbeType == "irtt" {
			// One session covers the whole interval: the far end paces the
			// train and reports every packet, so there is nothing to schedule
			// here beyond when it starts.
			if !sleepUntil(ctx, times[0]) {
				aborted = true
			} else {
				probeWG.Add(1)
				go func() {
					defer probeWG.Done()
					defer e.recoverProbe(spec, "irtt session")
					probeIRTT(ctx, col, addr, spec, finalizeAt)
				}()
			}
		} else {
			for i, at := range times {
				if !sleepUntil(ctx, at) {
					aborted = true
					break
				}
				if c != nil {
					if err := c.send(col, i, addr, spec.PacketSize); err != nil {
						log.Printf("probe: target %d: send: %v", spec.TargetID, err)
					}
					continue
				}
				// A userspace probe blocks until it answers or times out, so it
				// runs on its own goroutine: waiting here would push every later
				// probe of the interval out by however long this one took, and
				// the schedule is what the distribution means.
				probeWG.Add(1)
				go func(i int) {
					defer probeWG.Done()
					defer e.recoverProbe(spec, "probe")
					runUserspaceProbe(ctx, col, i, addr, spec)
				}(i)
			}
		}

		e.targetWG.Add(1)
		go func(col *collector, bucket int64) {
			defer e.targetWG.Done()
			defer e.recoverProbe(spec, "finalize")
			// On shutdown this returns early: the bucket finalizes with
			// whatever was measured, and the writer still flushes it. What it
			// must not do is call the probes that were still in flight lost,
			// so the truncation is passed through and finalize excludes them.
			full := sleepUntil(ctx, finalizeAt)
			// Drops are counted per socket, not per target, so any target
			// sharing this socket during the overflow is suspect: its loss
			// may be ours rather than the network's.
			dropped := c != nil && c.drops() != dropsBefore
			if dropped {
				e.overflows.Add(1)
			}
			// Every in-flight userspace probe has to have finished or timed
			// out before the bucket is read, or its reply lands after the
			// snapshot and counts as loss that never happened.
			probeWG.Wait()
			raw := c != nil && c.raw
			m := col.finalize(spec, bucket, conditions{
				rawSocket: raw, overflowed: dropped, truncated: !full,
			})
			if c != nil {
				c.forget(col)
			}
			select {
			case e.results <- m:
			default:
				log.Printf("probe: results backlog full, dropping measurement for target %d", spec.TargetID)
				e.dropped.Add(1)
			}
		}(col, bucket)
		if aborted {
			return
		}
	}
}

// writer is the single store writer: it batches finalized measurements into
// one transaction per flush tick. It runs until stop closes AND the results
// channel is drained, so a shutdown never loses finalized measurements.
func (e *Engine) writer(stop <-chan struct{}) {
	var batch []store.Measurement
	flush := func() {
		if len(batch) == 0 {
			return
		}
		wctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stored := true
		if err := e.st.WriteMeasurements(wctx, batch); err != nil {
			log.Printf("probe: write %d measurements: %v; they are dropped and not alerted on",
				len(batch), err)
			e.writeErrs.Add(1)
			e.dropped.Add(int64(len(batch)))
			stored = false
		} else {
			e.written.Add(int64(len(batch)))
		}
		// Evaluate only what was stored, and only after storing it: an alert
		// that outlives its evidence would be unexplainable. The comment said
		// so before the code did — Observe ran on the failure branch too.
		if e.alerter != nil && stored {
			inputs := make([]alert.Input, len(batch))
			for i := range batch {
				inputs[i] = batch[i].AlertInput()
			}
			e.alerter.Observe(wctx, inputs)
		}
		cancel()
		batch = batch[:0]
	}
	tick := time.NewTicker(writerFlush)
	defer tick.Stop()
	for {
		select {
		case m := <-e.results:
			batch = append(batch, m)
			if len(batch) >= writerMaxBatch {
				flush()
			}
		case <-tick.C:
			flush()
		case <-stop:
			for {
				select {
				case m := <-e.results:
					batch = append(batch, m)
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Stats reports the prober's own health. Everything here is about smokeng,
// never about the network: the measurements themselves live in the store at
// full resolution and are read as Arrow.
type Stats struct {
	// LateReplies arrived after their bucket had been finalized.
	LateReplies int64
	// SocketOverflows counts measurements taken while the kernel was dropping
	// replies for want of receive-queue space — loss that is ours, not the
	// network's. A non-zero value means smokeng is behind, not the link.
	SocketOverflows     int64
	MeasurementsWritten int64
	WriteErrors         int64
	DNSFailures         int64
	// PathChanges counts routes recorded because they differed from the last.
	PathChanges int64
	// Dropped measurements were finalized but never stored, because the
	// writer could not keep up. This should always be zero.
	Dropped int64
	// ActiveTargets is how many target loops are currently running.
	ActiveTargets int64
	// Panics were contained instead of ending the process. Any non-zero value
	// is a bug in smokeng, and the log line next to it has the stack.
	Panics int64
}

func (e *Engine) Stats() Stats {
	e.mu.Lock()
	active := int64(len(e.running))
	e.mu.Unlock()
	return Stats{
		LateReplies:         e.late.Load(),
		SocketOverflows:     e.overflows.Load(),
		MeasurementsWritten: e.written.Load(),
		WriteErrors:         e.writeErrs.Load(),
		DNSFailures:         e.dnsErrs.Load(),
		PathChanges:         e.traces.Load(),
		Dropped:             e.dropped.Load(),
		ActiveTargets:       active,
		Panics:              e.panics.Load(),
	}
}

// runTracer discovers the path to a target periodically and records it only
// when it differs from the last one. It runs on its own schedule, well apart
// from the measurement loop: a traceroute costs a round trip per hop, and a
// route changes on a scale of days rather than seconds.
func (e *Engine) runTracer(ctx context.Context, spec TargetSpec) {
	interval := time.Duration(spec.TraceIntervalS) * time.Second
	// Stagger the first run so a restart does not traceroute every target at
	// once, and so the first measurements are not competing with it.
	if !sleepUntil(ctx, time.Now().Add(phaseOffset(spec)+5*time.Second)) {
		return
	}
	for {
		e.traceOnce(ctx, spec)
		if !sleepUntil(ctx, time.Now().Add(interval)) {
			return
		}
	}
}

func (e *Engine) traceOnce(ctx context.Context, spec TargetSpec) {
	addr, err := e.dns.Lookup(ctx, spec.Host, spec.Family)
	if err != nil {
		return // the measurement loop already logs and counts this
	}
	path, err := trace.Trace(ctx, trace.Options{Dest: addr, Timeout: 2 * time.Second})
	if err != nil {
		// Unsupported is reported once per target rather than every run: it
		// is a property of the platform, not an event.
		if errors.Is(err, trace.ErrUnsupported) {
			e.traceUnsupported.Do(func() {
				log.Printf("probe: path discovery unavailable on this platform; " +
					"no routes will be recorded")
			})
			return
		}
		log.Printf("probe: target %d: trace: %v", spec.TargetID, err)
		return
	}
	if len(path) == 0 {
		return
	}
	hops := path.String()

	last, err := e.st.LastPath(ctx, spec.TargetID, store.LocalAgentID)
	if err != nil {
		log.Printf("probe: target %d: read last path: %v", spec.TargetID, err)
		return
	}
	if last == hops {
		return
	}
	if err := e.st.RecordPath(ctx, spec.TargetID, store.LocalAgentID, time.Now().Unix(), hops); err != nil {
		log.Printf("probe: target %d: record path: %v", spec.TargetID, err)
		return
	}
	e.traces.Add(1)
	if last != "" {
		log.Printf("probe: target %d (%s): path changed\n  was: %s\n  now: %s",
			spec.TargetID, spec.Host, last, hops)
	}
}

// runUserspaceProbe dispatches the probe types that do not share the icmp
// socket. Most of them are timed around a userspace call and finalize flags
// their measurements accordingly; dns is the exception, running on a socket of
// its own so the kernel can stamp it (dnssocket.go). Either way the flags say
// which happened, because a band widened by a busy prober must not read as a
// slow service.
func runUserspaceProbe(ctx context.Context, col *collector, idx int, addr netip.Addr, spec TargetSpec) {
	switch spec.ProbeType {
	case "dns":
		probeDNS(ctx, col, idx, addr, spec)
	case "tcp":
		probeTCP(ctx, col, idx, addr, spec)
	case "http", "https":
		probeHTTP(ctx, col, idx, addr, spec)
	default:
		// validateSpec refuses an unknown type before it can be scheduled, so
		// reaching this means a type was added to the tree's allowed set
		// without a prober behind it. Recording it as a send failure rather
		// than as loss keeps the distinction that matters: nothing was asked
		// of the target, so the target is not what failed.
		col.markSendFailed(idx, store.SendReasonSocket)
	}
}

// recoverProbe contains a panic to the single probe that caused it.
//
// The alternative is what smokeng did until now: one malformed reply, in one
// probe, against one of two hundred targets, ends the process — and takes the
// UI, the API and every other target's measurement with it. A gap in one
// series is the proportionate outcome, and the counter makes the gap
// attributable rather than mysterious.
//
// This is deliberately not a blanket recover. It sits on goroutines that hold
// no shared lock when they run, so recovering cannot leave a mutex held: a
// process that survives a panic into a deadlock is worse off than one that
// crashed, because systemd restarts a crash and cannot see a wedge.
func (e *Engine) recoverProbe(spec TargetSpec, what string) {
	r := recover()
	if r == nil {
		return
	}
	e.panics.Add(1)
	log.Printf("probe: target %d (%s): %s panicked: %v\n%s",
		spec.TargetID, spec.Host, what, r, debug.Stack())
}

// recoverTarget contains a panic to the target loop that caused it, and
// arranges for that target to start measuring again.
//
// Descheduling is the point. Without it the loop would end, e.running would
// still hold the target, and reload would never restart it — the target would
// simply stop being measured, quietly, until somebody noticed the flat line.
func (e *Engine) recoverTarget(spec TargetSpec) {
	r := recover()
	if r == nil {
		return
	}
	e.panics.Add(1)
	log.Printf("probe: target %d (%s): probe loop panicked: %v\n%s",
		spec.TargetID, spec.Host, r, debug.Stack())
	e.mu.Lock()
	if rt, ok := e.running[spec.TargetID]; ok && rt.spec == spec {
		rt.cancel()
		delete(e.running, spec.TargetID)
	}
	e.mu.Unlock()
	log.Printf("probe: target %d will be restarted within %s", spec.TargetID, specRefresh)
}
