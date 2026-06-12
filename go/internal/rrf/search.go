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
// auditTrailFactor controls audit-trail damping (Welle 41 M039): pass 1.0
// for no damping (audit-target queries), 0.3 for generic queries.
// categoriesExclude / blockRolesExclude (v2.0.0 C2 / M048): optional
// exclude-lists. Empty slice = no-op (NULL passed to SQL). Trigger: CRAG
// Bench Session 38c topic-map-private slot-stealing in 4/10 movie queries.
//
// Temporal gravity is applied Post-RRF in the handler layer via
// ApplyGravityBoost (linear) and ApplyCyclicGravityBoost (multi-dim cyclic).
// The 5th RRF channel was removed in M020 (never activated from Go).
func Search(ctx context.Context, pool *pgxpool.Pool, embedding []float32, query, querySpaced string, scopes []string, category *string, tags []string, limit int, temporal string, queryOR string, auditTrailFactor float64, categoriesExclude []string, blockRolesExclude []string) ([]SearchResult, error) {
	if len(embedding) == 0 {
		return nil, fmt.Errorf("rrf: empty embedding")
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("rrf: empty scopes")
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
	var blockRolesExcludeParam interface{}
	if len(blockRolesExclude) > 0 {
		blockRolesExcludeParam = blockRolesExclude
	}

	rows, err := pool.Query(ctx,
		`SELECT rrf_score, cosine_sim, id, title, category, tags, content, scope, updated_at
		 FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9, $10, $11::text[], $12::text[])`,
		hv, query, querySpaced, scopes, category, tagsParam, limit, temporalParam, queryORParam, auditTrailFactor, categoriesExcludeParam, blockRolesExcludeParam,
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
