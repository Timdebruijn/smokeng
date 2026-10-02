package ingest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newAgent(t *testing.T, id int64, name string) (Agent, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Agent{ID: id, Name: name, PubKey: pub, Enabled: true}, priv
}

// signedRequest builds a request the way an agent would.
func signedRequest(t *testing.T, method, path string, id int64, key ed25519.PrivateKey,
	body []byte, now time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if err := Sign(r, id, key, body, now); err != nil {
		t.Fatal(err)
	}
	// httptest.NewRequest leaves Body readable; Parse consumes it.
	r.Body = http.NoBody
	r.Body = readCloser(body)
	return r
}

func readCloser(b []byte) *nopCloser { return &nopCloser{Reader: bytes.NewReader(b)} }

type nopCloser struct{ *bytes.Reader }

func (n *nopCloser) Close() error { return nil }

func verifierFor(agents ...Agent) *Verifier {
	byID := map[int64]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	return &Verifier{Lookup: func(id int64) (Agent, bool) {
		a, ok := byID[id]
		return a, ok
	}}
}

func TestValidRequestPasses(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	now := time.Unix(1_756_400_000, 0)
	body := []byte("measurement batch")
	r := signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body, now)

	parsed, err := Parse(r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifierFor(agent).Check(parsed, now)
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if got.Name != "ams" {
		t.Errorf("agent = %q, want ams", got.Name)
	}
	if !bytes.Equal(parsed.Body, body) {
		t.Error("body did not survive parsing")
	}
}

// Each of these is a way in if the check is missing, so each gets its own case.
func TestForgeryIsRejected(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	other, otherKey := newAgent(t, 8, "rtm")
	disabled, disabledKey := newAgent(t, 9, "old")
	disabled.Enabled = false
	now := time.Unix(1_756_400_000, 0)
	body := []byte("batch")

	cases := map[string]struct {
		build func() Signed
		now   time.Time
	}{
		"unknown agent": {func() Signed {
			s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", 999, key, body, now))
			return s
		}, now},
		"disabled agent": {func() Signed {
			return parse(t, signedRequest(t, "POST", "/api/v1/ingest", disabled.ID, disabledKey, body, now))
		}, now},
		"signed by another agent's key": {func() Signed {
			s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, otherKey, body, now))
			return s
		}, now},
		"tampered body": {func() Signed {
			s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body, now))
			s.Body = []byte("different batch")
			return s
		}, now},
		"replayed against another path": {func() Signed {
			s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body, now))
			s.Path = "/api/v1/agent/targets"
			return s
		}, now},
		"replayed with another method": {func() Signed {
			s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body, now))
			s.Method = "GET"
			return s
		}, now},
		"timestamp too old": {func() Signed {
			return parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body,
				now.Add(-MaxSkew-time.Minute)))
		}, now},
		"timestamp in the future": {func() Signed {
			return parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body,
				now.Add(MaxSkew+time.Minute)))
		}, now},
		"claimed agent id swapped": {func() Signed {
			s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body, now))
			s.AgentID = other.ID
			return s
		}, now},
		"empty signature": {func() Signed {
			s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body, now))
			s.Signature = nil
			return s
		}, now},
	}
	for name, c := range cases {
		v := verifierFor(agent, other, disabled)
		if _, err := v.Check(c.build(), c.now); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, ErrRejected) {
			t.Errorf("%s: error does not wrap ErrRejected: %v", name, err)
		}
	}
}

