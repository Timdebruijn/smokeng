// Package agent runs smokeng as a remote measurement node (DESIGN.md §9).
//
// An agent pulls its assignments from the master, probes with the same engine
// the master uses locally, buffers results in its own SQLite database, and
// pushes them in signed batches. Buffering is not a special offline mode: the
// agent always writes to its store first and drains from there, so a master
// that is unreachable costs latency, never measurements.
package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timdebruijn/smokeng/internal/ingest"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Config is how an agent is told who it is and where to report.
type Config struct {
	// Master is the base URL, e.g. https://smokeng.example.org.
	Master string
	// Name must match the enrolment on the master; the id comes back from it.
	AgentID  int64
	KeyPath  string
	DBPath   string
	Insecure bool // permit plain HTTP, for local development only
	// Version is reported to the master so an operator can see which agents
	// still predate a fix. It is sent unsigned, on a signed request: it says
	// what this agent claims to be, not what it has proven.
	Version string
}

// Agent is the remote node.
type Agent struct {
	cfg    Config
	key    ed25519.PrivateKey
	st     *store.SQLite
	client *http.Client
}

const (
	pullEvery = 60 * time.Second
	pushEvery = 15 * time.Second
	// pushBatch caps one submission. Well below the master's body limit, and
	// small enough that a long backlog drains in visible steps rather than
	// one enormous request that might time out repeatedly.
	pushBatch = 2000
)

// LoadOrCreateKey reads the agent's private key, generating one on first
// start. The public half is printed so it can be enrolled on the master.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, false, fmt.Errorf("agent: %s does not contain a valid key", path)
		}
		return ed25519.PrivateKey(raw), false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, err
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	// 0600: the private key is the agent's whole identity.
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return nil, false, err
	}
	return priv, true, nil
}

