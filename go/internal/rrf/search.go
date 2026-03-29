package rrf

import (
	"context"
	"fmt"
	"time"

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
}

// TemporalGravityParams holds parameters for the 5th RRF temporal gravity channel.
type TemporalGravityParams struct {
	Date      string // ISO date, e.g. "2026-03-28"
	Direction string // "past", "future", "both"
	Cutoff    int    // days, e.g. 14 for weekday, 60 for month
}

// Search executes the ctx_rrf PG function with a single SQL call.
// The embedding is passed as pgvector HalfVector, query/querySpaced for FTS,
// scopes for scope filtering, and optional category/tags/limit/temporal.
// temporal is a websearch_to_tsquery OR string for date expansion (may be empty).
// gravity is optional temporal gravity parameters for the 5th RRF channel.
func Search(ctx context.Context, pool *pgxpool.Pool, embedding []float32, query, querySpaced string, scopes []string, category *string, tags []string, limit int, temporal string, gravity *TemporalGravityParams) ([]SearchResult, error) {
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

	// Pass gravity params (BUG-7 fix: wire 5th RRF channel).
	var gravDate, gravDir interface{}
	var gravCutoff interface{}
	if gravity != nil {
		gravDate = gravity.Date
		gravDir = gravity.Direction
		gravCutoff = gravity.Cutoff
	}

	rows, err := pool.Query(ctx,
		`SELECT rrf_score, cosine_sim, id, title, category, tags, content, scope, updated_at
		 FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9::date, $10, $11::int)`,
		hv, query, querySpaced, scopes, category, tagsParam, limit, temporalParam,
		gravDate, gravDir, gravCutoff,
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
