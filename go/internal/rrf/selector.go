package rrf

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Semantic strategy modes (design/02-strategy-selektor.md §4.2). The three
// values are POLICY (Go + log); SQL only ever sees the two-valued
// p_semantic_mode plus a budget/cap — grey is ann with a raised
// hnsw.max_scan_tuples budget (§4.6 mapping table).
const (
	ModeANN   = "ann"
	ModeExact = "exact"
	ModeGrey  = "grey"
)

// Decision reasons (§4.2). Every value is a stable log token — Achse 01
// correlates recall@k per (mode × reason × estimate bucket), so renaming one
// breaks a measurement series, not just a string.
const (
	ReasonDisabled    = "disabled"
	ReasonProbeExact  = "probe<=exact_max"
	ReasonStatsGrey   = "stats<=grey_max"
	ReasonStatsLarge  = "stats>grey_max"
	ReasonProbeError  = "probe_error"
	ReasonStatsStale  = "stats_stale"
	ReasonExactCapHit = "exact_cap_hit"
)

// Mechanism clamps (§5.4). Policy is data, but its RANGE is code: the settings
// store validates types, not semantics, and both knobs dimension buffer touch
// and materialisation of the SHARED database. Out-of-range values are clamped
// and warned, never rejected — a hot reload must not break.
//
// greyScanTuplesCeil is mirrored as the last line of defence in the SQL body
// (migrations/112_rrf_gen15_dual_arm.sql, `p_scan_tuples > 200000` → RAISE).
// Keep both in sync.
const (
	exactMaxFloor       = 64
	exactMaxCeil        = 65536
	greyScanTuplesFloor = 1000
	greyScanTuplesCeil  = 200000
)

// SelectorPolicy is the caller-supplied policy. The zero value is OFF and
// forces the Ist path (mode ann, NO probe roundtrip) — a caller without a
// policy behaves byte-identically to the pre-selector state (fail-closed
// against forgotten wiring, same doctrine as the empty-scope guard in Search).
// The rrf package never reads internal/config (F1 layering): policy arrives as
// a parameter, W02-3 wires the config mirror.
type SelectorPolicy struct {
	Enabled        bool
	ExactMax       int           // probe cap + exact threshold (clamp §5.4)
	GreyMax        int           // grey-zone ceiling (pg_stats estimate, W02-4)
	GreyScanTuples int           // hnsw.max_scan_tuples in the grey case (clamp §5.4)
	StatsTTL       time.Duration // pg_stats snapshot age (W02-4)
}

// SelectorDecision is the logged decision — the correlation input for Achse 01.
// It NEVER carries content, only strategy metadata (§5.5): no query text, no
// titles, no block ids.
type SelectorDecision struct {
	Mode     string  // "ann" | "exact" | "grey"
	Reason   string  // see the Reason* constants
	Estimate int     // probe count (exact, ≤ ExactMax+1) or pg_stats estimate
	ProbeMs  float64 // probe roundtrip duration; 0 when no probe ran

	// SQLLimit is the CLAMPED p_limit that ctx_rrf actually received, so an
	// operator's log carries the effective window instead of the value the
	// caller computed before the clamp (Issue #40 Bug 1: the two diverged
	// silently, and result_count then looked like a thin corpus). It rides
	// this struct because it is per-search mechanism metadata on the exact
	// path the decision already travels; 0 means the search never reached
	// the clamp (an early fail-closed reject).
	SQLLimit int
}

// selectorProbe is the injected cardinality probe. Injecting it (instead of
// handing decide a *pgxpool.Pool) keeps the dispatch algorithm DB-free
// testable — including the "disabled ⇒ not a single roundtrip" assertion,
// which needs a call counter (gate W02-2-G1).
type selectorProbe func(ctx context.Context, scopes []string, limit int) (int, error)

// rrfExec runs ONE ctx_rrf call with the given Gen-15 selector arguments.
// Injected for the same reason as selectorProbe: the exact_cap_hit retry
// (§5.6 stage 3) is a Go-side control flow and is unit-tested without a DB.
type rrfExec func(ctx context.Context, mode string, scanTuples, exactCap any) ([]SearchResult, error)

