package rrf

import (
	"context"
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

// SearchResult holds one row from the ctx_rrf PG function.
type SearchResult struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	Category         string    `json:"category"`
	Tags             []string  `json:"tags"`
	Content          string    `json:"content"`
	Scope            string    `json:"scope"`
	UpdatedAt        time.Time `json:"updated_at"`
	RRFScore         float64   `json:"rrf_score"`
	CosineSim        *float64  `json:"cosine_sim,omitempty"`
	RerankScore      *float64  `json:"rerank_score,omitempty"`
	RRFScoreOriginal *float64  `json:"rrf_score_original,omitempty"`

	// Sensitivity is the scope-floor-adjusted content classification, set by
	// the batch lookup AFTER GraphExpand over ALL result IDs (not top-N: a
	// supersedes/graph straggler from rank >50 can advance into the final
	// llmSources — F3 §2.3). Never serialized; zero value acts as credentials
	// inside the trust gate (fail-closed).
	Sensitivity backends.Sensitivity `json:"-"`

	// TypeName is the block's policy type (M073 RETURNS column, WF T5) — the
	// UI-badge/fold consumer input (seam §2.8). Wire-visible since WF T10
	// (wire name `type`, the registry vocabulary). omitempty: graph-hydrated
	// shapes that don't select the column stay byte-identical.
	TypeName string `json:"type,omitempty"`

	// Graph-expansion provenance (GottZ Graph Expansion, Wave 1). All fields
	// are zero-valued for native RRF hits and omitted from JSON (omitempty), so
	// the wire format is byte-identical to pre-Wave-1 when the graph stage is
	// off. ViaGraph is true only for a neighbor that was NEWLY introduced by the
	// traversal (a block RRF did not already return); a native hit that merely
	// got reinforced by a graph edge keeps ViaGraph=false. GraphSeedID /
	// GraphRelationship record which seed edge introduced the neighbor (the
	// highest-injection one when several seeds point at it).
	ViaGraph          bool   `json:"via_graph,omitempty"`
	GraphSeedID       string `json:"graph_seed_id,omitempty"`
	GraphRelationship string `json:"graph_relationship,omitempty"`

	// MatchedComment is the aggregate-to-parent fold's issue-specific response
	// contract (Achse-02 I-E, design/02 §4.4): when a comment folds onto its
	// parent issue, the delivered issue row carries the best-ranked child comment
	// (id + content preview) so the caller sees WHY the issue ranked — the passage
	// that actually hit. Set ONLY on a folded parent (nil for native hits and for
	// a child kept raw), so omitempty keeps every non-fold wire shape byte-
	// identical. Never serialized as the child itself (the child folded away).
	MatchedComment *MatchedComment `json:"matched_comment,omitempty"`
}

// MatchedComment is the fold attribution attached to a folded parent issue
// (design/02 §4.4). Preview is a rune-safe truncation of the child comment body.
type MatchedComment struct {
	ID      string `json:"id"`
	Preview string `json:"preview"`
}

