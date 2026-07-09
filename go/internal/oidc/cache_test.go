package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingServer returns an httptest server that counts hits and serves body.
func countingServer(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// W2 gate: cache miss — the first Get hits upstream exactly once.
func TestCacheMissFetchesUpstream(t *testing.T) {
	srv, hits := countingServer(t, `{"a":1}`)
	c := NewCache(Options{Client: srv.Client()})

	body, err := c.Discovery(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Discovery: %v", err)
	}
	if string(body) != `{"a":1}` {
		t.Fatalf("body = %q", body)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

// W2 gate: cache hit — two Gets produce exactly one upstream call.
func TestCacheHitServesFromCache(t *testing.T) {
	srv, hits := countingServer(t, `{"a":1}`)
	c := NewCache(Options{Client: srv.Client()})

	for i := 0; i < 2; i++ {
		if _, err := c.Discovery(context.Background(), srv.URL); err != nil {
			t.Fatalf("Discovery #%d: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

// W2 gate: TTL expiry — after the TTL has passed (injected clock, no
// sleeping) the next Get goes upstream again.
func TestCacheTTLExpiry(t *testing.T) {
	srv, hits := countingServer(t, `{"a":1}`)
	c := NewCache(Options{Client: srv.Client(), DiscoveryTTL: 5 * time.Minute})

	base := time.Now()
	c.now = func() time.Time { return base }

	if _, err := c.Discovery(context.Background(), srv.URL); err != nil {
		t.Fatalf("Discovery #1: %v", err)
	}
	// still fresh at base+4min → cache hit
	c.now = func() time.Time { return base.Add(4 * time.Minute) }
	if _, err := c.Discovery(context.Background(), srv.URL); err != nil {
		t.Fatalf("Discovery #2: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits before expiry = %d, want 1", got)
	}
	// expired at base+6min → refetch
	c.now = func() time.Time { return base.Add(6 * time.Minute) }
	if _, err := c.Discovery(context.Background(), srv.URL); err != nil {
		t.Fatalf("Discovery #3: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits after expiry = %d, want 2", got)
	}
}

// W2 gate: singleflight — N concurrent misses collapse into one upstream
// call. The handler blocks briefly so all goroutines are in flight together.
func TestCacheSingleflightCollapsesConcurrentMisses(t *testing.T) {
	var hits atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	t.Cleanup(srv.Close)
	c := NewCache(Options{Client: srv.Client()})

	const n = 16
	var start, done sync.WaitGroup
	start.Add(n)
	done.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			start.Done()
			start.Wait() // barrier: all goroutines enter together
			_, err := c.JWKS(context.Background(), srv.URL)
			errs <- err
		}()
	}
	start.Wait()
	// Give the flight leader time to reach the handler, then release it.
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("JWKS: %v", err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (singleflight)", got)
	}
}

// W2 gate: non-200 is NOT cached — after an upstream 500 the next Get hits
// upstream again (no negative caching) and succeeds.
func TestCacheNon200NotCached(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	t.Cleanup(srv.Close)
	c := NewCache(Options{Client: srv.Client()})

	if _, err := c.Discovery(context.Background(), srv.URL); err == nil {
		t.Fatal("Discovery #1: want error on 500, got nil")
	}
	body, err := c.Discovery(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Discovery #2: %v", err)
	}
	if string(body) != `{"a":1}` {
		t.Fatalf("body = %q", body)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (500 must not be cached)", got)
	}
}

// Discovery and JWKS use separate buckets even at the same URL.
func TestCacheSeparateBucketsForDiscoveryAndJWKS(t *testing.T) {
	srv, hits := countingServer(t, `{"a":1}`)
	c := NewCache(Options{Client: srv.Client()})

	if _, err := c.Discovery(context.Background(), srv.URL); err != nil {
		t.Fatalf("Discovery: %v", err)
	}
	if _, err := c.JWKS(context.Background(), srv.URL); err != nil {
		t.Fatalf("JWKS: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (separate buckets)", got)
	}
}

// Oversized bodies are rejected, not truncated into the cache.
func TestCacheBodyCapRejectsOversizedResponse(t *testing.T) {
	big := make([]byte, maxFetchBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	t.Cleanup(srv.Close)
	c := NewCache(Options{Client: srv.Client()})

	if _, err := c.Discovery(context.Background(), srv.URL); err == nil {
		t.Fatal("want error for oversized body, got nil")
	}
}