func parse(t *testing.T, r *http.Request) Signed {
	t.Helper()
	s, err := Parse(r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A captured request replayed verbatim is refused while the nonce is
// remembered. Idempotent storage is the real defense — this only keeps the
// logs quiet and the work cheap — but within the window it should hold.
func TestNonceBlocksReplay(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	now := time.Unix(1_756_400_000, 0)
	body := []byte("batch")
	r := signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body, now)
	s := parse(t, r)

	v := verifierFor(agent)
	if _, err := v.Check(s, now); err != nil {
		t.Fatalf("first submission rejected: %v", err)
	}
	if _, err := v.Check(s, now); err == nil {
		t.Error("replay within the window was accepted")
	}
	// Once the nonce has expired the timestamp is out of range too, so the
	// request is still refused — the two windows are deliberately ordered
	// that way.
	if _, err := v.Check(s, now.Add(NonceTTL+time.Minute)); err == nil {
		t.Error("replay after nonce expiry was accepted")
	}
}

func TestRateLimit(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	now := time.Unix(1_756_400_000, 0)
	v := verifierFor(agent)

	// A burst is allowed; sustained flooding is not.
	accepted := 0
	for range burstSize * 3 {
		s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, []byte("b"), now))
		if _, err := v.Check(s, now); err == nil {
			accepted++
		}
	}
	if accepted != burstSize {
		t.Errorf("accepted %d requests in an instant, want the burst size %d", accepted, burstSize)
	}

	// Tokens come back with time, so a well-behaved agent is never stuck.
	s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, []byte("b"),
		now.Add(time.Minute)))
	if _, err := v.Check(s, now.Add(time.Minute)); err != nil {
		t.Errorf("still limited after a minute of quiet: %v", err)
	}
}

// The canonical string is the contract between the two implementations. If it
// ever changes shape, every deployed agent stops authenticating, so its exact
// bytes are pinned here.
func TestCanonicalStringIsStable(t *testing.T) {
	got := CanonicalString("POST", "/api/v1/ingest", 7, 1756400000, "bm9uY2U=", []byte("body"))
	want := "smokeng-ingest-v1\nPOST\n/api/v1/ingest\n7\n1756400000\nbm9uY2U=\n" +
		"230d8358dc8e8890b4c58deeb62912ee2f20357ae92a5cc861b98e68fe31acb5"
	if got != want {
		t.Errorf("canonical string changed:\n got %q\nwant %q", got, want)
	}
}

func TestParseRejectsMalformedHeaders(t *testing.T) {
	body := []byte("b")
	cases := map[string]map[string]string{
		"no headers at all": {},
		"bad agent id":      {HeaderID: "not-a-number"},
		"bad timestamp":     {HeaderID: "1", HeaderTime: "soon"},
		"missing nonce":     {HeaderID: "1", HeaderTime: "1756400000"},
		"bad signature":     {HeaderID: "1", HeaderTime: "1756400000", HeaderNonce: "n", HeaderSig: "!!!"},
	}
	for name, headers := range cases {
		r := httptest.NewRequest("POST", "/api/v1/ingest", bytes.NewReader(body))
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		if _, err := Parse(r, 1<<20); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// An oversized body must be refused rather than read into memory.
func TestParseCapsBodySize(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	body := bytes.Repeat([]byte("x"), 4096)
	r := signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, body, time.Now())
	if _, err := Parse(r, 1024); err == nil {
		t.Error("accepted a body over the cap")
	}
}

// M1 regression: an unauthenticated flood carrying a real agent's id but a
// signature that does not verify must not spend that agent's rate budget or
// burn its nonces. Before the fix, the per-agent token bucket was drained
// before verification, so a stranger who guessed the id could silently reject
// the genuine agent's signed submissions.
func TestUnsignedFloodDoesNotStarveTheAgent(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	_, wrongKey := newAgent(t, 8, "attacker") // valid key, wrong agent
	now := time.Unix(1_756_400_000, 0)
	v := verifierFor(agent)

	// Flood far past the bucket size with junk signed by the wrong key but
	// carrying the victim's id. Every one must be rejected...
	for range burstSize * 5 {
		s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, wrongKey, []byte("junk"), now))
		if _, err := v.Check(s, now); err == nil {
			t.Fatal("a request with a non-verifying signature was accepted")
		}
	}

	// ...and the genuine agent, signing correctly, must still get through — its
	// bucket was never touched by the flood.
	s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, []byte("real"), now))
	if _, err := v.Check(s, now); err != nil {
		t.Fatalf("the genuine agent was rejected after an unauthenticated flood: %v", err)
	}
}

