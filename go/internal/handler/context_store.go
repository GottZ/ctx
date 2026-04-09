package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StoreHandler handles POST /api/store.
type StoreHandler struct {
	pool           *pgxpool.Pool
	rateLimitWrite int // 0 = disabled
}

// NewStoreHandler creates a new StoreHandler.
func NewStoreHandler(pool *pgxpool.Pool, rateLimitWrite int) *StoreHandler {
	return &StoreHandler{
		pool:           pool,
		rateLimitWrite: rateLimitWrite,
	}
}

type storeRequest struct {
	Category string         `json:"category"`
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Tags     []string       `json:"tags"`
	Metadata map[string]any `json:"metadata"`
	Scope    string         `json:"scope"`
}

// HandleStore processes upsert requests with auto-embedding.
func (h *StoreHandler) HandleStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// Parse body.
	var req storeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("store: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// Validate required fields.
	if req.Category == "" || req.Title == "" || req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required fields: category, title, content",
		})
		return
	}

	// Size limits (HTTP 413).
	if len(req.Category) > 100 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": "Category exceeds 100 characters",
		})
		return
	}
	if len(req.Title) > 500 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": "Title exceeds 500 characters",
		})
		return
	}
	if len(req.Content) > 50*1024 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": "Content exceeds 50KB",
		})
		return
	}

	// Scope validation: write scope must be home_scope or "shared" if allowed.
	writeScope := authResult.HomeScope
	scopeExplicit := false
	if req.Scope != "" {
		scopeExplicit = true
		if req.Scope == authResult.HomeScope {
			writeScope = req.Scope
		} else if req.Scope == "shared" && contains(authResult.AllowedScopes, "shared") {
			writeScope = "shared"
		} else {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"success": false, "error": "Cannot write to requested scope",
			})
			return
		}
	}

	// Rate limit check (writes/min, 0 = disabled).
	if h.rateLimitWrite > 0 {
		writeCount, err := store.CheckRateLimit(ctx, h.pool, authResult.ApiKeyID)
		if err != nil {
			slog.Error("store: rate limit check error", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "Internal server error",
			})
			return
		}
		if writeCount >= h.rateLimitWrite {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"success": false, "error": fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", h.rateLimitWrite),
			})
			return
		}
	}

	// Hash NOOP check: skip if identical content already exists.
	existingID, err := store.HashNOOPCheck(ctx, h.pool, req.Content, writeScope, req.Category, req.Title)
	if err != nil {
		slog.Error("store: hash noop check error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if existingID != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":     true,
			"action":      "noop",
			"reason":      "identical_content",
			"existing_id": existingID,
		})
		return
	}

	// Execute upsert.
	block, err := store.UpsertBlock(ctx, h.pool, req.Category, req.Title, req.Content, req.Tags, req.Metadata, writeScope, scopeExplicit)
	if err != nil {
		slog.Error("store: upsert error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	// Log write (fire-and-forget in background).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if logErr := store.LogAccess(bgCtx, h.pool, authResult.ApiKeyID, block.ID, "write"); logErr != nil {
			slog.Error("store: write log error", "error", logErr, "request_id", reqID)
		}
	}()

	// Enrich temporal data (fire-and-forget).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		times := store.ExtractDates(block.Content)
		if err := store.UpdateContentTimes(bgCtx, h.pool, block.ID, times); err != nil {
			slog.Error("store: content_times update failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
		// Always populate: createdAt is included as meta-anchor even without content times.
		if err := store.PopulateTemporal(bgCtx, h.pool, block.ID, times, block.CreatedAt); err != nil {
			slog.Error("store: temporal populate failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}()

	// Embedding generated async by scheduler backfill loop.
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"block":   block,
	})
}

// contains checks if a string slice contains a value.
func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}
