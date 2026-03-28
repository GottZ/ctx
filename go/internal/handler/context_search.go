package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SearchHandler handles POST /webhook/context-search.
type SearchHandler struct {
	pool *pgxpool.Pool
}

// NewSearchHandler creates a new SearchHandler.
func NewSearchHandler(pool *pgxpool.Pool) *SearchHandler {
	return &SearchHandler{pool: pool}
}

type searchRequest struct {
	Query    string   `json:"query"`
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
	Compact  *bool    `json:"compact"`
	Limit    int      `json:"limit"`
}

// HandleSearch processes lightweight search requests (no LLM).
func (h *SearchHandler) HandleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// Parse body.
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("search: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// Defaults.
	compact := true
	if req.Compact != nil {
		compact = *req.Compact
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	// Search.
	results, err := store.SearchBlocks(ctx, h.pool, req.Query, authResult.ReadScopes, req.Category, req.Tags, limit, compact)
	if err != nil {
		slog.Error("search: query error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	// Format response.
	var categoryFilter any = nil
	if req.Category != "" {
		categoryFilter = req.Category
	}
	var tagsFilter any = nil
	if len(req.Tags) > 0 {
		tagsFilter = req.Tags
	}
	var queryFilter any = nil
	if req.Query != "" {
		queryFilter = req.Query
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"count":   len(results),
		"compact": compact,
		"filters": map[string]any{
			"query":    queryFilter,
			"category": categoryFilter,
			"tags":     tagsFilter,
			"limit":    limit,
		},
		"results": results,
	})
}
