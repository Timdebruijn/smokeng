package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/timdebruijn/smokeng/internal/store"
)

// connError classifies why a tcp dial or an http(s) request got no answer, as
// a store.ProbeError of kind ProbeErrConn. It is called only for errors that
// are not this host's own (isLocalResourceError), so every outcome here is a
// fact about the far end or the path to it.
//
// TLS comes before the timeout check: a certificate the prober rejects is a
// definite answer, and an error that wraps both is about the certificate.
//
// It returns 0, no reason, when ctx itself is done: the probe was abandoned by
// a shutdown or a settings change (ctx is the target's own context, not the
// probe's timeout), and nothing was learned about the far end. A reason there would also make finalize count a
// probe it otherwise leaves out of a truncated interval.
func connError(ctx context.Context, err error) uint16 {
	if ctx.Err() != nil {
		return 0
	}
	var (
		unknownCA *x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
		verify    *tls.CertificateVerificationError
		record    tls.RecordHeaderError
		alert     tls.AlertError
	)
	var netErr net.Error
	var opErr *net.OpError
	detail := store.ProbeConnOther
	switch {
	case errors.As(err, &verify), errors.As(err, &unknownCA), errors.As(err, &hostname),
		errors.As(err, &invalid), errors.As(err, &record), errors.As(err, &alert),
		// An alert the server sent (it wants a client certificate, it supports
		// no common version) arrives as an OpError "remote error"; AlertError
		// only wraps alerts this side sent.
		errors.As(err, &opErr) && opErr.Op == "remote error",
		// https to a port that speaks plain HTTP. net/http reports it as a bare
		// string, so the string is all there is to match.
		strings.Contains(err.Error(), "server gave HTTP response to HTTPS client"):
		detail = store.ProbeConnTLS
	case errors.Is(err, syscall.ECONNREFUSED):
		detail = store.ProbeConnRefused
	case errors.Is(err, syscall.ECONNRESET):
		detail = store.ProbeConnReset
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		detail = store.ProbeConnUnreachable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &netErr) && netErr.Timeout():
		detail = store.ProbeConnTimeout
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		detail = store.ProbeConnClosed
	}
	return store.ProbeError(store.ProbeErrConn, detail)
}
