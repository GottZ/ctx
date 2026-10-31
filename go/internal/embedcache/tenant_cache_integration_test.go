//go:build integration

// GHSA 2026-10-31 §8 — the embed cache is the operator's. context_embed_cache
// keys on (text_hash, model) across every tenant and the model name is a free
// field on a tenant-admin's pool row, so a tenant row must neither be served
// from the cache (a hit/miss would tell the tenant whether anyone embedded the
// exact text before) nor write to it (crafted vectors under the operator's
// model name would poison every tenant's retrieval, first writer wins). The
// same chain on a _global row keeps the cache-first behaviour.
//
// RED without embedcache.cacheable: the tenant row's seeded text is served
// from the cache (no error, no wire attempt, hit_count +1).
//
// Since §3 a tenant row dials through internal/ssrfguard, so its wire call to
// the loopback fake below is refused at dial time ("SSRF guard"): the tenant
// subtests assert that the chain WENT to the wire (the refusal) instead of
// answering from the cache, and that nothing reached the server or the cache.
// The write side uses the same cacheable predicate as the probe; a tenant
// wire success cannot be produced against a loopback fake any more.
//
//	go test -tags=integration ./internal/embedcache/ -run TestEmbedChainTenantRowBypassesCache -count=1 -v
package embedcache_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestEmbedChainTenantRowBypassesCache(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Fake ollama /api/embed wire: counts calls, answers a quality-gate
	// passing vector (alternating 0/2, see embedcache_admission_test.go).
	var wireHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		wireHits.Add(1)
		raw := make([]float64, embed.TargetDims)
		for i := range raw {
			raw[i] = float64((i % 2) * 2)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{raw}, "prompt_eval_count": 3})
	}))
	t.Cleanup(srv.Close)

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	dispatch.SetPrincipalHook(func(context.Context) dispatch.Principal {
		return dispatch.Principal{ApiKeyID: "k1", TenantID: "t1", HomeScope: "tenant-a"}
	})
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	adm := embedcache.Admission{Admitter: d, Class: dispatch.ClassInteractive}

	const model = "shared-operator-embed"
	row := func(scope string) []backends.Backend {
		return []backends.Backend{{
			ID: "embed-" + scope, Name: "embed-" + scope, Host: srv.URL, Scope: scope,
			ModelMap: map[string]backends.ModelSpec{"default": {Model: model}},
		}}
	}

	// The operator's cached vector for the seeded text.
	const seeded = "quarterly revenue forecast for the board"
	seededKey := embedcache.HashKey(embed.PrefixQuery, seeded)
	vec := make([]float32, embed.TargetDims)
	for i := range vec {
		vec[i] = float32(i%7) * 0.1
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview) VALUES ($1, $2, $3, $4)`,
		seededKey, model, pgvec.NewVector(vec), seeded); err != nil {
		t.Fatalf("seed cache entry: %v", err)
	}
	hitCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT hit_count FROM context_embed_cache WHERE text_hash = $1 AND model = $2`, seededKey, model).Scan(&n); err != nil {
			t.Fatalf("read seeded hit_count: %v", err)
		}
		return n
	}
	cacheRows := func(text string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_cache WHERE text_hash = $1 AND model = $2`,
			embedcache.HashKey(embed.PrefixQuery, text), model).Scan(&n); err != nil {
			t.Fatalf("count cache rows: %v", err)
		}
		return n
	}
	call := func(chain []backends.Backend, text string) (served *backends.Backend, wired bool) {
		t.Helper()
		v, served, _, wired, err := embedcache.EmbedChain(ctx, pool, chain, "embed", text, embed.PrefixQuery, nil, adm)
		if err != nil || len(v) != embed.TargetDims {
			t.Fatalf("EmbedChain(%q): dims=%d err=%v", text, len(v), err)
		}
		return served, wired
	}

	// tenantCall runs the chain on a tenant row and expects the §3 dial-time
	// refusal: proof that the chain skipped the cache and went to the wire,
	// without a byte reaching the fake server.
	tenantCall := func(text string) {
		t.Helper()
		before := wireHits.Load()
		v, served, _, wired, err := embedcache.EmbedChain(ctx, pool, row("tenant-a"), "embed", text, embed.PrefixQuery, nil, adm)
		if err == nil || !strings.Contains(err.Error(), "SSRF guard") {
			t.Fatalf("tenant row EmbedChain(%q): dims=%d served=%v err=%v, want the SSRF-guard wire refusal (a cache hit answers without error)",
				text, len(v), served, err)
		}
		if !wired || served != nil || wireHits.Load() != before {
			t.Fatalf("tenant row: wired=%v served=%v wire hits +%d, want a refused wire attempt", wired, served, wireHits.Load()-before)
		}
	}

	hits0 := hitCount()

	t.Run("TenantRow_NotServedFromCache", func(t *testing.T) {
		tenantCall(seeded)
		if got := hitCount(); got != hits0 {
			t.Errorf("tenant row probed the cache: hit_count %d -> %d, want untouched", hits0, got)
		}
	})

	t.Run("TenantRow_WritesNoCacheRow", func(t *testing.T) {
		const fresh = "tenant-a only text that nobody embedded before"
		tenantCall(fresh)
		if n := cacheRows(fresh); n != 0 {
			t.Errorf("tenant row wrote %d cache row(s) under the operator's model name, want 0", n)
		}
	})

	t.Run("GlobalRow_ServesAndWrites", func(t *testing.T) {
		before := wireHits.Load()
		served, wired := call(row(backends.GlobalScope), seeded)
		if wired || served != nil || wireHits.Load() != before {
			t.Fatalf("_global row missed the cache: wired=%v served=%v wire hits +%d, want a cache hit",
				wired, served, wireHits.Load()-before)
		}
		if got := hitCount(); got != hits0+1 {
			t.Errorf("_global cache hit: hit_count %d -> %d, want +1", hits0, got)
		}
		const fresh = "operator text that lands in the cache"
		if _, wired := call(row(backends.GlobalScope), fresh); !wired {
			t.Fatal("fresh _global text not wired")
		}
		if n := cacheRows(fresh); n != 1 {
			t.Errorf("_global row wrote %d cache row(s), want 1", n)
		}
	})
}
