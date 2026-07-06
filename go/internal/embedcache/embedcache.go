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
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
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

// ReportFunc mirrors the llm-side health feedback without importing llm —
// wired to Pool.ReportSuccess/ReportFailure by the caller.
type ReportFunc func(backendID string, class backends.ErrClass, retryAfter time.Duration)

// Admission mirrors llm.Admission without importing llm (same decoupling
// decision as ReportFunc above): the ONE process-wide dispatch admitter
// (I-D1) bound to the CALLER's class — query embed interactive, query-path
// backfill interactive per E-U5(a), scheduler backfill and dream keyword
// embeds background (design/01 §4.6 N3). The principal is ctx-derived since
// MW4 (design/03 §4.1.1) — carrying it as a field would reopen the stored-
// principal replay. Target and per-attempt binding happen inside EmbedChain;
// a zero Admission fails the acquire loudly instead of passing an unadmitted
// wire call (MW5).
type Admission struct {
	Admitter dispatch.Admitter
	Class    dispatch.Class
}

// acquire leases one wire attempt on the backend's physical origin. The
// embed path deliberately carries NO deadline hint (embed.go: wire timeout
// removed in Welle 49; the reaper's lease_max_age fallback covers a leaked
// lease — design/01 §4.4).
func (a Admission) acquire(ctx context.Context, b *backends.Backend, role string) (*dispatch.Lease, context.Context, error) {
	if a.Admitter == nil {
		return nil, nil, fmt.Errorf("embedcache: %s call site without dispatch admitter (I-D1)", role)
	}
	// MW4 (design/03 §4.1.1): no principal parameter — the dispatcher
	// derives it from the request ctx flowing into this acquire.
	return a.Admitter.Acquire(ctx, dispatch.Request{
		Target: dispatch.Target{Origin: b.Host}, // Acquire normalizes defensively (design/01 §4.3)
		Class:  a.Class,
		Role:   role,
	})
}

// cacheProbe is the cache fast-path lookup, a package var as a test seam
// (pattern: the dream package's chatJSON seam): the I-D1 cache-hit clause —
// a hit contacts no backend and acquires NO lease — needs a DB-free negative
// probe. UPDATE ... RETURNING is atomic; no race with concurrent hits. Any
// non-hit (nil pool, ErrNoRows, unexpected DB error) reports a miss — cache
// failures must never block the hot path.
var cacheProbe = func(ctx context.Context, pool *pgxpool.Pool, key []byte, model string) ([]float32, bool) {
	if pool == nil || model == "" {
		return nil, false
	}
	var cached pgvector.Vector
	err := pool.QueryRow(ctx,
		`UPDATE context_embed_cache
		SET hit_count = hit_count + 1, last_access = now()
		WHERE text_hash = $1 AND model = $2
		RETURNING embedding`,
		key, model,
	).Scan(&cached)
	if err != nil {
		return nil, false
	}
	return cached.Slice(), true
}

// EmbedChain is the chained Embed (F3-P3): cache first (keyed on the FIRST
// backend's resolved model — the chain's preferred model), then wire attempts
// over the chain with Classify deciding continuation. role resolves the
// per-backend model via ModelFor (pool rows carry model_map, not the F1 Model
// field). wired=false on a cache hit — a hit contacts no backend, is no
// egress, and must produce NO llmlog row (§2.7.3 row semantics: one row per
// actual WIRE call). served names the answering backend; attempts counts wire
// tries for llmlog. Retry-After precision is not extracted here (local embed
// backends don't rate-limit; a 429 earns the class default cooldown).
//
// MW5 admission (design/01 §4.6 N3): each wire attempt runs under its own
// lease on the attempt's physical origin, acquired immediately before the
// embed call with a FRESH queue position — a multi-call consumer (the
// backfill loops) therefore holds no standing reservation across calls, which
// is exactly the E-U5(a) structural overtake: a younger interactive query
// embed passes the waiting backfill rest between two of its acquires. The
// cache-hit fast path stays lease-free (I-D1). A failed acquire is TERMINAL
// (doctrine §4.3): no attempt count, no Classify, no health report — and
// wired stays false unless an earlier attempt DID reach the wire, so the
// caller's llmlog gate keeps the no-wire-no-row semantics. A successful
// attempt charges the backend-reported prompt tokens into the lease before
// release (MW22/C1); a backend without counts stays uncharged.
func EmbedChain(ctx context.Context, pool *pgxpool.Pool, chain []backends.Backend, role, text string, prefix embed.Prefix, report ReportFunc, adm Admission) (vec []float32, served *backends.Backend, attempts int, wired bool, err error) {
	if len(chain) == 0 {
		return nil, nil, 0, false, fmt.Errorf("embedcache: chain is empty")
	}

	key := hashKey(prefix, text)
	if cached, hit := cacheProbe(ctx, pool, key, chain[0].ModelFor(role).Model); hit {
		return cached, nil, 0, false, nil
	}

	var lastErr error
	for i := range chain {
		b := chain[i] // copy: the model resolves per role without mutating the snapshot
		b.Model = b.ModelFor(role).Model
		if b.Model == "" {
			attempts++
			lastErr = fmt.Errorf("embedcache: backend %s has no model for role %s", b.Name, role)
			continue
		}

		lease, runCtx, admErr := adm.acquire(ctx, &b, role)
		if admErr != nil {
			return nil, nil, attempts, wired, fmt.Errorf("embedcache: admission: %w", admErr)
		}
		v, werr := func() ([]float32, error) {
			// defer is the only allowed release form (B1: panic-safe).
			defer lease.Release()
			v, ptoks, werr := embed.Embed(runCtx, b, text, prefix)
			if werr == nil && ptoks > 0 {
				// Embeds charge their prompt side only (C1); booked at Release.
				lease.ReportUsage(dispatch.Usage{PromptTokens: ptoks})
			}
			return v, werr
		}()
		attempts++
		wired = true
		if werr == nil {
			if report != nil {
				report(b.ID, backends.ClassOK, 0)
			}
			if pool != nil {
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
					key, b.Model, pgvector.NewVector(v), preview,
				)
			}
			return v, &chain[i], attempts, true, nil
		}
		lastErr = werr
		class := backends.Classify(werr, b.ProviderClass)
		if report != nil && class != backends.ClassCanceled {
			report(b.ID, class, 0)
		}
		if ctx.Err() != nil || !class.Next() {
			return nil, nil, attempts, true, lastErr
		}
	}
	return nil, nil, attempts, wired, fmt.Errorf("embedcache: chain exhausted: %w", lastErr)
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
