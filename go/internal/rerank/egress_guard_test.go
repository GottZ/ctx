package rerank

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestRerankTenantRowNeverDialsLoopback (GHSA 2026-10-31 §3): a tenant row
// never reaches a loopback sidecar; a _global row does.
func TestRerankTenantRowNeverDialsLoopback(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":1.5}]}`)
	}))
	defer srv.Close()

	_, _, err := Score(context.Background(), backends.Backend{Host: srv.URL, Scope: "acme"}, "m", "q", []string{"a"}, "")
	if err == nil || !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("tenant rerank error = %v, want SSRF guard refusal", err)
	}
	if c := backends.Classify(err, ""); c != backends.ClassTransport {
		t.Errorf("Classify = %v, want transport", c)
	}
	if hits.Load() != 0 {
		t.Fatalf("tenant rerank reached the server")
	}
	if _, _, err := Score(context.Background(), backends.Backend{Host: srv.URL, Scope: backends.GlobalScope}, "m", "q", []string{"a"}, ""); err != nil {
		t.Fatalf("_global rerank: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("_global rerank hits = %d, want 1", hits.Load())
	}
}