// A request that is rate limited must not leave its nonce behind. The nonce
// used to be stored before the limiter was asked, so an agent past its budget
// still filled the cache with everything it sent — and a nonce is whatever
// string the sender chose, up to the header limit.
func TestRateLimitedRequestsDoNotFillTheNonceCache(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	now := time.Unix(1_756_400_000, 0)
	v := verifierFor(agent)
	for range burstSize * 10 {
		s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, []byte("b"), now))
		_, _ = v.Check(s, now)
	}
	if got := len(v.nonces.seen); got != burstSize {
		t.Errorf("the cache holds %d nonces after %d requests, want only the %d that were admitted",
			got, burstSize*10, burstSize)
	}
}

// Guards against putting the limiter before the replay check. (The code before
// this change already refused a replay first; this is not a regression test of
// it.) A replay verifies a second time, because it is a
// genuine signed request, so it has to be refused before it touches the rate
// budget. Otherwise anyone who has seen one valid request can drain that
// agent's bucket by repeating it.
func TestReplayDoesNotSpendTheRateBudget(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	now := time.Unix(1_756_400_000, 0)
	v := verifierFor(agent)
	s := parse(t, signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, []byte("b"), now))
	if _, err := v.Check(s, now); err != nil {
		t.Fatal(err)
	}
	before := v.limits.buckets[agent.ID].tokens
	for range 100 {
		if _, err := v.Check(s, now); err == nil {
			t.Fatal("a replay was accepted")
		}
	}
	if after := v.limits.buckets[agent.ID].tokens; after != before {
		t.Errorf("replays spent rate budget: %.1f tokens left, was %.1f", after, before)
	}
}

// A nonce is exactly nonceBytes of base64, which is what Sign produces and has
// since the header was introduced. Anything else is refused at Parse, because
// the master holds an accepted nonce for ten minutes and its size is what that
// costs.
func TestParseRejectsMalformedNonces(t *testing.T) {
	agent, key := newAgent(t, 7, "ams")
	now := time.Unix(1_756_400_000, 0)
	enc := base64.StdEncoding.EncodeToString
	cases := map[string]struct {
		nonce string
		ok    bool
	}{
		"a real one":          {enc(make([]byte, nonceBytes)), true},
		"one byte short":      {enc(make([]byte, nonceBytes-1)), false},
		"one byte long":       {enc(make([]byte, nonceBytes+1)), false},
		"not base64":          {"not base64 at all!!", false},
		"a megabyte":          {strings.Repeat("A", 1<<20), false},
		"url-safe alphabet":   {strings.NewReplacer("+", "-", "/", "_").Replace(enc(bytes.Repeat([]byte{0xFB, 0xFF}, 8))), false},
		"unpadded base64 (!)": {strings.TrimRight(enc(make([]byte, nonceBytes)), "="), false},
	}
	for name, c := range cases {
		r := signedRequest(t, "POST", "/api/v1/ingest", agent.ID, key, []byte("b"), now)
		r.Header.Set(HeaderNonce, c.nonce)
		_, err := Parse(r, 1<<20)
		if (err == nil) != c.ok {
			t.Errorf("%s: Parse err = %v, want ok=%v", name, err, c.ok)
		}
	}
}

// Expired nonces are dropped on a timer. "Sweep when the map is big" meant a
// walk of the whole map under the lock on every request for as long as the map
// stayed big, which is exactly when nothing in it had expired to delete.
func TestNonceSweepRunsOnATimerNotOnEveryInsert(t *testing.T) {
	var c nonceCache
	t0 := time.Unix(1_756_400_000, 0)
	ok := func() bool { return true }
	stale := func(n int, at time.Time) {
		for i := range n {
			c.seen[strings.Repeat("s", 1+i%7)+string(rune('a'+i%26))+time.Duration(i).String()] = at
		}
	}
	c.admit("first", t0, ok)
	stale(5000, t0)

	// A sweep is due: everything past its TTL goes.
	later := t0.Add(NonceTTL + time.Second)
	c.admit("second", later, ok)
	if n := len(c.seen); n != 1 {
		t.Fatalf("after the sweep the cache holds %d entries, want only the fresh one", n)
	}

	// One second after that no sweep is due, so stale entries are left alone
	// and the insert stays cheap.
	stale(5000, t0)
	c.admit("third", later.Add(time.Second), ok)
	if n := len(c.seen); n < 5000 {
		t.Errorf("the cache was swept again within %s (%d entries left)", nonceSweepEvery, n)
	}
}
