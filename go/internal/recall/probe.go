// probe.go — the two-leg probe (design/01 §4.2.3) plus the per-probe plan
// proof (§4.2.4) and the ties-robust, n_eff-normalized recall arithmetic.
//
// One probe = one (query vector, scope window, k): two separate BEGIN READ
// ONLY transactions on the same pool, identical predicate block — production-
// identical to the semantic CTE (073:110-120) EXCEPT the grant arm
// (`OR cb.id = ANY(p_granted_block_ids)`): probes never run on behalf of a
// key, so the OR-arm plan shape is measured offline in the grant-arm sweep
// lane (§4.5) instead. DELIBERATE, DOCUMENTED deviation — readers of the
// measurement series must not take the grant path as covered.
//
// Source: https://github.com/GottZ/ctx
package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

// hnswIndexName is the production ANN index the plan proof asserts on.
const hnswIndexName = "idx_embedding_hnsw"

// Invalid-reason codes of the plan assertion (§4.2.4). Fail-closed: a
// violated assertion writes valid=false + the reason, never a recall number.
const (
	ReasonAnnLegNotIndex    = "ann_leg_not_index"
	ReasonExactLegUsedIndex = "exact_leg_used_index"
)

// Leg statement texts. The /* recall_leg:... */ marker makes the two texts
// DIFFERENT — each leg earns its own prepared-statement cache entry and can
// never ride the other leg's plan (barrier 2 of §4.2.3; barrier 1 is
// plan_cache_mode=force_custom_plan per transaction, barrier 3 the per-probe
// EXPLAIN proof). Predicate arms: NOT is_archived + per-scope type allowlist
// + scope arm, exactly the semantic-CTE block minus the grant arm (see file
// header). $4 is the loo self-exclusion; NULL for logged queries keeps ONE
// statement text per leg (production NULL-arm style). halfvec cast as in 073.
const (
	exactLegSQL = `SELECT /* recall_leg:exact */ id::text,
	       (embedding::halfvec(1024) <=> $1)::float8 AS dist
	FROM context_blocks
	WHERE NOT is_archived
	  AND type_name = ANY($2)
	  AND scope = ANY($3)
	  AND ($4::uuid IS NULL OR id != $4::uuid)
	ORDER BY embedding::halfvec(1024) <=> $1
	LIMIT $5`

	annLegSQL = `SELECT /* recall_leg:ann */ id::text,
	       (embedding::halfvec(1024) <=> $1)::float8 AS dist
	FROM context_blocks
	WHERE NOT is_archived
	  AND type_name = ANY($2)
	  AND scope = ANY($3)
	  AND ($4::uuid IS NULL OR id != $4::uuid)
	ORDER BY embedding::halfvec(1024) <=> $1
	LIMIT $5`
)

// ProbeSpec parameterizes one two-leg probe.
type ProbeSpec struct {
	Vec          []float32 // 1024d query vector (log-sampled or loo document vector)
	SelfID       *string   // loo self-exclusion (id != $self in BOTH legs); nil for log queries
	Scopes       []string  // scope arm: [scope] for a stratum, all scopes for the pseudo-stratum "all"
	VisibleTypes []string  // per-scope type allowlist (SnapshotForTenant, §4.2.1)
	K            int
	EfSearch     int           // ANN-leg hnsw.ef_search; 0 = pgvector default 40
	Epsilon      float64       // tie tolerance of the recall definition
	Timeout      time.Duration // per-leg statement_timeout (min of rest budget and leg_timeout_ms)
}

// ProbeResult is the outcome of one two-leg probe. ExactIDs/ExactDists are
// IN-MEMORY ONLY (determinism gate f + debugging) — they must never flow into
// context_recall_runs; the persist layer's meta allowlist enforces that
// fail-closed.
type ProbeResult struct {
	Valid         bool
	InvalidReason string  // ReasonAnnLegNotIndex | ReasonExactLegUsedIndex, "" when valid
	Recall        float64 // hit / n_eff, clamped to 1.0 (ties at the k boundary)
	NEff          int     // min(k, |exact list|)
	AnnMs         float64
	ExactMs       float64
	ExactIDs      []string  // never persisted (see above)
	ExactDists    []float64 // never persisted (see above)
}

// legRow is one (id, dist) row of a leg result.
type legRow struct {
	ID   string
	Dist float64
}

