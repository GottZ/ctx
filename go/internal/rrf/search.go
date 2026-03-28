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

// Search executes the ctx_rrf PG function with a single SQL call.
// The embedding is passed as pgvector HalfVector, query/querySpaced for FTS,
// scopes for scope filtering, and optional category/tags/limit.
func Search(ctx context.Context, pool *pgxpool.Pool, embedding []float32, query, querySpaced string, scopes []string, category *string, tags []string, limit int) ([]SearchResult, error) {
	if len(embedding) == 0 {
		return nil, fmt.Errorf("rrf: empty embedding")
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("rrf: empty scopes")
	}
	if limit < 1 || limit > 20 {
		limit = 5
	}

	// Build halfvec from float32 slice.
	hv := pgvec.NewHalfVector(embedding)

	// Use native pgx []string for PG TEXT[] parameters (no manual string building).
	var tagsParam interface{}
	if len(tags) > 0 {
		tagsParam = tags
	}

	rows, err := pool.Query(ctx,
		`SELECT rrf_score, cosine_sim, id, title, category, tags, content, scope, updated_at
		 FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7)`,
		hv, query, querySpaced, scopes, category, tagsParam, limit,
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
