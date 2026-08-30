package probe

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync/atomic"
)

// trustedCAs is the certificate pool https probes verify against, or nil to
// use the platform's own.
//
// It is process-wide rather than a per-target setting on purpose. A CA is a
// file on a disk and a property of the deployment, not of one measurement, and
// putting a PEM blob in the target tree would mean shipping certificates
// through the API and inheriting them down the tree — machinery for something
// an operator already knows how to place on a host.
//
// The consequence is worth stating plainly: a remote agent measuring an
// internally-signed target needs the same CA file. The master cannot supply it,
// because trusting whatever the master sends would be a wider trust
// relationship than an agent needs to have.
var trustedCAs atomic.Pointer[x509.CertPool]

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
	if len(paths) == 0 {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("probe: read system certificate pool: %w", err)
	}
	for _, p := range paths {
		pem, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("probe: read CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("probe: %s contains no PEM certificates", p)
		}
	}
	trustedCAs.Store(pool)
	return nil
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