// Search executes the ctx_rrf PG function with a single SQL call.
// The embedding is passed as pgvector HalfVector, query/querySpaced for FTS,
// scopes for scope filtering, and optional category/tags/limit/temporal.
// temporal is a websearch_to_tsquery OR string for date expansion (may be empty).
// queryOR is an OR-joined query for broader FTS recall (may be empty).
//
// Type policy (WF T5, M073 — design/01 §3.5): visibleTypes is the retrieval
// ALLOWLIST (p_types_visible), sourced from the registry snapshot
// (blocktype.Set.VisibleTypes). Fail-closed HARD: an empty/nil list is a Go
// error here (analogous to the empty-scopes reject) — SQL would return 0
// rows, but a silent empty result would mask a wiring bug. dampedTypes/
// dampedFactors are the parallel damping arrays (Set.DampedTypesFor — an
// intent-lifted type is absent, factor 1.0 via COALESCE); they replace the
// former scalar auditTrailFactor (Welle 41 M039). typesExclude is the
// request-level opt-in exclude (wire field block_roles_exclude, seam 17).
// categoriesExclude (v2.0.0 C2 / M048): optional exclude-list. Empty slice =
// no-op (NULL passed to SQL). Trigger: CRAG Bench Session 38c
// topic-map-private slot-stealing in 4/10 movie queries.
//
// grantedBlockIDs (T40b, design/07 §4.2): the resolved block-grant set for the
// caller's tenant — the row-level read-share OR-arm on the SQL retrieval side.
// nil/empty slice → NULL passed (no-op OR-arm, byte-identical to the scope-only
// state); the empty-scope reject (len(scopes)==0) stays HARD and is never
// relaxed by a non-empty grant set (§5.3.1: scope-gate is the primary
// fail-closed point, the grant arm is strictly additive).
//
// Temporal gravity is applied Post-RRF in the handler layer via
// ApplyGravityBoost (linear) and ApplyCyclicGravityBoost (multi-dim cyclic).
// The 5th RRF channel was removed in M020 (never activated from Go).
//
// policy is the semantic strategy policy (Achse 02, design/02 §4.2). The ZERO
// VALUE is off and reproduces the Ist path exactly: no probe roundtrip, the
// legacy 15-argument ctx_rrf statement, Decision{ann, disabled}. The returned
// SelectorDecision is the Achse-01 correlation input (slog + access-log
// metadata are wired in W02-3) — it carries strategy metadata only, never
// content (§5.5).
func Search(ctx context.Context, pool *pgxpool.Pool, embedding []float32, query, querySpaced string, scopes []string, category *string, tags []string, limit int, temporal string, queryOR string, visibleTypes []string, dampedTypes []string, dampedFactors []float64, categoriesExclude []string, typesExclude []string, grantedBlockIDs []string, policy SelectorPolicy) ([]SearchResult, SelectorDecision, error) {
	// Fail-closed core BEFORE the selector (§5.3): empty scopes / empty type
	// allowlist reject, and no probe ever runs for a request that must not
	// retrieve anything.
	if len(embedding) == 0 {
		return nil, SelectorDecision{}, fmt.Errorf("rrf: empty embedding")
	}
	if len(scopes) == 0 {
		return nil, SelectorDecision{}, fmt.Errorf("rrf: empty scopes")
	}
	// Fail-closed allowlist guard (§3.5 invariant 1): NULL/empty
	// p_types_visible means 0 hits by design — a caller that reaches this
	// point without a resolved type set has a wiring bug, surface it loudly.
	if len(visibleTypes) == 0 {
		return nil, SelectorDecision{}, fmt.Errorf("rrf: empty visible-types allowlist (block-type registry not wired?)")
	}
	if len(dampedTypes) != len(dampedFactors) {
		return nil, SelectorDecision{}, fmt.Errorf("rrf: damped types/factors length mismatch (%d != %d)", len(dampedTypes), len(dampedFactors))
	}
	if limit < 1 || limit > 200 {
		limit = 5
	}

	// Build halfvec from float32 slice.
	hv := pgvec.NewHalfVector(embedding)

	// Use native pgx []string for PG TEXT[] parameters (no manual string building).
	var tagsParam interface{}
	if len(tags) > 0 {
		tagsParam = tags
	}

	// Pass temporal as NULL if empty (PG function default).
	var temporalParam interface{}
	if temporal != "" {
		temporalParam = temporal
	}

	// Pass queryOR as NULL if empty (PG function default).
	var queryORParam interface{}
	if queryOR != "" {
		queryORParam = queryOR
	}

	// v2.0.0 C2 (M048): empty slice → NULL (SQL default = no-op exclude).
	var categoriesExcludeParam interface{}
	if len(categoriesExclude) > 0 {
		categoriesExcludeParam = categoriesExclude
	}
	var typesExcludeParam interface{}
	if len(typesExclude) > 0 {
		typesExcludeParam = typesExclude
	}

	// Damping arrays (M073): empty → NULL/NULL (unnest of NULL arrays yields
	// zero rows → COALESCE factor 1.0 everywhere, no damping).
	var dampedTypesParam, dampedFactorsParam interface{}
	if len(dampedTypes) > 0 {
		dampedTypesParam = dampedTypes
		dampedFactorsParam = dampedFactors
	}

	// T40b (design/07 §4.2): empty/nil grant set → NULL (SQL DEFAULT NULL = no-op
	// OR-arm via the `p_granted_block_ids IS NOT NULL` guard, byte-identical to
	// the scope-only state — M048 empty→NULL convention).
	var grantedBlockIDsParam interface{}
	if len(grantedBlockIDs) > 0 {
		grantedBlockIDsParam = grantedBlockIDs
	}

	// Achse 02: the strategy dispatch. The policy clamps (§5.4) are applied
	// ONCE here so the warn logs fire once per search, not once per derivation.
	if policy.Enabled {
		policy = clampPolicy(policy)
	}
	probe := func(ctx context.Context, scopes []string, limit int) (int, error) {
		return boundedProbe(ctx, pool, scopes, limit)
	}
	decision := decide(ctx, probe, scopes, grantedBlockIDs, policy)

	exec := func(ctx context.Context, mode string, scanTuples, exactCap any) ([]SearchResult, error) {
		args := []any{hv, query, querySpaced, scopes, category, tagsParam, limit, temporalParam, queryORParam,
			visibleTypes, dampedTypesParam, dampedFactorsParam, categoriesExcludeParam, typesExcludeParam, grantedBlockIDsParam}
		sql := rrfQueryLegacy
		if !isIstParams(mode, scanTuples, exactCap) {
			// Only a non-Ist strategy needs the Gen-15 parameter surface. The
			// Ist path keeps the literal legacy statement — byte-identical wire
			// behaviour AND callable against a pre-Gen-15 function.
			sql = rrfQueryGen15
			args = append(args, mode, scanTuples, exactCap)
		}
		return queryRRF(ctx, pool, sql, args)
	}

	return runSelected(ctx, decision, policy, exec)
}

