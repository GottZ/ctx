package handler

import (
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/digest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DigestHandler handles POST /api/digest.
type DigestHandler struct {
	pool *pgxpool.Pool
}

// NewDigestHandler creates a new DigestHandler.
func NewDigestHandler(pool *pgxpool.Pool) *DigestHandler {
	return &DigestHandler{pool: pool}
}

// HandleDigest triggers a topic map rebuild for the authenticated scope.
func (h *DigestHandler) HandleDigest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// Run digest.
	err := digest.RunDigest(ctx, h.pool, authResult.HomeScope, authResult.ReadScopes)
	if err != nil {
		slog.Error("digest: run error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Digest generation failed",
		})
		return
	}

	// Count blocks and categories for the response.
	var blockCount, categoryCount int
	var contentLength int
	_ = h.pool.QueryRow(ctx,
		`SELECT
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived), 0),
			COALESCE((SELECT count(DISTINCT category)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived), 0),
			COALESCE((SELECT length(content)::int FROM context_blocks WHERE category = 'index' AND title = 'topic-map-' || $2 AND NOT is_archived LIMIT 1), 0)`,
		authResult.ReadScopes, authResult.HomeScope,
	).Scan(&blockCount, &categoryCount, &contentLength)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"action":        "digest",
		"scope":         authResult.HomeScope,
		"title":         "topic-map-" + authResult.HomeScope,
		"blockCount":    blockCount,
		"categoryCount": categoryCount,
		"contentLength": contentLength,
	})
}
