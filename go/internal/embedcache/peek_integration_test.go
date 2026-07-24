//go:build integration

// Integration coverage for W01-2 gate (e): touch-freedom of the recall
// sampler's cache read. RED against the EXISTING cacheProbe path (via the
// CacheProbeForTest seam — a white-box test could not import testdb without
// an import cycle): the production fast-path lookup is an UPDATE ...
// RETURNING that increments hit_count and refreshes last_access — exactly
// the side effect the recall probe must not have (§4.2.2/§5.4: it would
// break the "only write is the runs insert" pledge AND make sampled entries
// eviction-immune). GREEN: PeekByHash returns the same vector and leaves
// both columns byte-identical.
//
// Run with:
//
//	go test -tags=integration ./internal/embedcache/ -run TestPeekByHash -count=1 -v
package embedcache_test

import (
	"context"
	"testing"
	"time"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestPeekByHashTouchFree(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const model = "peek-test-model"
	const text = "which scheduler arm owns the janitor bundle"
	key := embedcache.HashKey(embed.PrefixQuery, text)
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = float32(i%7) * 0.1
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
		 VALUES ($1, $2, $3, $4)`,
		key, model, pgvec.NewVector(vec), text); err != nil {
		t.Fatalf("seed cache entry: %v", err)
	}

	readState := func() (hits int, last time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT hit_count, last_access FROM context_embed_cache WHERE text_hash = $1 AND model = $2`,
			key, model,
		).Scan(&hits, &last); err != nil {
			t.Fatalf("read cache state: %v", err)
		}
		return hits, last
	}

	hits0, last0 := readState()

	// RED against the existing path: cacheProbe is the production fast-path
	// lookup — it MUST show the touch (that is the as-is behavior the recall
	// sampler is forbidden to reuse). If this ever stops touching, the
	// eviction mechanics changed and the PeekByHash rationale needs a
	// re-check.
	got, hit := embedcache.CacheProbeForTest(ctx, pool, key, model)
	if !hit {
		t.Fatal("cacheProbe missed a seeded entry")
	}
	if len(got) != 1024 {
		t.Fatalf("cacheProbe returned %d dims", len(got))
	}
	hits1, last1 := readState()
	if hits1 != hits0+1 {
		t.Errorf("cacheProbe: hit_count %d -> %d, expected the touch (+1) — as-is red evidence", hits0, hits1)
	}
	if !last1.After(last0) {
		t.Errorf("cacheProbe: last_access %v -> %v, expected the touch", last0, last1)
	}
	t.Logf("RED (as-is cacheProbe): hit_count %d -> %d, last_access advanced=%v", hits0, hits1, last1.After(last0))

	// GREEN: PeekByHash reads the same vector without any touch.
	peeked, hit, err := embedcache.PeekByHash(ctx, pool, key, model)
	if err != nil {
		t.Fatalf("PeekByHash: %v", err)
	}
	if !hit {
		t.Fatal("PeekByHash missed a seeded entry")
	}
	for i := range vec {
		if peeked[i] != vec[i] {
			t.Fatalf("PeekByHash vector differs at dim %d: %v != %v", i, peeked[i], vec[i])
		}
	}
	hits2, last2 := readState()
	if hits2 != hits1 || !last2.Equal(last1) {
		t.Errorf("PeekByHash touched the entry: hit_count %d -> %d, last_access %v -> %v",
			hits1, hits2, last1, last2)
	}
	t.Logf("GREEN (PeekByHash): hit_count stays %d, last_access stays %v", hits2, last2)

	// Miss semantics: unknown hash is (nil, false, nil), not an error.
	none, hit, err := embedcache.PeekByHash(ctx, pool, embedcache.HashKey(embed.PrefixQuery, "never cached"), model)
	if err != nil || hit || none != nil {
		t.Errorf("miss: got (%v, %v, %v), want (nil, false, nil)", none, hit, err)
	}
}
