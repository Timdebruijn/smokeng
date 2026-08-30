package probe

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
)

// trustedCAs is the certificate pool https probes verify against, or nil to
// use the platform's own.
//
// It is read in exactly one place — tlsConfigFor, for an https probe — and
// deliberately nowhere else. In particular an agent's connection to its master
// does not use it: that client verifies against the host's own trust store,
// which is what keeps "who this instance measures" separate from "who this
// instance believes". There is a test that pins that separation.
var trustedCAs atomic.Pointer[x509.CertPool]

// The pool is built from two independent sources and rebuilt whenever either
// changes, because appending cannot express "the master withdrew a CA".
var (
	caMu      sync.Mutex
	localCAs  [][]byte // from --tls-ca-file on this host
	remoteCAs [][]byte // handed down by the master, for agents
)

// TrustCAFiles adds the PEM certificates in each path to the pool used for
// https probes, on top of the system roots rather than instead of them — a
// smokeng instance usually measures both internal and public endpoints, and
// replacing the pool would break the public ones the moment you added a
// private root.
//
// Each file may hold any number of certificates. A file that parses to none is
// an error rather than a silent no-op: the operator's intent was to trust
// something, and quietly trusting nothing would surface later as an
// unexplained wall of loss.
func TrustCAFiles(paths []string) error {
	var pems [][]byte
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("probe: read CA file: %w", err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(data) {
			return fmt.Errorf("probe: %s contains no PEM certificates", p)
		}
		pems = append(pems, data)
	}
	caMu.Lock()
	defer caMu.Unlock()
	localCAs = pems
	return rebuildLocked()
}

// LocalCAPEMs returns the PEM blocks this instance was given on the command
// line, which is what a master hands down to its agents. It never returns what
// an agent was itself handed: a master relays its own operator's decision, not
// another master's.
func LocalCAPEMs() [][]byte {
	caMu.Lock()
	defer caMu.Unlock()
	return append([][]byte(nil), localCAs...)
}

// TrustRemoteCAPEMs replaces the set an agent was handed by its master.
//
// Replacement rather than accumulation is the point: withdrawing a CA on the
// master has to reach the agents, and a pool that only ever grew would keep
// trusting a root long after it was retired. Anything from --tls-ca-file on
// the agent's own host survives regardless — a local decision is not the
// master's to revoke.
//
// Changes are logged with each certificate's subject and fingerprint. An agent
// being told what to trust is a thing an operator must be able to audit after
// the fact, so it is never silent.
func TrustRemoteCAPEMs(pems [][]byte) error {
	caMu.Lock()
	defer caMu.Unlock()
	if samePEMs(remoteCAs, pems) {
		return nil
	}
	for _, p := range pems {
		for _, c := range describeCerts(p) {
			log.Printf("probe: trusting CA from master: %s", c)
		}
	}
	if len(pems) == 0 && len(remoteCAs) > 0 {
		log.Printf("probe: the master withdrew every CA it had supplied")
	}
	remoteCAs = pems
	return rebuildLocked()
}

// rebuildLocked composes the pool from the system roots plus both sources.
func rebuildLocked() error {
	if len(localCAs) == 0 && len(remoteCAs) == 0 {
		// Nil means "use the platform's own", which is not the same as an
		// empty pool — an empty pool would trust nothing at all.
		trustedCAs.Store(nil)
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("probe: read system certificate pool: %w", err)
	}
	for _, p := range append(append([][]byte(nil), localCAs...), remoteCAs...) {
		// Already validated as parseable when it was accepted; a failure here
		// would mean the stored bytes changed underneath us.
		if !pool.AppendCertsFromPEM(p) {
			return fmt.Errorf("probe: stored CA data holds no PEM certificates")
		}
	}
	trustedCAs.Store(pool)
	return nil
}

func samePEMs(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// describeCerts renders each certificate in a PEM blob as subject plus a
// SHA-256 fingerprint of the DER — enough to recognise it in a log and to
// compare it against what the operator believes they deployed.
func describeCerts(blob []byte) []string {
	var out []string
	for {
		var block *pem.Block
		block, blob = pem.Decode(blob)
		if block == nil {
			return out
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		sum := sha256.Sum256(block.Bytes)
		fp := hex.EncodeToString(sum[:])
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			out = append(out, fmt.Sprintf("%s (sha256:%s)", c.Subject, fp))
			continue
		}
		out = append(out, fmt.Sprintf("unparseable certificate (sha256:%s)", fp))
	}
}

// tlsConfigFor builds the client TLS configuration for one target.
func tlsConfigFor(spec TargetSpec) *tls.Config {
	cfg := &tls.Config{RootCAs: trustedCAs.Load()}
	if spec.TLSSkipVerify {
		// #nosec G402 — deliberate, per-target, and never the default. See
		// the setting's own documentation for why the alternative is worse:
		// an internal service on a private PKI reading as 100% loss is a
		// graph reporting an outage that is not happening.
		cfg.InsecureSkipVerify = true
	}
	return cfg
}
