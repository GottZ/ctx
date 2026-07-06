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
