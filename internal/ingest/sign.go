// Package ingest implements the signed agent protocol (DESIGN.md §9): remote
// agents push measurement batches to the master and pull their assignments,
// authenticated by an Ed25519 signature over a canonical request string.
//
// The master holds only public keys, so compromising its database does not
// let an attacker impersonate an agent — the reason this is signatures rather
// than a shared-secret HMAC, at the same complexity.
//
// Both sides build the signing input with CanonicalString. That is deliberate:
// two implementations of the same format drift, and a drift here reads as an
// authentication failure that nobody can explain.
package ingest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol constants. The domain string is versioned: changing the canonical
// format means changing this, so an old signature cannot be replayed against
// a new interpretation of the same bytes.
const (
	Domain      = "smokeng-ingest-v1"
	HeaderID    = "X-Agent-Id"
	HeaderTime  = "X-Timestamp"
	HeaderNonce = "X-Nonce"
	HeaderSig   = "X-Signature"

	// MaxSkew is how far a request's timestamp may be from ours.
	MaxSkew = 5 * time.Minute
	// NonceTTL must exceed MaxSkew, or a nonce could expire while its
	// timestamp is still acceptable and the replay would be let through.
	NonceTTL = 10 * time.Minute
)

// CanonicalString builds the exact bytes that get signed. METHOD and PATH are
// included so a captured signature cannot be replayed against a different
// endpoint, and the body hash binds the payload.
func CanonicalString(method, path string, agentID, timestamp int64, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		Domain,
		method,
		path,
		strconv.FormatInt(agentID, 10),
		strconv.FormatInt(timestamp, 10),
		nonce,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// Sign attaches the agent's credentials to an outgoing request.
func Sign(r *http.Request, agentID int64, key ed25519.PrivateKey, body []byte, now time.Time) error {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	nonce := base64.StdEncoding.EncodeToString(raw[:])
	ts := now.Unix()
	sig := ed25519.Sign(key, []byte(CanonicalString(r.Method, r.URL.Path, agentID, ts, nonce, body)))

	r.Header.Set(HeaderID, strconv.FormatInt(agentID, 10))
	r.Header.Set(HeaderTime, strconv.FormatInt(ts, 10))
	r.Header.Set(HeaderNonce, nonce)
	r.Header.Set(HeaderSig, base64.StdEncoding.EncodeToString(sig))
	return nil
}

// Signed is a parsed, not-yet-verified request.
type Signed struct {
	Method, Path string
	AgentID      int64
	Timestamp    int64
	Nonce        string
	Signature    []byte
	Body         []byte
}

// Parse pulls the credentials and body off a request. It does not verify
// anything: the body is attacker-controlled until Verifier.Check says
// otherwise.
func Parse(r *http.Request, maxBody int64) (Signed, error) {
	var s Signed
	s.Method, s.Path = r.Method, r.URL.Path

	id, err := strconv.ParseInt(r.Header.Get(HeaderID), 10, 64)
	if err != nil {
		return s, fmt.Errorf("bad %s", HeaderID)
	}
	s.AgentID = id
	if s.Timestamp, err = strconv.ParseInt(r.Header.Get(HeaderTime), 10, 64); err != nil {
		return s, fmt.Errorf("bad %s", HeaderTime)
	}
	s.Nonce = r.Header.Get(HeaderNonce)
	if s.Nonce == "" {
		return s, fmt.Errorf("missing %s", HeaderNonce)
	}
	if s.Signature, err = base64.StdEncoding.DecodeString(r.Header.Get(HeaderSig)); err != nil {
		return s, fmt.Errorf("bad %s", HeaderSig)
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return s, fmt.Errorf("reading body: %w", err)
	}
	s.Body = body
	return s, nil
}

// Agent is what the verifier needs to know about an enrolled agent.
type Agent struct {
	ID      int64
	Name    string
	PubKey  ed25519.PublicKey
	Enabled bool
}

// Verifier checks signed requests. It is safe for concurrent use.
type Verifier struct {
	// Lookup returns the enrolled agent, if any.
	Lookup   func(id int64) (Agent, bool)
	nonces   nonceCache
	limits   rateLimiters
	accepted atomic.Int64
	rejected atomic.Int64
}

// Stats reports how ingest is faring. Rejections are counted without a
// breakdown by reason: the log carries that, and a label per reason would let
// anyone who can reach /metrics probe which check they are failing.
func (v *Verifier) Stats() (accepted, rejected int64) {
	return v.accepted.Load(), v.rejected.Load()
}

// ErrRejected is what callers should return to the network. The specific
// reason is wrapped for the log but must not reach the client: telling an
// attacker which check failed hands them a probe oracle.
var ErrRejected = errors.New("rejected")

// Check verifies a parsed request and returns the agent it belongs to.
// The order is fixed by the design: cheap and non-cryptographic checks first,
// so a flood of junk cannot make the master do signature maths.
func (v *Verifier) Check(s Signed, now time.Time) (Agent, error) {
	agent, err := v.check(s, now)
	if err != nil {
		v.rejected.Add(1)
	} else {
		v.accepted.Add(1)
	}
	return agent, err
}

func (v *Verifier) check(s Signed, now time.Time) (Agent, error) {
	agent, ok := v.Lookup(s.AgentID)
	if !ok {
		return agent, fmt.Errorf("%w: unknown agent %d", ErrRejected, s.AgentID)
	}
	if !agent.Enabled {
		return agent, fmt.Errorf("%w: agent %q is disabled", ErrRejected, agent.Name)
	}
	if !v.limits.allow(s.AgentID, now) {
		return agent, fmt.Errorf("%w: agent %q is over its rate limit", ErrRejected, agent.Name)
	}
	skew := now.Sub(time.Unix(s.Timestamp, 0))
	if skew < -MaxSkew || skew > MaxSkew {
		return agent, fmt.Errorf("%w: agent %q timestamp is %s away (check its clock)",
			ErrRejected, agent.Name, skew.Round(time.Second))
	}
	if !v.nonces.remember(s.Nonce, now) {
		return agent, fmt.Errorf("%w: agent %q reused a nonce", ErrRejected, agent.Name)
	}
	msg := []byte(CanonicalString(s.Method, s.Path, s.AgentID, s.Timestamp, s.Nonce, s.Body))
	if !ed25519.Verify(agent.PubKey, msg, s.Signature) {
		return agent, fmt.Errorf("%w: agent %q signature does not verify", ErrRejected, agent.Name)
	}
	return agent, nil
}

// nonceCache blocks replays within the timestamp window. It is deliberately
// in-memory and therefore empty after a restart: correctness rests on ingest
// being idempotent, not on this. What it buys is cheap rejection and quiet
// logs.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// remember records a nonce, reporting false if it has been seen already.
func (c *nonceCache) remember(nonce string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	// Sweep opportunistically; the map only ever holds one window's worth.
	if len(c.seen) > 4096 {
		for n, at := range c.seen {
			if now.Sub(at) > NonceTTL {
				delete(c.seen, n)
			}
		}
	}
	if at, ok := c.seen[nonce]; ok && now.Sub(at) <= NonceTTL {
		return false
	}
	c.seen[nonce] = now
	return true
}

// rateLimiters caps how often one agent may submit: a token bucket per agent
// id, refilling steadily. An agent reporting every interval needs a handful
// of requests a minute; the cap is far above that and far below a flood.
type rateLimiters struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
}

const (
	burstSize  = 30
	refillRate = 30.0 / 60.0 // tokens per second
)

type bucket struct {
	tokens float64
	last   time.Time
}

func (r *rateLimiters) allow(id int64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buckets == nil {
		r.buckets = map[int64]*bucket{}
	}
	b, ok := r.buckets[id]
	if !ok {
		r.buckets[id] = &bucket{tokens: burstSize - 1, last: now}
		return true
	}
	b.tokens = min(burstSize, b.tokens+now.Sub(b.last).Seconds()*refillRate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
