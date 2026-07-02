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
	// UI-badge/fold consumer input (seam §2.8). NOT serialized yet: response
	// exposure is wave T10's contract change, the wire format stays
	// byte-identical here.
	TypeName string `json:"-"`

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
func Search(ctx context.Context, pool *pgxpool.Pool, embedding []float32, query, querySpaced string, scopes []string, category *string, tags []string, limit int, temporal string, queryOR string, visibleTypes []string, dampedTypes []string, dampedFactors []float64, categoriesExclude []string, typesExclude []string, grantedBlockIDs []string) ([]SearchResult, error) {
	if len(embedding) == 0 {
		return nil, fmt.Errorf("rrf: empty embedding")
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("rrf: empty scopes")
	}
	// Fail-closed allowlist guard (§3.5 invariant 1): NULL/empty
	// p_types_visible means 0 hits by design — a caller that reaches this
	// point without a resolved type set has a wiring bug, surface it loudly.
	if len(visibleTypes) == 0 {
		return nil, fmt.Errorf("rrf: empty visible-types allowlist (block-type registry not wired?)")
	}
	if len(dampedTypes) != len(dampedFactors) {
		return nil, fmt.Errorf("rrf: damped types/factors length mismatch (%d != %d)", len(dampedTypes), len(dampedFactors))
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

	rows, err := pool.Query(ctx,
		`SELECT rrf_score, cosine_sim, id, title, category, tags, content, scope, updated_at, type_name
		 FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9, $10::text[], $11::text[], $12::float8[], $13::text[], $14::text[], $15::uuid[])`,
		hv, query, querySpaced, scopes, category, tagsParam, limit, temporalParam, queryORParam, visibleTypes, dampedTypesParam, dampedFactorsParam, categoriesExcludeParam, typesExcludeParam, grantedBlockIDsParam,
	)
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
