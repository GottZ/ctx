// Package oidc is the OIDC/OAuth consumer library for external login
// (design/04 §4.1, waves W2/W3). Pure library: no ctx store/handler
// dependencies — wiring into routes and provider config rows happens in
// later waves (W6). Ported from the hand-written serviceportal consumer
// (/compose/serviceportal internal/cache/oidc.go + internal/auth/oidc.go)
// and generalized: the RS256 hardlock becomes a per-provider algorithm set,
// the Entra tid check becomes the generic ClaimFilter hook.
//
// Design decisions carried over from the port source:
//   - The cache stores []byte (raw JSON). Callers decode into their own
//     types — avoids import cycles between cache users.
//   - Discovery and JWKS get separate TTLs (5min / 10min default — JWKS
//     rotates less often, the discovery URL is typically static).
//   - Negative caching is disabled: non-200 responses are never stored.
//     A transient 500 must not block logins for a full TTL.
//   - singleflight de-duplicates concurrent cache misses for the same key,
//     so a login storm cannot thundering-herd the IdP.
//   - http.Client is injectable (Options.Client) for httptest.
//
// Added over the port source: a hard response body cap (fail-closed against
// endless bodies) and an injectable clock for TTL tests.
package oidc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Default TTLs and timeout — single source, callers pass empty Options
// instead of duplicating the values.
const (
	// DefaultDiscoveryTTL — the discovery document rarely changes.
	DefaultDiscoveryTTL = 5 * time.Minute
	// DefaultJWKSTTL — IdPs rotate signing keys far less often than 10min.
	DefaultJWKSTTL = 10 * time.Minute
	// DefaultFetchTimeout bounds every upstream call.
	DefaultFetchTimeout = 10 * time.Second
	// maxFetchBytes caps every fetched body. Discovery documents and JWKS
	// are a few KB; 1 MiB is generous headroom while still refusing to
	// buffer an attacker-sized response into memory.
	maxFetchBytes = 1 << 20
)

// Options configures a Cache. Zero values fall back to the defaults above.
type Options struct {
	// Client is used for all HTTP calls. Default: client with
	// DefaultFetchTimeout. Injection point for httptest AND for a
	// hardened transport (e.g. ssrfguard dial control) at wiring time.
	Client *http.Client
	// DiscoveryTTL is the cache lifetime for discovery documents.
	DiscoveryTTL time.Duration
	// JWKSTTL is the cache lifetime for JWKS documents.
	JWKSTTL time.Duration
}

// Cache is a TTL cache for OIDC discovery documents and JWKS bodies.
// Thread-safe; singleflight collapses concurrent misses into one fetch.
type Cache struct {
	client       *http.Client
	discoveryTTL time.Duration
	jwksTTL      time.Duration

	mu      sync.Mutex
	entries map[string]*cacheEntry

	sf singleflight.Group

	// now is the clock, injectable in tests (TTL expiry without sleeping).
	now func() time.Time
}

type cacheEntry struct {
	body      []byte
	expiresAt time.Time
}

// NewCache creates a Cache. The zero Options value is valid (all defaults).
func NewCache(opts Options) *Cache {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: DefaultFetchTimeout}
	}
	if opts.DiscoveryTTL <= 0 {
		opts.DiscoveryTTL = DefaultDiscoveryTTL
	}
	if opts.JWKSTTL <= 0 {
		opts.JWKSTTL = DefaultJWKSTTL
	}
	return &Cache{
		client:       opts.Client,
		discoveryTTL: opts.DiscoveryTTL,
		jwksTTL:      opts.JWKSTTL,
		entries:      make(map[string]*cacheEntry),
		now:          time.Now,
	}
}

// Discovery returns the cached discovery document body or fetches it on miss.
// Callers decode the []byte themselves (see discoveryDocument in provider_oidc.go).
func (c *Cache) Discovery(ctx context.Context, url string) ([]byte, error) {
	return c.fetchCached(ctx, "disc:"+url, url, c.discoveryTTL)
}

// JWKS returns the cached JWKS body or fetches it on miss.
func (c *Cache) JWKS(ctx context.Context, url string) ([]byte, error) {
	return c.fetchCached(ctx, "jwks:"+url, url, c.jwksTTL)
}

// fetchCached is the shared lookup-then-fetch logic. key is namespace-prefixed
// (disc:/jwks:) so discovery and JWKS at the same URL use separate buckets.
func (c *Cache) fetchCached(ctx context.Context, key, url string, ttl time.Duration) ([]byte, error) {
	if body, ok := c.lookup(key); ok {
		return body, nil
	}

	// singleflight: concurrent cache misses for the same key collapse into
	// exactly one upstream call. shared=true is not a security concern —
	// every caller receives the same immutable []byte.
	v, err, _ := c.sf.Do(key, func() (any, error) {
		// After acquiring the flight: re-check whether someone filled the
		// cache between our lookup and sf.Do (lookup/Do race).
		if body, ok := c.lookup(key); ok {
			return body, nil
		}

		body, err := c.fetch(ctx, url)
		if err != nil {
			return nil, err
		}
		c.store(key, body, ttl)
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// lookup checks the cache. Returns (body, true) on a fresh hit; expired
// entries are evicted and reported as a miss.
func (c *Cache) lookup(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if c.now().After(e.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return e.body, true
}

// store writes an entry. ttl<=0 means "do not cache".
func (c *Cache) store(key string, body []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &cacheEntry{
		body:      body,
		expiresAt: c.now().Add(ttl),
	}
}

// fetch performs the actual HTTP GET. Non-200 → error, no cache insert
// (negative caching is deliberately off). The body is hard-capped at
// maxFetchBytes — an oversized response is a reject, not a truncate.
func (c *Cache) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: build request for %s: %w", url, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: fetch %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("oidc: read %s: %w", url, err)
	}
	if len(body) > maxFetchBytes {
		return nil, fmt.Errorf("oidc: fetch %s: response exceeds %d bytes", url, maxFetchBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: fetch %s returned %d: %s", url, resp.StatusCode, truncate(body, 256))
	}
	return body, nil
}

// truncate shortens a body to at most n bytes so error messages never embed
// a full HTML error page.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