const rrfQueryCols = `SELECT rrf_score, cosine_sim, id, title, category, tags, content, scope, updated_at, type_name`

// rrfQueryLegacy is the Ist statement (15 arguments, Gen-15 parameters left on
// their DEFAULTS: 'ann'/NULL/NULL). Unchanged since Gen 3.
const rrfQueryLegacy = rrfQueryCols + `
		 FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9, $10::text[], $11::text[], $12::float8[], $13::text[], $14::text[], $15::uuid[])`

// rrfQueryGen15 carries the explicit strategy surface (migration 112 §3.2):
// $16 p_semantic_mode, $17 p_scan_tuples, $18 p_exact_cap.
const rrfQueryGen15 = rrfQueryCols + `
		 FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9, $10::text[], $11::text[], $12::float8[], $13::text[], $14::text[], $15::uuid[], $16::text, $17::int, $18::int)`

// queryRRF runs one ctx_rrf statement and scans the result rows.
func queryRRF(ctx context.Context, pool *pgxpool.Pool, sql string, args []any) ([]SearchResult, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("rrf: query ctx_rrf: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var cosineSim *float64
		err := rows.Scan(
			&r.RRFScore,
			&cosineSim,
			&r.ID,
			&r.Title,
			&r.Category,
			&r.Tags,
			&r.Content,
			&r.Scope,
			&r.UpdatedAt,
			&r.TypeName,
		)
		if err != nil {
			return nil, fmt.Errorf("rrf: scan row: %w", err)
		}
		r.CosineSim = cosineSim
		results = append(results, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rrf: rows iteration: %w", err)
	}

	return results, nil
}
