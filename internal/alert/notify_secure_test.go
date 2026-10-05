package alert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func firing(name string) []Alert {
	return []Alert{{Rule: &Rule{Name: name, Metric: MetricLoss}, TargetPath: "/t", Firing: true}}
}

// A webhook URL is often the credential (a token in the path, a key in the
// query, a user and password before the host). Errors and log lines name where
// alerts go, not how to post to it.
func TestWebhookErrorsDoNotRepeatTheURL(t *testing.T) {
	secrets := []string{"tok-3f9a", "key=abc123", "hunter2"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	url := strings.Replace(srv.URL, "http://", "http://bot:hunter2@", 1) + "/hooks/tok-3f9a?key=abc123"
	w := &Webhook{URL: url}

	err := w.Notify(t.Context(), firing("loss"))
	if err == nil {
		t.Fatal("a 500 was not an error")
	}
	for _, s := range secrets {
		if strings.Contains(err.Error(), s) {
			t.Errorf("status error repeats %q: %v", s, err)
		}
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the status is gone from the error: %v", err)
	}

	srv.Close() // now a connection error, which net/http words with the whole URL
	err = w.Notify(t.Context(), firing("loss"))
	if err == nil {
		t.Fatal("an unreachable receiver was not an error")
	}
	for _, s := range secrets {
		if strings.Contains(err.Error(), s) {
			t.Errorf("connection error repeats %q: %v", s, err)
		}
	}
	if !strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "refused") {
		t.Errorf("the cause is gone from the error: %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://hooks.example.org/services/T0/B0/xyz": "https://hooks.example.org/…",
		"http://am:9093/api/v2/alerts":                 "http://am:9093/…",
		"http://am:9093":                               "http://am:9093",
		"https://u:p@host.example/path?token=1":        "https://host.example/…",
		"https://host.example?token=1":                 "https://host.example/…",
		"::not a url::":                                "(unparseable URL)",
		"":                                             "(unparseable URL)",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

type recorder struct {
	mu      sync.Mutex
	got     []string
	block   chan struct{}
	entered chan struct{} // signalled when a delivery is in flight, if block is set
}

func (r *recorder) Notify(ctx context.Context, a []Alert) error {
	if r.block != nil {
		r.entered <- struct{}{}
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, a[0].Rule.Name)
	return nil
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Alert delivery is called from the loop that writes measurements. A receiver
// that is slow or down must not hold that loop for its timeout, every time.
func TestAQueuedNotifierNeverHoldsItsCaller(t *testing.T) {
	inner := &recorder{block: make(chan struct{}), entered: make(chan struct{}, 8)}
	q := NewQueue(inner, 3)
	defer q.Close(time.Second)
	start := time.Now()
	q.Notify(t.Context(), firing("a"))
	<-inner.entered // "a" is in flight, so the queue itself is empty
	for _, n := range []string{"b", "c", "d", "e", "f", "g"} {
		if err := q.Notify(t.Context(), firing(n)); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("seven Notify calls against a stuck receiver took %v", d)
	}
	close(inner.block)
	// Of what was waiting, a full queue gives up its oldest to make room, so
	// what is current is what gets delivered.
	eventually(t, "the newest batches to be delivered", func() bool { return len(inner.names()) >= 4 })
	time.Sleep(50 * time.Millisecond)
	if got := strings.Join(inner.names(), ""); got != "aefg" {
		t.Errorf("delivered %q, want %q: the one in flight, then the newest three in order", got, "aefg")
	}
}

// A resolved alert is not announced again, so stopping must deliver what is
// queued rather than drop it.
func TestClosingAQueueDeliversWhatIsQueuedInOrder(t *testing.T) {
	inner := &recorder{block: make(chan struct{}), entered: make(chan struct{}, 8)}
	q := NewQueue(inner, 8)
	q.Notify(t.Context(), firing("1"))
	<-inner.entered
	q.Notify(t.Context(), firing("2"))
	q.Notify(t.Context(), firing("3"))
	go func() { time.Sleep(50 * time.Millisecond); close(inner.block) }()
	q.Close(5 * time.Second)
	if got := strings.Join(inner.names(), ""); got != "123" {
		t.Errorf("after Close, delivered %q, want 123", got)
	}
	q.Close(time.Second)                  // twice is fine
	q.Notify(t.Context(), firing("late")) // must not block or panic
}

// What the worker delivers with is its own context, not the one the caller
// passed, which belongs to a write that is about to finish.
func TestAQueuedNotifierDoesNotUseTheCallersContext(t *testing.T) {
	inner := &ctxCheck{seen: make(chan error, 1)}
	q := NewQueue(inner, 2)
	defer q.Close(time.Second)
	callers, cancel := context.WithCancel(t.Context())
	cancel()
	q.Notify(callers, firing("x"))
	select {
	case err := <-inner.seen:
		if err != nil {
			t.Errorf("delivery ran under the caller's cancelled context: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never delivered")
	}
}

type ctxCheck struct{ seen chan error }

func (c *ctxCheck) Notify(ctx context.Context, _ []Alert) error {
	c.seen <- ctx.Err()
	return nil
}

// A receiver that never answers costs the grace, not the delivery timeout.
func TestClosingAQueueCancelsADeliveryThatOutlastsTheGrace(t *testing.T) {
	inner := &blockOnContext{started: make(chan struct{}), done: make(chan struct{})}
	q := NewQueue(inner, 2)
	q.Notify(t.Context(), firing("x"))
	<-inner.started
	start := time.Now()
	q.Close(100 * time.Millisecond)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v against a grace of 100ms", d)
	}
	select {
	case <-inner.done:
	default:
		t.Fatal("the delivery was not cancelled")
	}
}

type blockOnContext struct{ started, done chan struct{} }

func (b *blockOnContext) Notify(ctx context.Context, _ []Alert) error {
	close(b.started)
	<-ctx.Done()
	close(b.done)
	return ctx.Err()
}
