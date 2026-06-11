// Package embedcache persists computed embeddings keyed by (text_hash, model),
// avoiding re-computation for repeated inputs. Primary hit-path: Dream-Keyword
// embeddings across cycles, query embeddings for repeated user queries.
//
// Cache reads update last_access and hit_count in the same UPDATE statement
// so eviction can prune by recency without a separate touch path.
//
// Source: https://github.com/GottZ/ctx
package embedcache

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// previewLen caps the stored text_preview column — just enough to recognise
// entries when inspecting the table, not enough to reconstruct prompts.
const previewLen = 200

// hashKey derives the cache key from the raw text plus the prefix role.
// Including the prefix prevents query-embeddings from colliding with
// document-embeddings of the same string.
func hashKey(prefix embed.Prefix, text string) []byte {
	h := sha256.New()
	h.Write([]byte{byte(prefix)})
	h.Write([]byte{0})
	h.Write([]byte(text))
	return h.Sum(nil)
}

// Embed returns an embedding for text under the backend tuple's model, serving
// from the cache if possible. A cache hit issues one UPDATE (and no embed call).
// A cache miss issues the embed call, then upserts the result. pool may be nil —
// in that case the cache is bypassed and the embed call runs unwrapped.
func Embed(ctx context.Context, pool *pgxpool.Pool, b backends.Backend, text string, prefix embed.Prefix) ([]float32, error) {
	if pool == nil {
		return embed.Embed(ctx, b, text, prefix)
	}

	key := hashKey(prefix, text)

	// Fast path: cache hit. UPDATE ... RETURNING is atomic; no race with concurrent hits.
	var cached pgvector.Vector
	err := pool.QueryRow(ctx,
		`UPDATE context_embed_cache
		SET hit_count = hit_count + 1, last_access = now()
		WHERE text_hash = $1 AND model = $2
		RETURNING embedding`,
		key, b.Model,
	).Scan(&cached)
	if err == nil {
		return cached.Slice(), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		// Unexpected DB error — fall through to compute. Cache failures must never
		// block the hot path; the call still succeeds via the embed backend.
		return embed.Embed(ctx, b, text, prefix)
	}

	// Cache miss: compute, then store.
	vec, err := embed.Embed(ctx, b, text, prefix)
	if err != nil {
		return nil, err
	}

	preview := text
	if len(preview) > previewLen {
		preview = preview[:previewLen]
	}
	_, _ = pool.Exec(ctx,
		`INSERT INTO context_embed_cache
			(text_hash, model, embedding, text_preview)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (text_hash, model) DO UPDATE SET
			hit_count   = context_embed_cache.hit_count + 1,
			last_access = now()`,
		key, b.Model, pgvector.NewVector(vec), preview,
	)
	return vec, nil
}

// Flush drops the ENTIRE cache. Called when an embed-cache-coupled config
// value changes (embed/dream_embed host or protocol, X2): the cache keys only
// on (text_hash, model), so vectors computed by the OLD backend would
// otherwise be served against documents embedded by the new one (cosine
// 0.997 != 1.0 measured — silent retrieval degradation, R5). Returns the
// number of rows removed.
func Flush(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM context_embed_cache`)
	if err != nil {
		return 0, fmt.Errorf("embedcache: flush: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Evict removes stale cache entries. Applies both a TTL (entries not accessed
// within ttl are dropped) and a size cap (if count exceeds maxRows, the
// least-recently-accessed rows are pruned to maxRows). Returns the count removed.
func Evict(ctx context.Context, pool *pgxpool.Pool, ttlDays int, maxRows int) (int64, error) {
	var removed int64

	if ttlDays > 0 {
		tag, err := pool.Exec(ctx,
			`DELETE FROM context_embed_cache WHERE last_access < now() - make_interval(days => $1)`,
			ttlDays,
		)
		if err != nil {
			return removed, fmt.Errorf("embedcache: ttl evict: %w", err)
		}
		removed += tag.RowsAffected()
	}

	if maxRows > 0 {
		tag, err := pool.Exec(ctx,
			`DELETE FROM context_embed_cache
			WHERE (text_hash, model) IN (
				SELECT text_hash, model FROM context_embed_cache
				ORDER BY last_access ASC
				OFFSET $1
			)`,
			maxRows,
		)
		if err != nil {
			return removed, fmt.Errorf("embedcache: size evict: %w", err)
		}
		removed += tag.RowsAffected()
	}

	return removed, nil
}

// Stats returns aggregate cache metrics for observability.
type CacheStats struct {
	Entries   int64
	TotalHits int64
	Models    int
}

// Stats returns aggregate metrics for observability (size, hit counters, model count).
func Stats(ctx context.Context, pool *pgxpool.Pool) (CacheStats, error) {
	var s CacheStats
	err := pool.QueryRow(ctx,
		`SELECT
			count(*)::bigint,
			COALESCE(sum(hit_count), 0)::bigint,
			count(DISTINCT model)::int
		FROM context_embed_cache`,
	).Scan(&s.Entries, &s.TotalHits, &s.Models)
	return s, err
}
