package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QueryHandler handles POST /api/query and /webhook/context-agent.
type QueryHandler struct {
	pool           *pgxpool.Pool
	ollamaHost     string
	embedModel     string
	chatModel      string
	rerankEnabled  bool
}

// NewQueryHandler creates a new QueryHandler.
func NewQueryHandler(pool *pgxpool.Pool, ollamaHost, embedModel, chatModel string, rerankEnabled bool) *QueryHandler {
	return &QueryHandler{
		pool:          pool,
		ollamaHost:    ollamaHost,
		embedModel:    embedModel,
		chatModel:     chatModel,
		rerankEnabled: rerankEnabled,
	}
}

// queryRequest is the JSON body for the query endpoint.
type queryRequest struct {
	Query    string   `json:"query"`
	Category *string  `json:"category,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Limit    *int     `json:"limit,omitempty"`
}

// queryResponse is the JSON response from the query endpoint.
type queryResponse struct {
	Success    bool             `json:"success"`
	Answer     string           `json:"answer"`
	Sources    []sourceResponse `json:"sources"`
	Confidence string           `json:"confidence"`
	Model      string           `json:"model,omitempty"`
	EvalCount  int              `json:"eval_count,omitempty"`
	Translated bool             `json:"translated"`
}

// sourceResponse is a single source in the query response.
type sourceResponse struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Category         string   `json:"category"`
	Score            float64  `json:"score"`
	AgeDays          int      `json:"age_days"`
	RerankScore      *float64 `json:"rerank_score,omitempty"`
	RRFScoreOriginal *float64 `json:"rrf_score_original,omitempty"`
}

// HandleQuery orchestrates the full query pipeline:
// parse -> auth -> detect language -> translate -> embed -> RRF search ->
// filter -> confidence -> reorder -> synthesize -> access log -> respond.
func (h *QueryHandler) HandleQuery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := RequestIDFromContext(ctx)

	// Step 1: Parse JSON body.
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("invalid request body",
			"error", err,
			"request_id", requestID,
		)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query is required"})
		return
	}

	// Query size limit (10 KB).
	if len(req.Query) > 10*1024 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Query exceeds 10KB limit"})
		return
	}

	// Step 2: Auth (from middleware context).
	ar := AuthResultFromContext(ctx)
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// Clamp limit: 1-20, default 5.
	limit := 5
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 {
			limit = 1
		}
		if limit > 20 {
			limit = 20
		}
	}

	slog.Info("query pipeline start",
		"query", query,
		"request_id", requestID,
	)

	// Step 3: Detect German and translate if needed.
	originalQuery := query
	searchQuery := query
	translated := false

	if llm.DetectGerman(query) {
		slog.Info("german detected, translating",
			"request_id", requestID,
		)
		translatedQuery, err := llm.TranslateQuery(ctx, h.ollamaHost, h.chatModel, query)
		if err != nil {
			slog.Warn("translation failed, using original",
				"error", err,
				"request_id", requestID,
			)
		} else if translatedQuery != query {
			searchQuery = translatedQuery
			translated = true
			slog.Info("query translated",
				"original", query,
				"translated", searchQuery,
				"request_id", requestID,
			)
		}
	}

	// Step 4: Embed the search query with query prefix.
	embedding, err := embed.Embed(ctx, h.ollamaHost, h.embedModel, searchQuery, embed.PrefixQuery)
	if err != nil {
		slog.Error("embedding failed",
			"error", err,
			"request_id", requestID,
		)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding failed"})
		return
	}

	// Step 5: Hyphen preprocessing for FTS.
	querySpaced := strings.ReplaceAll(searchQuery, "-", " ")

	// Step 6: RRF Search via PG function.
	results, err := rrf.Search(ctx, h.pool, embedding, searchQuery, querySpaced, ar.ReadScopes, req.Category, req.Tags, limit)
	if err != nil {
		slog.Error("rrf search failed",
			"error", err,
			"request_id", requestID,
		)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}

	slog.Info("rrf search complete",
		"result_count", len(results),
		"request_id", requestID,
	)

	// Step 6b: Rerank via LLM (skipped if disabled or fewer than 3 results).
	if h.rerankEnabled {
		results, err = rrf.Rerank(ctx, h.ollamaHost, h.chatModel, originalQuery, results)
		if err != nil {
			slog.Warn("rerank failed, using original order",
				"error", err,
				"request_id", requestID,
			)
		}
	}

	// Step 7: Convert RRF results to LLM source format.
	now := time.Now()
	sources := make([]llm.Source, len(results))
	for i, r := range results {
		ageDays := int(math.Floor(now.Sub(r.UpdatedAt).Hours() / 24))
		if ageDays < 0 {
			ageDays = 0
		}
		sources[i] = llm.Source{
			ID:               r.ID,
			Title:            r.Title,
			Category:         r.Category,
			Content:          r.Content,
			Score:            r.RRFScore,
			RerankScore:      r.RerankScore,
			RRFScoreOriginal: r.RRFScoreOriginal,
			AgeDays:          ageDays,
		}
	}

	// Step 8: Synthesize (filter, confidence, reorder, LLM call).
	// Uses originalQuery so the LLM answers in the user's language.
	synthResult, err := llm.Synthesize(ctx, h.ollamaHost, h.chatModel, originalQuery, sources)
	if err != nil {
		slog.Error("synthesis failed",
			"error", err,
			"request_id", requestID,
		)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "synthesis failed"})
		return
	}

	// Step 9: Access log (async, non-blocking).
	go h.logAccess(ar, results, query)

	// Step 10: Build response.
	respSources := make([]sourceResponse, len(synthResult.Sources))
	for i, s := range synthResult.Sources {
		respSources[i] = sourceResponse{
			ID:               s.ID,
			Title:            s.Title,
			Category:         s.Category,
			Score:            s.Score,
			AgeDays:          s.AgeDays,
			RerankScore:      s.RerankScore,
			RRFScoreOriginal: s.RRFScoreOriginal,
		}
	}

	resp := queryResponse{
		Success:    true,
		Answer:     synthResult.Answer,
		Sources:    respSources,
		Confidence: synthResult.Confidence,
		Model:      synthResult.Model,
		EvalCount:  synthResult.EvalCount,
		Translated: translated,
	}

	// Set model even when LLM was skipped (for consistency).
	if resp.Model == "" {
		resp.Model = h.chatModel
	}

	slog.Info("query pipeline complete",
		"confidence", resp.Confidence,
		"source_count", len(respSources),
		"eval_count", resp.EvalCount,
		"translated", translated,
		"request_id", requestID,
	)

	writeJSON(w, http.StatusOK, resp)
}

// logAccess inserts access log entries asynchronously.
// One row per block returned in the search results.
// Errors are logged but never surface to the caller.
func (h *QueryHandler) logAccess(ar *auth.AuthResult, results []rrf.SearchResult, queryText string) {
	if ar == nil || ar.ApiKeyID == "" || len(results) == 0 {
		return
	}

	// Use a background context with generous timeout — the request context may be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, r := range results {
		metadata := map[string]interface{}{
			"score":  math.Round(r.RRFScore*10000) / 10000,
			"scope":  r.Scope,
			"source": "agent",
		}
		metaJSON, err := json.Marshal(metadata)
		if err != nil {
			slog.Warn("access log: marshal metadata failed",
				"error", err,
				"block_id", r.ID,
			)
			continue
		}

		_, err = h.pool.Exec(ctx,
			`INSERT INTO context_access_log (api_key_id, block_id, action, query_text, metadata)
			 VALUES ($1, $2, 'query', $3, $4)`,
			ar.ApiKeyID, r.ID, queryText, string(metaJSON),
		)
		if err != nil {
			slog.Warn("access log: insert failed",
				"error", err,
				"block_id", r.ID,
			)
		}
	}
}
