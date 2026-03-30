package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ManageHandler handles POST /api/manage.
type ManageHandler struct {
	pool       *pgxpool.Pool
	ollamaHost string
	embedModel string
}

// NewManageHandler creates a new ManageHandler.
func NewManageHandler(pool *pgxpool.Pool, ollamaHost, embedModel string) *ManageHandler {
	return &ManageHandler{
		pool:       pool,
		ollamaHost: ollamaHost,
		embedModel: embedModel,
	}
}

type manageRequest struct {
	Action   string          `json:"action"`
	ID       string          `json:"id"`
	Data     json.RawMessage `json:"data"`
	Category string          `json:"category"`
	Status   string          `json:"status"`
	Limit    int             `json:"limit"`
}

// HandleManage dispatches CRUD and guard management actions.
func (h *ManageHandler) HandleManage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// Parse body.
	var req manageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("manage: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	switch req.Action {
	case "stats":
		h.handleStats(w, r, authResult)
	case "get":
		h.handleGet(w, r, authResult, req)
	case "list-categories":
		h.handleListCategories(w, r, authResult)
	case "list-meta":
		h.handleListMeta(w, r, authResult)
	case "update":
		h.handleUpdate(w, r, authResult, req)
	case "delete":
		h.handleDelete(w, r, authResult, req)
	case "guard-list":
		h.handleGuardList(w, r, authResult, req)
	case "guard-stats":
		h.handleGuardStats(w, r, authResult)
	case "guard-resolve":
		h.handleGuardResolve(w, r, authResult, req)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "Unknown action",
		})
	}
}

func (h *ManageHandler) handleStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	stats, err := store.GetStats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: stats error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "stats",
		"success": true,
		"stats":   stats,
	})
}

func (h *ManageHandler) handleGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}

	// Log access.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.LogAccess(bgCtx, h.pool, ar.ApiKeyID, req.ID, "manage-get"); err != nil {
			slog.Error("manage: access log error", "error", err, "request_id", reqID)
		}
	}()

	block, err := store.GetBlock(ctx, h.pool, req.ID, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: get error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "get",
		"success": true,
		"block":   block,
	})
}

func (h *ManageHandler) handleListCategories(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	categories, err := store.ListCategories(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: list-categories error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":     "list-categories",
		"success":    true,
		"categories": categories,
	})
}

func (h *ManageHandler) handleListMeta(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	blocks, err := store.ListMeta(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: list-meta error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "list-meta",
		"success": true,
		"blocks":  blocks,
	})
}

func (h *ManageHandler) handleUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}
	if len(req.Data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: data",
		})
		return
	}

	var data store.UpdateBlockData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		slog.Warn("manage: invalid update data", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid data format",
		})
		return
	}

	// Size limits (match context_store.go limits).
	if data.Category != nil && len(*data.Category) > 100 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": "Category exceeds 100 characters",
		})
		return
	}
	if data.Title != nil && len(*data.Title) > 500 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": "Title exceeds 500 characters",
		})
		return
	}
	if data.Content != nil && len(*data.Content) > 50*1024 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": "Content exceeds 50KB",
		})
		return
	}

	// Scope write restriction on update.
	if data.Scope != nil {
		scope := *data.Scope
		if scope != ar.HomeScope && (scope != "shared" || !contains(ar.AllowedScopes, "shared")) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"success": false, "error": "Cannot set scope to requested value",
			})
			return
		}
	}

	block, needsReEmbed, err := store.UpdateBlock(ctx, h.pool, req.ID, data, ar.HomeScope)
	if err != nil {
		slog.Error("manage: update error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	// Re-extract temporal data when content changes.
	if data.Content != nil {
		dates := store.ExtractDates(block.Content)
		if err := store.UpdateContentDates(ctx, h.pool, block.ID, dates); err != nil {
			slog.Error("manage: content_dates update failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
		if len(dates) > 0 {
			if err := store.PopulateTemporal(ctx, h.pool, block.ID, dates); err != nil {
				slog.Error("manage: temporal populate failed", "error", err, "block_id", block.ID, "request_id", reqID)
			}
		}
	}

	// Re-embed if content or title changed.
	if needsReEmbed {
		embedText := block.Title + "\n\n" + block.Content
		vec, err := embed.Embed(ctx, h.ollamaHost, h.embedModel, embedText, embed.PrefixDocument)
		if err != nil {
			slog.Error("manage: re-embed failed", "error", err, "block_id", block.ID, "request_id", reqID)
			// Return success but with warning.
			writeJSON(w, http.StatusOK, map[string]any{
				"action":  "update",
				"success": true,
				"block":   block,
				"warning": "Re-embedding failed",
			})
			return
		}
		if err := store.StoreEmbedding(ctx, h.pool, block.ID, vec); err != nil {
			slog.Error("manage: re-embed store failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "update",
		"success": true,
		"block":   block,
	})
}

func (h *ManageHandler) handleDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}

	block, err := store.DeleteBlock(ctx, h.pool, req.ID, ar.HomeScope)
	if err != nil {
		slog.Error("manage: delete error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "delete",
		"success": true,
		"deleted": block,
	})
}

func (h *ManageHandler) handleGuardList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}

	items, err := store.GuardList(ctx, h.pool, ar.ReadScopes, req.Category, req.Status, limit)
	if err != nil {
		slog.Error("manage: guard-list error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "guard-list",
		"success": true,
		"count":   len(items),
		"blocks":  items,
	})
}

func (h *ManageHandler) handleGuardStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	stats, err := store.GetGuardStats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: guard-stats error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	resp := map[string]any{
		"action":           "guard-stats",
		"success":          true,
		"total_blocks":     stats.TotalBlocks,
		"active":           stats.Active,
		"clean":            stats.Clean,
		"needs_review":     stats.NeedsReview,
		"near_duplicate":   stats.NearDuplicate,
		"unchecked":        stats.Unchecked,
		"archived_dups":    stats.ArchivedDups,
		"write_log_entries": stats.WriteLogEntries,
		"dirty_since":      stats.DirtySince,
		"last_guard_at":    stats.LastGuardAt,
		"pending_count":    stats.PendingCount,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *ManageHandler) handleGuardResolve(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}

	// Parse resolution from data.
	var resolveData struct {
		Resolution string `json:"resolution"`
	}
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &resolveData); err != nil {
			slog.Warn("manage: invalid resolve data", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid data format",
			})
			return
		}
	}

	if resolveData.Resolution != "archive" && resolveData.Resolution != "keep" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Resolution must be 'archive' or 'keep'",
		})
		return
	}

	block, err := store.GuardResolve(ctx, h.pool, req.ID, resolveData.Resolution, ar.HomeScope)
	if err != nil {
		slog.Error("manage: guard-resolve error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	// Map guard_status to resolution string for response.
	resolution := "keep"
	if block.GuardStatus == "archived_dup" {
		resolution = "archive"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":     "guard-resolve",
		"success":    true,
		"resolved":   block,
		"resolution": resolution,
	})
}
