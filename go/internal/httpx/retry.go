// Package httpx hardens HTTP calls against transient transport failures of
// llama.cpp's cpp-httplib servers. Their short keep-alive timeout (~5s) races
// with Go's idle-connection reuse: the server closes an idle connection in the
// exact moment the client reuses it, surfacing as "connection reset by peer"
// or an unexpected EOF before any response bytes arrive. Go's net/http only
// auto-retries such failures on connections it knows were reused; a reset on a
// fresh connection (e.g. server-side backlog churn) passes through.
//
// Inference requests carry no server-side state, so replaying a POST whose
// connection died before a response is safe — the worst case is wasted compute.
package httpx

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// IsTransientNetErr reports whether err looks like a connection that died
// before an HTTP response was received. Context cancellation and deadline
// expiry are explicitly NOT transient: the caller's time budget is spent.
func IsTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "server closed idle connection")
}

// IsBackendUnavailable reports whether err indicates the backend could not be
// reached at transport level: a transient mid-flight failure (IsTransientNetErr)
// or the host being down (dial refused/timeout, no route, DNS failure). A slow
// but alive backend (context deadline during read) does NOT count — failing
// over from a busy server to a slower one helps nobody.
func IsBackendUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if IsTransientNetErr(err) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no route to host")
}

// DoRetryOnce executes req and retries exactly once on a transient transport
// error, replaying the given body on a fresh connection. HTTP status errors
// (the server answered) and context errors (timeout/cancel) are returned
// unchanged. body must be the same bytes the original request was built from.
func DoRetryOnce(c *http.Client, req *http.Request, body []byte) (*http.Response, error) {
	resp, err := c.Do(req) //nolint:gosec // G704: URL stammt aus Betreiber-Config (CTX_*_HOST), kein User-Input — kein SSRF-Vektor.
	if err == nil || !IsTransientNetErr(err) || req.Context().Err() != nil {
		return resp, err
	}
	slog.Warn("httpx: transient transport error, retrying once",
		"url", req.URL.String(), "error", err)
	retry := req.Clone(req.Context())
	retry.Body = io.NopCloser(bytes.NewReader(body))
	retry.ContentLength = int64(len(body))
	return c.Do(retry) //nolint:gosec // G704: siehe oben — Config-URLs, kein SSRF-Vektor.
}
