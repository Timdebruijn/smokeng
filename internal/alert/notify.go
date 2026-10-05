package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Alert is one outgoing notification.
type Alert struct {
	Rule       *Rule
	TargetID   int64
	AgentID    int64
	TargetPath string
	TargetHost string
	AgentName  string
	Firing     bool
	Since      time.Time
	// Ended is when a resolved alert resolved, stamped where the transition
	// happens: delivery may wait in a queue, and a receiver that tracks state
	// takes the end as fact.
	Ended time.Time
	Value float64
	// Acked and its detail describe an acknowledgement: a firing alert a person
	// marked seen so it stops demanding attention, without changing the rule or
	// stopping the alert. It never suppresses delivery — the alert is still
	// firing and downstream still needs to know — only the UI's own attention.
	Acked   bool
	AckedAt time.Time
	AckedBy string
	// Silenced and SilencedUntil describe a silence covering this alert now: it
	// is still firing, but delivery is suppressed and the UI shows it muted
	// until the window closes. Unlike an acknowledgement, a silence does stop
	// delivery — that is the point of a maintenance window.
	Silenced      bool
	SilencedUntil time.Time
}

// Notifier delivers alerts. smokeng ships exactly one implementation, a
// webhook: notification channels are somebody else's job, and Alertmanager
// already does grouping, silencing and routing better than a monitoring tool
// would by reinventing them.
type Notifier interface {
	Notify(ctx context.Context, alerts []Alert) error
}

// Webhook posts alerts in Alertmanager's v2 format, so it can be pointed
// straight at an Alertmanager or at anything that speaks the same shape.
type Webhook struct {
	URL    string
	Client *http.Client
}

// amAlert mirrors Alertmanager's POST /api/v2/alerts payload.
type amAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt,omitempty"`
	EndsAt       string            `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

func (w *Webhook) Notify(ctx context.Context, alerts []Alert) error {
	if len(alerts) == 0 {
		return nil
	}
	payload := make([]amAlert, 0, len(alerts))
	for _, a := range alerts {
		unit := "ms"
		if a.Rule.Metric == MetricLoss {
			unit = "%"
		}
		am := amAlert{
			Labels: map[string]string{
				"alertname": a.Rule.Name,
				"target":    a.TargetPath,
				"host":      a.TargetHost,
				"agent":     a.AgentName,
				"metric":    string(a.Rule.Metric),
				"severity":  "warning",
			},
			Annotations: map[string]string{
				"summary": fmt.Sprintf("%s on %s: %s is %.3g%s",
					a.Rule.Name, a.TargetPath, a.Rule.Metric, a.Value, unit),
				"description": fmt.Sprintf("Rule %q (%s) has been satisfied for %s.",
					a.Rule.Name, a.Rule.Describe(), a.TargetPath),
			},
		}
		if !a.Since.IsZero() {
			am.StartsAt = a.Since.UTC().Format(time.RFC3339)
		}
		// A firing alert carries no end: Alertmanager expires it on its own
		// resolve timeout if we stop repeating it, which is why firing alerts
		// are re-sent periodically rather than announced once.
		if !a.Firing {
			ended := a.Ended
			if ended.IsZero() {
				ended = time.Now()
			}
			am.EndsAt = ended.UTC().Format(time.RFC3339)
		}
		payload = append(payload, am)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http words a failure with the whole URL, and the URL is often the
		// credential: a token in the path, a key in the query, a user and
		// password before the host.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("alert: webhook %s: %w", RedactURL(w.URL), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("alert: webhook %s returned %s", RedactURL(w.URL), resp.Status)
	}
	return nil
}

// RedactURL names where a URL points without what authorises a request to it:
// scheme, host and port, and an ellipsis where a path or query was. For logs
// and errors.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(unparseable URL)"
	}
	out := u.Scheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		out += "/…"
	}
	return out
}

// Queue delivers through another Notifier on its own goroutine. Delivery is
// called from the loop that writes measurements, and a receiver that is slow or
// down held that loop for its whole timeout on every batch.
//
// The queue is bounded. When it is full the oldest batch is dropped to make room
// for the newest, and the drop is logged: what is current matters more than what
// was current a while ago, and a firing alert is announced again on a timer
// (Manager.Repeat), so what is lost is a transition, said so, not a state. A
// resolved alert is not announced again, which is why Close drains what is
// queued instead of discarding it.
type Queue struct {
	inner  Notifier
	ch     chan []Alert
	stop   chan struct{}
	done   chan struct{}
	dctx   context.Context
	cancel context.CancelFunc
	once   sync.Once
}

// queueDeliveryTimeout bounds one delivery, as the webhook's own client does.
const queueDeliveryTimeout = 30 * time.Second

// NewQueue starts the worker. Call Close to stop it.
func NewQueue(inner Notifier, size int) *Queue {
	q := &Queue{inner: inner, ch: make(chan []Alert, size), stop: make(chan struct{}), done: make(chan struct{})}
	q.dctx, q.cancel = context.WithCancel(context.Background())
	go q.run()
	return q
}

func (q *Queue) run() {
	defer close(q.done)
	for {
		select {
		case alerts := <-q.ch:
			q.send(alerts)
		case <-q.stop:
			for {
				select {
				case alerts := <-q.ch:
					q.send(alerts)
				default:
					return
				}
			}
		}
	}
}

func (q *Queue) send(alerts []Alert) {
	// Not the caller's context: it belongs to a write that has finished by the
	// time this runs.
	ctx, cancel := context.WithTimeout(q.dctx, queueDeliveryTimeout)
	defer cancel()
	if err := q.inner.Notify(ctx, alerts); err != nil {
		log.Printf("alert: deliver %d alert(s): %v", len(alerts), err)
	}
}

// Notify queues a batch and returns at once.
func (q *Queue) Notify(_ context.Context, alerts []Alert) error {
	for {
		select {
		case q.ch <- alerts:
			return nil
		default:
		}
		select {
		case old := <-q.ch:
			log.Printf("alert: the delivery queue is full; dropped a batch of %d alert(s), the oldest", len(old))
		default:
		}
	}
}

// Close delivers what is queued, waiting at most grace for it, and stops the
// worker. A delivery still running when the grace is over is cancelled, and what
// is left is dropped with a log line per batch. Batches queued after Close are
// not delivered.
func (q *Queue) Close(grace time.Duration) {
	q.once.Do(func() { close(q.stop) })
	select {
	case <-q.done:
	case <-time.After(grace):
		q.cancel()
		<-q.done
	}
	q.cancel()
}