// boundedProbe is stage 1 of the cardinality estimation (§4.3a): a LIMIT-capped
// index probe over idx_context_scope_active (btree (scope, created_at DESC)
// WHERE NOT is_archived — the probe predicate is deckungsgleich with the
// partial-index WHERE). Exact up to the cap, zero staleness, and cost-bounded
// by construction: at most `limit` index entries regardless of scope size, so
// the probe is scale-invariant (§6.1).
//
// The probe decides the STRATEGY, never the VISIBILITY — it counts only what
// the caller's read scopes already expose (§5.5).
func boundedProbe(ctx context.Context, pool *pgxpool.Pool, scopes []string, limit int) (int, error) {
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM (
			SELECT 1 FROM context_blocks
			WHERE scope = ANY($1::text[]) AND NOT is_archived
			LIMIT $2
		) t`, scopes, limit).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// statsEstimator is stage 2 of the cardinality estimation (§4.3b): the
// TTL-cached pg_stats estimate for a scope set, floored at exactMax+1.
// Injected exactly like selectorProbe so the dispatch stays DB-free testable;
// the implementation (catalog read, snapshot cache, §4.3b edge cases) lives
// in selector_stats.go. ok=false means "unusable" and maps onto reason
// stats_stale → plain ann.
type statsEstimator func(ctx context.Context, scopes []string, exactMax int, ttl time.Duration) (int, bool)

// poolStatsEstimator binds the process snapshot cache to a pool. The refresh
// is lazy: it happens inside the search path when the snapshot has expired,
// not on a timer — a process that never searches never reads the catalog.
func poolStatsEstimator(pool *pgxpool.Pool) statsEstimator {
	read := poolStatsReader(pool)
	return func(ctx context.Context, scopes []string, exactMax int, ttl time.Duration) (int, bool) {
		return scopeStatsCache.estimate(ctx, pool, read, scopes, exactMax, ttl)
	}
}

// clampPolicy applies the §5.4 mechanism clamps once per call site and warns
// about every clamped value. Idempotent: re-clamping an in-range policy is
// silent, so Search can clamp up-front while decide/selectorSQLArgs re-derive
// the same numbers without duplicating log lines.
func clampPolicy(p SelectorPolicy) SelectorPolicy {
	p.ExactMax = clampInt(p.ExactMax, exactMaxFloor, exactMaxCeil, "retrieval.selector.exact_max")
	p.GreyScanTuples = clampInt(p.GreyScanTuples, greyScanTuplesFloor, greyScanTuplesCeil, "retrieval.selector.grey_scan_tuples")
	return p
}

func clampInt(v, lo, hi int, key string) int {
	switch {
	case v < lo:
		slog.Warn("rrf selector: policy value clamped to mechanism floor",
			"key", key, "value", v, "clamped_to", lo)
		return lo
	case v > hi:
		slog.Warn("rrf selector: policy value clamped to mechanism ceiling",
			"key", key, "value", v, "clamped_to", hi)
		return hi
	default:
		return v
	}
}

// decide is the three-stage dispatch of §4.6. It is pure control flow over an
// injected probe: no visibility decision, no SQL text, no config.
//
//	!enabled            → {ann, disabled}            (no probe roundtrip)
//	probe error         → {ann, probe_error}         (degrade to Ist path, warn)
//	n+grants <= exactMax → {exact, probe<=exact_max}
//	stats unavailable   → {ann, stats_stale}         (stale/absent snapshot, §4.3b)
//	est <= GreyMax      → {grey, stats<=grey_max}
//	otherwise           → {ann, stats>grey_max}
func decide(ctx context.Context, probe selectorProbe, stats statsEstimator, scopes, granted []string, policy SelectorPolicy) SelectorDecision {
	if !policy.Enabled {
		return SelectorDecision{Mode: ModeANN, Reason: ReasonDisabled}
	}

	exactMax := clampInt(policy.ExactMax, exactMaxFloor, exactMaxCeil, "retrieval.selector.exact_max")

	start := time.Now()
	n, err := probe(ctx, scopes, exactMax+1)
	probeMs := float64(time.Since(start).Nanoseconds()) / 1e6
	if err != nil {
		// N5 degradation muster (query_fold.go probe): a failing probe costs
		// the strategy, never the query.
		slog.Warn("rrf selector: cardinality probe failed, falling back to ann",
			"error", err, "scopes", len(scopes), "probe_ms", probeMs)
		return SelectorDecision{Mode: ModeANN, Reason: ReasonProbeError, ProbeMs: probeMs}
	}

	// The grant OR-arm widens the exact pool additively (SQL: scope = ANY(...)
	// OR id = ANY(granted)), so the cap has to account for it — exactly as the
	// in-body cap guard does (§5.6).
	n += len(granted)

	if n <= exactMax {
		return SelectorDecision{Mode: ModeExact, Reason: ReasonProbeExact, Estimate: n, ProbeMs: probeMs}
	}

	// Stage 2 (§4.3b). The floor exactMax+1 is handed in rather than applied
	// here: the estimator is the place that knows whether it derived a value
	// at all, and the floor is evidence FROM the probe (the scope set is
	// proven larger than exactMax), not a post-hoc correction.
	est, ok := stats(ctx, scopes, exactMax, policy.StatsTTL)
	if !ok {
		return SelectorDecision{Mode: ModeANN, Reason: ReasonStatsStale, Estimate: n, ProbeMs: probeMs}
	}
	if est <= policy.GreyMax {
		return SelectorDecision{Mode: ModeGrey, Reason: ReasonStatsGrey, Estimate: est, ProbeMs: probeMs}
	}
	return SelectorDecision{Mode: ModeANN, Reason: ReasonStatsLarge, Estimate: est, ProbeMs: probeMs}
}

// selectorSQLArgs maps a decision onto the Gen-15 parameter surface
// (§4.6): exact → ('exact', NULL, clamped ExactMax) · grey → ('ann',
// clamped GreyScanTuples, NULL) · ann → ('ann', NULL, NULL).
// The three-valuedness stays in Go; SQL keeps the minimal mechanism.
func selectorSQLArgs(dec SelectorDecision, policy SelectorPolicy) (mode string, scanTuples, exactCap any) {
	switch dec.Mode {
	case ModeExact:
		return ModeExact, nil, clampInt(policy.ExactMax, exactMaxFloor, exactMaxCeil, "retrieval.selector.exact_max")
	case ModeGrey:
		return ModeANN, clampInt(policy.GreyScanTuples, greyScanTuplesFloor, greyScanTuplesCeil, "retrieval.selector.grey_scan_tuples"), nil
	default:
		return ModeANN, nil, nil
	}
}

// isIstParams reports whether the mapped arguments are the Ist parameter
// surface — plain ann, no budget, no cap. That combination is what the legacy
// 15-argument call produces through the Gen-15 parameter DEFAULTS, so Search
// keeps sending the legacy statement for it (byte-identical wire behaviour,
// and the pre-Gen-15 function stays callable).
func isIstParams(mode string, scanTuples, exactCap any) bool {
	return mode == ModeANN && scanTuples == nil && exactCap == nil
}

// isExactCapHit reports whether err is the in-body cap guard firing
// (SQLSTATE 54000 program_limit_exceeded with the exact_cap_hit marker,
// migration 112 / §5.6). A bare 54000 from anywhere else is NOT treated as the
// race — no marker, no retry.
func isExactCapHit(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "54000" && strings.Contains(pgErr.Message, ReasonExactCapHit)
}

// runSelected executes the decision and handles the probe→execution race
// (§5.6 stage 3): if the exact call trips the in-body cap guard, retry EXACTLY
// ONCE as plain ann and record the degradation in the decision + a warn log.
// The user never loses the query; Achse 01 sees the race frequency in the log.
func runSelected(ctx context.Context, dec SelectorDecision, policy SelectorPolicy, exec rrfExec) ([]SearchResult, SelectorDecision, error) {
	mode, scanTuples, exactCap := selectorSQLArgs(dec, policy)
	res, err := exec(ctx, mode, scanTuples, exactCap)
	if err == nil || mode != ModeExact || !isExactCapHit(err) {
		return res, dec, err
	}

	slog.Warn("rrf selector: exact_cap_hit, retrying once as ann",
		"error", err, "estimate", dec.Estimate, "exact_cap", exactCap)
	dec.Mode = ModeANN
	dec.Reason = ReasonExactCapHit
	res, err = exec(ctx, ModeANN, nil, nil)
	return res, dec, err
}
