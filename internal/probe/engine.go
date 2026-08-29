package probe

import (
	"context"
	"errors"
	"log"
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

	traceUnsupported sync.Once

	mu       sync.Mutex
	conns    map[connKey]*conn
	running  map[int64]*runningTarget
	targetWG sync.WaitGroup
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
		specs[n.ID] = TargetSpec{
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
		}
	}
	return specs, nil
}

func (e *Engine) connFor(family string, dscp int) (*conn, error) {
	key := connKey{family, dscp}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.conns[key]; ok {
		return c, nil
	}
	c, err := openConn(family, dscp, &e.late)
	if err != nil {
		return nil, err
	}
	e.conns[key] = c
	return c, nil
}

// runTarget is the per-target loop: one iteration per interval bucket.
func (e *Engine) runTarget(ctx context.Context, spec TargetSpec) {
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

		c, err := e.connFor(spec.Family, spec.DSCP)
		if err != nil {
			log.Printf("probe: target %d: %v", spec.TargetID, err)
			if !sleepUntil(ctx, time.Unix(bucket+int64(spec.IntervalS), 0)) {
				return
			}
			continue
		}

		col := newCollector(spec.Pings, &e.late)
		dropsBefore := c.drops()
		aborted := false
		for i, at := range times {
			if !sleepUntil(ctx, at) {
				aborted = true
				break
			}
			if err := c.send(col, i, addr, spec.PacketSize); err != nil {
				log.Printf("probe: target %d: send: %v", spec.TargetID, err)
			}
		}

		// Finalize asynchronously: the finalization wait (bucket end + timeout)
		// overlaps the next bucket's send window, so awaiting it inline would
		// skip buckets whenever the phase offset is shorter than the timeout.
		finalizeAt := time.Unix(bucket, 0).Add(interval).Add(timeout)
		e.targetWG.Add(1)
		go func(col *collector, bucket int64) {
			defer e.targetWG.Done()
			// On shutdown this returns early: the bucket finalizes with
			// whatever was measured, and the writer still flushes it. What it
			// must not do is call the probes that were still in flight lost,
			// so the truncation is passed through and finalize excludes them.
			full := sleepUntil(ctx, finalizeAt)
			// Drops are counted per socket, not per target, so any target
			// sharing this socket during the overflow is suspect: its loss
			// may be ours rather than the network's.
			dropped := c.drops() != dropsBefore
			if dropped {
				e.overflows.Add(1)
			}
			m := col.finalize(spec, bucket, conditions{
				rawSocket: c.raw, overflowed: dropped, truncated: !full,
			})
			c.forget(col)
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
		if err := e.st.WriteMeasurements(wctx, batch); err != nil {
			log.Printf("probe: write %d measurements: %v", len(batch), err)
			e.writeErrs.Add(1)
		} else {
			e.written.Add(int64(len(batch)))
		}
		// Evaluate only what was stored, and only after storing it: an alert
		// that outlives its evidence would be unexplainable.
		if e.alerter != nil {
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