// exactGUCs / annGUCs build the per-transaction SET LOCAL list (§4.2.3
// table). Both legs: plan_cache_mode=force_custom_plan (neutralizes the
// frozen-generic-plan bypass — barrier 1) + statement_timeout. Exact leg
// forces the brute-force reference (no index paths); ANN leg forces the
// index path (enable_sort=off AND enable_bitmapscan=off: under seqscan=off
// alone the planner dodged onto a bitmap scan, Achse-02 §6.2 finding) and
// mirrors the production iterative_scan setting (073:100).
func exactGUCs(timeout time.Duration) []string {
	return []string{
		"SET LOCAL plan_cache_mode = force_custom_plan",
		fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds()),
		"SET LOCAL enable_indexscan = off",
		"SET LOCAL enable_bitmapscan = off",
	}
}

func annGUCs(timeout time.Duration, efSearch int) []string {
	gucs := []string{
		"SET LOCAL plan_cache_mode = force_custom_plan",
		fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds()),
		"SET LOCAL enable_seqscan = off",
		"SET LOCAL enable_sort = off",
		"SET LOCAL enable_bitmapscan = off",
		"SET LOCAL hnsw.iterative_scan = 'relaxed_order'",
	}
	if efSearch > 0 {
		gucs = append(gucs, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch))
	}
	return gucs
}

// Probe runs one two-leg probe with the production GUC forcing (§4.2.3).
func Probe(ctx context.Context, pool *pgxpool.Pool, spec ProbeSpec) (ProbeResult, error) {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	return probeWithGUCs(ctx, pool, spec, exactGUCs(timeout), annGUCs(timeout, spec.EfSearch))
}

// probeWithGUCs is the GUC-injectable core (test seam: gate (a) proves that
// WITHOUT forcing the plan proof fails closed on today's corpus — the planner
// never takes the HNSW path voluntarily, which is exactly the self-deception
// the assertion exists to catch).
func probeWithGUCs(ctx context.Context, pool *pgxpool.Pool, spec ProbeSpec, exactSet, annSet []string) (ProbeResult, error) {
	// Exact leg first: its last distance is the reference d_ref.
	exactRows, exactMs, exactUsedIdx, err := runLeg(ctx, pool, exactLegSQL, exactSet, spec)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("recall: exact leg: %w", err)
	}
	if exactUsedIdx {
		return ProbeResult{Valid: false, InvalidReason: ReasonExactLegUsedIndex}, nil
	}

	annRows, annMs, annUsedIdx, err := runLeg(ctx, pool, annLegSQL, annSet, spec)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("recall: ann leg: %w", err)
	}
	if !annUsedIdx {
		return ProbeResult{Valid: false, InvalidReason: ReasonAnnLegNotIndex}, nil
	}

	recall, nEff := computeRecall(exactRows, annRows, spec.K, spec.Epsilon)
	res := ProbeResult{
		Valid:   true,
		Recall:  recall,
		NEff:    nEff,
		AnnMs:   annMs,
		ExactMs: exactMs,
	}
	for _, r := range exactRows {
		res.ExactIDs = append(res.ExactIDs, r.ID)
		res.ExactDists = append(res.ExactDists, r.Dist)
	}
	return res, nil
}

// runLeg executes one leg: BEGIN READ ONLY on the pool, SET LOCAL gucs, the
// plan proof (EXPLAIN of the IDENTICAL statement text on the SAME connection,
// BEFORE the leg statement — §4.2.4: a once-per-stratum assertion on a
// potentially different pool connection could structurally not see a plan-
// cache bypass of probes 2..N), then the timed leg statement. READ ONLY makes
// any accidental write break hard with SQLSTATE 25006 instead of mutating
// silently (§5.4).
func runLeg(ctx context.Context, pool *pgxpool.Pool, sql string, gucs []string, spec ProbeSpec) (rows []legRow, elapsedMs float64, usedHNSW bool, err error) {
	tx, err := beginLegTx(ctx, pool, gucs)
	if err != nil {
		return nil, 0, false, err
	}
	// Read-only transaction: rollback is the only sensible end either way.
	defer func() { _ = tx.Rollback(ctx) }()

	args := []any{
		pgvec.NewHalfVector(spec.Vec),
		spec.VisibleTypes,
		spec.Scopes,
		spec.SelfID,
		spec.K,
	}

	usedHNSW, err = planUsesHNSW(ctx, tx, sql, args)
	if err != nil {
		return nil, 0, false, fmt.Errorf("plan proof: %w", err)
	}

	start := time.Now()
	r, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, usedHNSW, fmt.Errorf("leg query: %w", err)
	}
	defer r.Close()
	for r.Next() {
		var id string
		var dist *float64
		if err := r.Scan(&id, &dist); err != nil {
			return nil, 0, usedHNSW, fmt.Errorf("leg scan: %w", err)
		}
		if dist == nil {
			// A NULL distance is a row without an embedding: the exact leg's
			// seq scan sorts those last and can surface them when the scope
			// window holds fewer embedded rows than k (production behaves
			// identically — the HNSW leg never returns them because pgvector
			// does not index NULL vectors). Not measurable, dropped.
			continue
		}
		rows = append(rows, legRow{ID: id, Dist: *dist})
	}
	if err := r.Err(); err != nil {
		return nil, 0, usedHNSW, fmt.Errorf("leg rows: %w", err)
	}
	elapsedMs = float64(time.Since(start).Microseconds()) / 1000.0
	return rows, elapsedMs, usedHNSW, nil
}

