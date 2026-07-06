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
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/llmlog"
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

// AdmissionError mirrors llm.AdmissionError without importing llm (same
// decoupling as Admission/ReportFunc above): a failed embed acquire carries
// the K9 rejection telemetry — the rejected target and the futile wait — so
// llm.RecordRejection can attribute the rejection line (MW11, design/05
// §3.2). Unwrap keeps errors.Is against the dispatch sentinels intact.
type AdmissionError struct {
	Err error
	// Backend/Host/WaitMs are zero-valued for the nil-admitter error
	// (nothing waited anywhere) — the mirror of llm.AdmissionError.
	Backend string
	Host    string
	WaitMs  int64
}

func (e *AdmissionError) Error() string { return e.Err.Error() }
func (e *AdmissionError) Unwrap() error { return e.Err }

// acquire leases one wire attempt on the backend's physical origin. The
// embed path deliberately carries NO deadline hint (embed.go: wire timeout
// removed in Welle 49; the reaper's lease_max_age fallback covers a leaked
// lease — design/01 §4.4).
func (a Admission) acquire(ctx context.Context, b *backends.Backend, role string) (*dispatch.Lease, context.Context, error) {
	if a.Admitter == nil {
		return nil, nil, &AdmissionError{Err: fmt.Errorf("embedcache: %s call site without dispatch admitter (I-D1)", role)}
	}
	enqueued := time.Now()
	// MW4 (design/03 §4.1.1): no principal parameter — the dispatcher
	// derives it from the request ctx flowing into this acquire.
	lease, runCtx, err := a.Admitter.Acquire(ctx, dispatch.Request{
		Target: dispatch.Target{Origin: b.Host}, // Acquire normalizes defensively (design/01 §4.3)
		Class:  a.Class,
		Role:   role,
	})
	if err != nil {
		return nil, nil, &AdmissionError{Err: err, Backend: b.Name, Host: b.Host, WaitMs: time.Since(enqueued).Milliseconds()}
	}
	return lease, runCtx, nil
}

// WireAttempt is one tried backend of an embed wire-call sequence — the
// embed mirror of llm.ChainAttempt with the SAME JSON keys (metadata.chain
// stays ONE vocabulary across pipelines). llm.LogEmbedWire derives the MW10
// telemetry columns from the row-defining attempt (design/05 §3.2) and
// stores the full slice as metadata.chain (Σ single-case forensics).
type WireAttempt struct {
	Backend string `json:"backend"`
	Class   string `json:"err_class"`
	Ms      int64  `json:"ms"`
	WaitMs  int64  `json:"wait_ms"`
	// PromptTokens is the embed backend's reported prompt-token usage of a
	// SUCCESSFUL attempt (0 otherwise). It is the D1(a) embed-token metric
	// substrate: llm.LogEmbedWire copies the serving attempt's count onto the
	// llmlog row (prompt_tokens), so the status page can aggregate embed tokens
	// per target/window from llmlog — the SAME column the chat path already
	// fills. Distinct from the dispatcher usage window (C1/MW22), which the
	// lease.ReportUsage call feeds independently.
	PromptTokens int `json:"prompt_tokens,omitempty"`
}

// abortClass classifies a dispatcher-caused cancel of one attempt's runCtx —
// the embed-loop mirror of llm's §4.4c discrimination (MW11, design/05
// §4.4c/B-R9): "preempted"/"reaped" via errors.Is over context.Cause
// (wrap-safe, never sentinel identity), everything else "".
func abortClass(runCtx context.Context) string {
	cause := context.Cause(runCtx)
	switch {
	case errors.Is(cause, dispatch.ErrPreempted):
		return llmlog.AbortPreempted
	case errors.Is(cause, dispatch.ErrReaped):
		return llmlog.AbortReaped
	default:
		return ""
	}
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
// (doctrine §4.3): no attempt entry, no Classify, no health report — and
// wired stays false unless an earlier attempt DID reach the wire, so the
// caller's llmlog gate keeps the no-wire-no-row semantics (the background
// callers hook llm.RecordRejection on that path for the K9 line, MW11). A
// successful attempt charges the backend-reported prompt tokens into the
// lease before release (MW22/C1); a backend without counts stays uncharged.
//
// MW11 telemetry (design/05 §4.4a/§4.4b): attempts carries per attempt the
// lease wait (admitted − enqueued, the single queue_wait_ms source) and the
// wait-free wire span — the old caller-side clock spanned acquire+call and
// would have double-counted the wait under enforcement. §4.4c abort rule:
// a dispatcher-canceled attempt (cause ErrPreempted/ErrReaped on the lease's
// runCtx) is TERMINAL — no failover onto following links and no report()
// (the pool health state is shared with interactive, B-R9) — and carries the
// abort kind as its class.
func EmbedChain(ctx context.Context, pool *pgxpool.Pool, chain []backends.Backend, role, text string, prefix embed.Prefix, report ReportFunc, adm Admission) (vec []float32, served *backends.Backend, attempts []WireAttempt, wired bool, err error) {
	if len(chain) == 0 {
		return nil, nil, nil, false, fmt.Errorf("embedcache: chain is empty")
	}

	key := hashKey(prefix, text)
	if cached, hit := cacheProbe(ctx, pool, key, chain[0].ModelFor(role).Model); hit {
		return cached, nil, nil, false, nil
	}

	var lastErr error
	for i := range chain {
		b := chain[i] // copy: the model resolves per role without mutating the snapshot
		b.Model = b.ModelFor(role).Model
		if b.Model == "" {
			attempts = append(attempts, WireAttempt{Backend: b.Name, Class: "no_model"})
			lastErr = fmt.Errorf("embedcache: backend %s has no model for role %s", b.Name, role)
			continue
		}

		lease, runCtx, admErr := adm.acquire(ctx, &b, role)
		if admErr != nil {
			return nil, nil, attempts, wired, fmt.Errorf("embedcache: admission: %w", admErr)
		}
		// Lease wait of THIS attempt, captured before the wire call; the
		// pass-through state yields a real 0 (design/05 §3.2, B-R4).
		waitMs := lease.WaitDur().Milliseconds()
		start := time.Now()
		var okTokens int
		v, werr := func() ([]float32, error) {
			// defer is the only allowed release form (B1: panic-safe).
			defer lease.Release()
			v, ptoks, werr := embed.Embed(runCtx, b, text, prefix)
			if werr == nil && ptoks > 0 {
				// Embeds charge their prompt side only (C1); booked at Release.
				lease.ReportUsage(dispatch.Usage{PromptTokens: ptoks})
				okTokens = ptoks // D1(a): also carried onto the llmlog row
			}
			return v, werr
		}()
		elapsed := time.Since(start).Milliseconds()
		wired = true
		if werr == nil {
			attempts = append(attempts, WireAttempt{Backend: b.Name, Class: "ok", Ms: elapsed, WaitMs: waitMs, PromptTokens: okTokens})
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
		// §4.4c abort rule (MW11 — BEFORE the generic Classify): terminal,
		// no health report, abort kind instead of the generic class.
		if abort := abortClass(runCtx); abort != "" {
			attempts = append(attempts, WireAttempt{Backend: b.Name, Class: abort, Ms: elapsed, WaitMs: waitMs})
			return nil, nil, attempts, true, lastErr
		}
		class := backends.Classify(werr, b.ProviderClass)
		attempts = append(attempts, WireAttempt{Backend: b.Name, Class: class.String(), Ms: elapsed, WaitMs: waitMs})
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
