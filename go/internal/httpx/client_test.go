package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPooledClientValues pins the transport values PooledClient inherited from
// the three former copies (embed, llm, rerank). They are a behaviour contract,
// not a preference: changing them changes connection reuse against the
// inference backends.
func TestPooledClientValues(t *testing.T) {
	c := PooledClient()

	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (every call site sets its own ctx deadline)", c.Timeout)
	}

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
	}
	if tr.MaxIdleConns != 20 {
		t.Errorf("MaxIdleConns = %d, want 20", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 10 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 10", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", tr.IdleConnTimeout)
	}
}

// TestPooledClientIsConstructor pins the constructor semantics: two calls yield
// two distinct clients with two distinct transports, so the callers keep
// separate idle pools — byte-equivalent to the three package-level vars that
// existed before. A singleton would silently merge the pools.
func TestPooledClientIsConstructor(t *testing.T) {
	a, b := PooledClient(), PooledClient()

	if a == b {
		t.Fatal("two calls returned the same *http.Client; PooledClient must not be a singleton")
	}
	if a.Transport == b.Transport {
		t.Error("two calls share one *http.Transport; the idle pools would be merged")
	}
}

// TestGuardedPooledClient: same pool values as PooledClient, but the dial
// runs through ssrfguard — a loopback server is refused before any request.
func TestGuardedPooledClient(t *testing.T) {
	c := GuardedPooledClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
	}
	if tr.MaxIdleConns != 20 || tr.MaxIdleConnsPerHost != 10 || tr.IdleConnTimeout != 90*time.Second || c.Timeout != 0 {
		t.Errorf("pool values drifted from PooledClient: %+v timeout %v", tr, c.Timeout)
	}
	if tr.DialContext == nil || !tr.ForceAttemptHTTP2 {
		t.Error("guarded transport needs the ssrfguard DialContext and HTTP/2 kept on")
	}

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	body := []byte(`{}`)
	_, err := DoRetryOnce(c, newReq(t, context.Background(), srv.URL, body), body) //nolint:bodyclose // error path, no body
	if err == nil || !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("guarded loopback call error = %v, want SSRF guard refusal", err)
	}
	if !IsBackendUnavailable(err) {
		t.Errorf("guard refusal must classify as backend-unavailable (failover), got %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("guarded client reached loopback")
	}

	// A NAME that resolves internally must not have its address echoed: the
	// error reaches a tenant-admin through /api/llmlog.
	byName := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	_, err = DoRetryOnce(c, newReq(t, context.Background(), byName, body), body) //nolint:bodyclose // error path, no body
	if err == nil || !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("guarded localhost call error = %v, want SSRF guard refusal", err)
	}
	if s := err.Error(); strings.Contains(s, "127.0.0.1") || strings.Contains(s, "::1") {
		t.Errorf("refusal echoes the resolved address: %v", err)
	}
	if !IsBackendUnavailable(err) {
		t.Errorf("redacted refusal must stay a dial failure, got %v", err)
	}
}

// TestClientsFor: one plain and one guarded pool per family, never shared.
func TestClientsFor(t *testing.T) {
	c := NewClients()
	if c.For(false) == c.For(true) {
		t.Fatal("guarded and plain calls share one client")
	}
	if tr := c.For(true).Transport.(*http.Transport); tr.DialContext == nil {
		t.Error("For(true) is not the guarded client")
	}
	if tr := c.For(false).Transport.(*http.Transport); tr.DialContext != nil {
		t.Error("For(false) carries a custom dialer")
	}
}