// beginLegTx opens a READ ONLY transaction and applies the leg's SET LOCAL
// list. All settings are transaction-local — the pool connection returns
// clean at rollback, no session GUC pollution (§5.7).
func beginLegTx(ctx context.Context, pool *pgxpool.Pool, gucs []string) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}) //nolint:forbidigo // Tx-Herausgabe: Leg-Lebenszyklus über mehrere Funktionen (probe.go:233-235)
	if err != nil {
		return nil, fmt.Errorf("begin read-only tx: %w", err)
	}
	for _, g := range gucs {
		if _, err := tx.Exec(ctx, g); err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("apply %q: %w", g, err)
		}
	}
	return tx, nil
}

// planUsesHNSW runs EXPLAIN (FORMAT JSON) of the exact leg statement text
// (marker included) inside the leg's own transaction and reports whether the
// plan tree contains the HNSW index. With plan_cache_mode=force_custom_plan
// active the EXPLAIN plan is representative for the following execution;
// cost: pure planning, no execute.
func planUsesHNSW(ctx context.Context, tx pgx.Tx, sql string, args []any) (bool, error) {
	var doc []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&doc); err != nil {
		return false, err
	}
	var parsed any
	if err := json.Unmarshal(doc, &parsed); err != nil {
		return false, fmt.Errorf("parse explain json: %w", err)
	}
	return jsonTreeHasIndex(parsed, hnswIndexName), nil
}

// jsonTreeHasIndex walks an arbitrary EXPLAIN JSON tree for an "Index Name"
// equal to name (structural walk, not a string match — robust against
// whitespace/format changes).
func jsonTreeHasIndex(node any, name string) bool {
	switch n := node.(type) {
	case map[string]any:
		if v, ok := n["Index Name"]; ok {
			if s, ok := v.(string); ok && s == name {
				return true
			}
		}
		for _, v := range n {
			if jsonTreeHasIndex(v, name) {
				return true
			}
		}
	case []any:
		for _, v := range n {
			if jsonTreeHasIndex(v, name) {
				return true
			}
		}
	}
	return false
}

// computeRecall implements the §4.2.3 definition — ties-robust and small-
// scope-correct. E = exact list (<= k rows: a scope window with fewer than k
// visible blocks yields fewer), n_eff = min(k, |E|), d_ref = distance of the
// LAST row of E, hit = |{a in ANN list : dist(a) <= d_ref + eps}|, recall =
// hit / n_eff. Distance-based instead of an ID intersection so distance ties
// at the k boundary produce no false misses; n_eff-normalized instead of /k
// so small scopes do not appear structurally degraded (with /k a 17-block
// scope at k=75 would sit at ~0.23 forever). Clamped at 1.0: with boundary
// ties the ANN list can legitimately carry MORE than n_eff rows within
// d_ref+eps.
func computeRecall(exact, ann []legRow, k int, eps float64) (recall float64, nEff int) {
	nEff = len(exact)
	if k < nEff {
		nEff = k
	}
	if nEff == 0 {
		return 0, 0
	}
	dRef := exact[nEff-1].Dist
	hit := 0
	for _, a := range ann {
		if a.Dist <= dRef+eps {
			hit++
		}
	}
	recall = float64(hit) / float64(nEff)
	if recall > 1.0 {
		recall = 1.0
	}
	return recall, nEff
}