// PublicKey renders a private key's public half for enrolment.
func PublicKey(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

func New(cfg Config, key ed25519.PrivateKey, st *store.SQLite) (*Agent, error) {
	// Plain HTTP to a loopback master needs no flag. The traffic never reaches
	// a network interface, so there is nothing for TLS to protect it from, and
	// demanding the flag anyway made the ordinary "prober as its own process
	// on this host" arrangement look like a compromise it is not — while
	// teaching operators to reach for --insecure-allow-http by reflex, which
	// is the opposite of what the flag is for.
	if !strings.HasPrefix(cfg.Master, "https://") && !cfg.Insecure && !masterIsLoopback(cfg.Master) {
		return nil, fmt.Errorf("agent: master URL %q is neither HTTPS nor on loopback; "+
			"pass --insecure-allow-http only for local development", cfg.Master)
	}
	return &Agent{
		cfg:    cfg,
		key:    key,
		st:     st,
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Run pulls assignments and pushes results until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.pull(ctx); err != nil {
		// A failed first pull is not fatal: the master may simply be down,
		// and the agent should keep trying rather than exit.
		log.Printf("agent: initial pull failed: %v", err)
	}
	pull := time.NewTicker(pullEvery)
	defer pull.Stop()
	push := time.NewTicker(pushEvery)
	defer push.Stop()
	for {
		select {
		case <-ctx.Done():
			// The final drain is the caller's to make, once the probing
			// engine has flushed its last batch into the buffer. Draining
			// here would run first and strand those measurements until the
			// next start.
			return ctx.Err()
		case <-pull.C:
			if err := a.pull(ctx); err != nil {
				log.Printf("agent: pull assignments: %v", err)
			}
		case <-push.C:
			a.push(ctx)
		}
	}
}

// assignment is one target as the master resolved it.
type assignment struct {
	TargetID      int64  `json:"target_id"`
	Path          string `json:"path"`
	Host          string `json:"host"`
	AddressFamily string `json:"address_family"`
	IntervalS     int    `json:"interval_s"`
	Pings         int    `json:"pings"`
	ProbeMode     string `json:"probe_mode"`
	BurstGapMS    int    `json:"burst_gap_ms"`
	TimeoutMS     int    `json:"timeout_ms"`
	PacketSize    int    `json:"packet_size"`
	DSCP          int    `json:"dscp"`
	ProbeType     string `json:"probe_type"`
	ProbePort     int    `json:"probe_port"`
	DNSQuery      string `json:"dns_query"`
	DNSRRType     string `json:"dns_rr_type"`
	HTTPPath      string `json:"http_path"`
	TLSSkipVerify bool   `json:"tls_skip_verify"`
}

// pull fetches the agent's assignments and mirrors them into the local store,
// which is what the probing engine reads. The master stays the single source
// of truth; this database is a cache of its decisions, plus a buffer for
// results.
func (a *Agent) pull(ctx context.Context) error {
	req, err := a.signedRequest(ctx, http.MethodGet, "/api/v1/agent/targets", nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent: master answered %s", resp.Status)
	}
	var body struct {
		Targets []assignment `json:"targets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	return a.mirror(ctx, body.Targets)
}

// mirror writes the assignments into the local tree, keeping target ids
// identical to the master's so measurements line up on arrival.
func (a *Agent) mirror(ctx context.Context, targets []assignment) error {
	existing, err := a.st.ListTargets(ctx)
	if err != nil {
		return err
	}
	wanted := map[int64]bool{}
	for _, t := range targets {
		wanted[t.TargetID] = true
		node := tree.Target{
			ID:            t.TargetID,
			ParentID:      ptr(int64(1)),
			Name:          strings.ReplaceAll(strings.TrimPrefix(t.Path, "/"), "/", "-"),
			Host:          &t.Host,
			AddressFamily: &t.AddressFamily,
			Enabled:       true,
			// The master already resolved inheritance, so every value is set
			// locally here: an agent must not re-derive settings, or the two
			// could disagree about what was measured.
			Settings: tree.Settings{
				IntervalS: &t.IntervalS, PingsPerInterval: &t.Pings,
				ProbeMode: &t.ProbeMode, BurstGapMS: &t.BurstGapMS,
				TimeoutMS: &t.TimeoutMS, PacketSize: &t.PacketSize, DSCP: &t.DSCP,
				Agents: ptr("local"),
			},
		}
		// The probe-type settings are mirrored only where the master actually
		// set one. An empty string is not "no opinion" to tree.Validate — it
		// is an invalid value, and writing it would make every assignment fail
		// validation on arrival and leave the agent measuring nothing at all.
		if t.ProbeType != "" {
			node.Settings.ProbeType = &t.ProbeType
		}
		if t.ProbePort != 0 {
			node.Settings.ProbePort = &t.ProbePort
		}
		if t.DNSQuery != "" {
			node.Settings.DNSQuery = &t.DNSQuery
		}
		if t.DNSRRType != "" {
			node.Settings.DNSRRType = &t.DNSRRType
		}
		if t.HTTPPath != "" {
			node.Settings.HTTPPath = &t.HTTPPath
		}
		// Unlike the others this is set unconditionally: false is a real
		// value here, not "no opinion", and leaving it unset would make the
		// agent's own root default decide whether a certificate is checked.
		node.Settings.TLSSkipVerify = &t.TLSSkipVerify
		if err := a.st.UpsertTarget(ctx, &node); err != nil {
			return fmt.Errorf("agent: mirror %s: %w", t.Path, err)
		}
	}
	// Anything no longer assigned stops being probed. Its buffered results
	// are kept: they were valid when taken and the master still wants them.
	for _, n := range existing {
		if n.ParentID == nil || n.Host == nil || wanted[n.ID] || !n.Enabled {
			continue
		}
		n.Enabled = false
		if err := a.st.UpsertTarget(ctx, &n); err != nil {
			return err
		}
	}
	return nil
}

// Drain pushes whatever is still buffered. Call it after the probing engine
// has stopped, so a clean shutdown leaves nothing behind.
func (a *Agent) Drain(ctx context.Context) { a.push(ctx) }

// push drains buffered measurements to the master, oldest first, and deletes
// only what the master confirmed.
func (a *Agent) push(ctx context.Context) {
	for {
		batch, err := a.st.PendingMeasurements(ctx, pushBatch)
		if err != nil {
			log.Printf("agent: read buffer: %v", err)
			return
		}
		if len(batch) == 0 {
			return
		}
		body, err := ingest.EncodeBatch(batch)
		if err != nil {
			log.Printf("agent: encode batch: %v", err)
			return
		}
		req, err := a.signedRequest(ctx, http.MethodPost, "/api/v1/ingest", body)
		if err != nil {
			log.Printf("agent: sign batch: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/vnd.apache.arrow.stream")
		resp, err := a.client.Do(req)
		if err != nil {
			log.Printf("agent: push: %v (keeping %d measurements buffered)", err, len(batch))
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("agent: master rejected the batch with %s (keeping %d buffered)",
				resp.Status, len(batch))
			return
		}
		// Only now is it safe to forget them.
		if err := a.st.DropSubmitted(ctx, batch); err != nil {
			log.Printf("agent: clearing the buffer: %v", err)
			return
		}
		log.Printf("agent: submitted %d measurements", len(batch))
		if len(batch) < pushBatch {
			return
		}
	}
}

func (a *Agent) signedRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(a.cfg.Master, "/")+path,
		bytesReader(body))
	if err != nil {
		return nil, err
	}
	if err := ingest.Sign(req, a.cfg.AgentID, a.key, body, time.Now()); err != nil {
		return nil, err
	}
	if a.cfg.Version != "" {
		req.Header.Set("X-Agent-Version", a.cfg.Version)
	}
	return req, nil
}

func ptr[T any](v T) *T { return &v }

// bytesReader avoids sending a typed-nil body for GET requests, which would
// make net/http advertise a zero-length body where none is intended.
func bytesReader(b []byte) io.Reader {
	if len(b) == 0 {
		return nil
	}
	return bytes.NewReader(b)
}

// masterIsLoopback reports whether the master URL names this host.
//
// Only a literal loopback address counts, not a name that resolves to one:
// "localhost" is usually 127.0.0.1 and occasionally whatever a compromised
// resolver says, and the point of the exemption is that the packets provably
// cannot leave the machine.
func masterIsLoopback(master string) bool {
	u, err := url.Parse(master)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}
