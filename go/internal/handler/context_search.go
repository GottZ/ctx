package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SearchHandler handles POST /api/search.
type SearchHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
}

// NewSearchHandler creates a new SearchHandler. The read rate limit comes
// from a config snapshot per request (F1-W7), not a boot copy.
func NewSearchHandler(pool *pgxpool.Pool, cfg ConfigStore) *SearchHandler {
	return &SearchHandler{pool: pool, cfg: cfg}
}

type searchRequest struct {
	Query    string   `json:"query"`
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
	Compact  *bool    `json:"compact"`
	Limit    int      `json:"limit"`
	// After is the keyset-pagination cursor (block-workbench W7) for the
	// empty-query browse path: the {after_updated, after_id} of the last row of
	// the previous page. nil/absent = page 1 (unchanged). Garbage is rejected
	// defensively (400) rather than crashing; the FTS path ignores it.
	After *store.SearchCursor `json:"after"`
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

	// Read rate limit check (0 = disabled).
	if limit := h.cfg.Snapshot().Query.RateLimitRead; limit > 0 {
		readCount, err := store.CheckRateLimitByAction(ctx, h.pool, authResult.ApiKeyID, "query")
		if err != nil {
			slog.Error("search: read rate limit check error", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
			return
		}
		if readCount >= limit {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"success": false,
				"error":   fmt.Sprintf("Rate limit exceeded: max %d reads per 60 seconds", limit),
			})
			return
		}
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

	// Keyset cursor (W7). A cursor with a zero/empty position is treated as
	// page 1 (defensive: a garbage cursor must not crash — it just resumes from
	// the start). The FTS path ignores any cursor (LIMIT-only).
	after := req.After
	if after != nil && (after.ID == "" || after.UpdatedAt.IsZero()) {
		after = nil
	}

	// Search. grantedBlockIDs nil (T40a): /api/search is NOT live-wired for block
	// grants in T40a (its grant wiring is T40b/a later wave) — nil ⇒ no-op OR-arm.
	results, err := store.SearchBlocks(ctx, h.pool, req.Query, authResult.ReadScopes, req.Category, req.Tags, limit, compact, after, nil)
	if err != nil {
		slog.Error("search: query error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	// Next-page cursor (W7). Only the empty-query browse path paginates; the
	// FTS path is "top matches" (no loadMore). nextSearchCursor returns the
	// {after_updated, after_id} of the LAST result when the page came back full
	// (so there may be more), else nil (last page / FTS / not paginating).
	nextAfter := nextSearchCursor(req.Query, results, limit)

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
		// next_after is the cursor for the FOLLOWING page (W7 "Load more"), or
		// null when there is no next page (last page / FTS top-matches mode).
		"next_after": nextAfter,
	})
}

// nextSearchCursor derives the next-page keyset cursor for the empty-query
// browse path (block-workbench W7). When the page came back FULL (len == limit)
// there may be more rows, so it returns the {after_updated, after_id} of the
// LAST result; a short/empty page is the last page (nil). The FTS path (query
// != "") never paginates ("top matches") and always returns nil.
//
func nextSearchCursor(query string, results []store.BlockPreview, limit int) *store.SearchCursor {
	// FTS "top matches" never paginates.
	if query != "" {
		return nil
	}
	// A short page (fewer than the requested limit) is the last page — no more
	// rows, so no next cursor. A full page MAY have more behind it: hand back
	// the last row's (updated_at, id) so the next request resumes after it.
	if len(results) < limit || len(results) == 0 {
		return nil
	}
	last := results[len(results)-1]
	return &store.SearchCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
}
