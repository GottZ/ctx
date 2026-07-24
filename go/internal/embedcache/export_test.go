package embedcache

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SetCacheProbeForTest swaps the cache fast-path lookup (DB-free negative
// probe of the I-D1 cache-hit clause) and returns the restore func.
func SetCacheProbeForTest(f func(ctx context.Context, pool *pgxpool.Pool, key []byte, model string) ([]float32, bool)) (restore func()) {
	prev := cacheProbe
	cacheProbe = f
	return func() { cacheProbe = prev }
}

// CacheProbeForTest exposes the production touch-UPDATE lookup to the
// black-box integration test (W01-2 gate (e) red side: prove the EXISTING
// path touches hit_count/last_access — the reason PeekByHash exists). The
// test must live in package embedcache_test: a white-box test cannot import
// testdb (testdb → store → ... → embedcache would be an import cycle).
func CacheProbeForTest(ctx context.Context, pool *pgxpool.Pool, key []byte, model string) ([]float32, bool) {
	return cacheProbe(ctx, pool, key, model)
}
