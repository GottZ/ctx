package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DreamController is the interface for controlling dream mode from the manage handler.
type DreamController interface {
	SetDreamMode(mode int32, throttleInterval time.Duration)
	GetDreamMode() (mode int32, throttleInterval time.Duration)
}

// ManageHandler handles POST /api/manage.
type ManageHandler struct {
	pool            *pgxpool.Pool
	dreamController DreamController
}

// NewManageHandler creates a new ManageHandler.
func NewManageHandler(pool *pgxpool.Pool, dreamController DreamController) *ManageHandler {
	return &ManageHandler{pool: pool, dreamController: dreamController}
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
	case "dream-stats":
		h.handleDreamStats(w, r, authResult)
	case "dream-review":
		h.handleDreamReview(w, r, authResult)
	case "dream-mode":
		h.handleDreamMode(w, r, req)
	case "mcp-client-create":
		h.handleMCPClientCreate(w, r, authResult, req)
	case "mcp-client-list":
		h.handleMCPClientList(w, r)
	case "mcp-client-delete":
		h.handleMCPClientDelete(w, r, req)
	case "api-key-create":
		h.handleApiKeyCreate(w, r, req)
	case "api-key-list":
		h.handleApiKeyList(w, r)
	case "api-key-delete":
		h.handleApiKeyDelete(w, r, req)
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

	resolvedID, matches, err := store.ResolveBlockID(ctx, h.pool, req.ID, ar.ReadScopes)
	if err != nil {
		if errors.Is(err, store.ErrAmbiguousID) {
			writeJSON(w, http.StatusOK, map[string]any{
				"action":  "get",
				"success": false,
				"error":   "Ambiguous id prefix",
				"matches": matches,
			})
			return
		}
		// Prefix-too-short and other validation errors → 400.
		if strings.Contains(err.Error(), "prefix must be at least") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": err.Error(),
			})
			return
		}
		slog.Error("manage: resolve id error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if resolvedID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	// Log access against the resolved ID, not the user-supplied prefix.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.LogAccess(bgCtx, h.pool, ar.ApiKeyID, resolvedID, "manage-get"); err != nil {
			slog.Error("manage: access log error", "error", err, "request_id", reqID)
		}
	}()

	block, err := store.GetBlock(ctx, h.pool, resolvedID, ar.ReadScopes)
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

	// Prefix-resolve within HomeScope only — writes are scope-restricted, so the
	// candidate set must be too.
	resolvedID, matches, err := store.ResolveBlockID(ctx, h.pool, req.ID, []string{ar.HomeScope})
	if err != nil {
		if errors.Is(err, store.ErrAmbiguousID) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"action":  "update",
				"success": false,
				"error":   "Ambiguous id prefix — re-specify with a longer prefix or full id",
				"matches": matches,
			})
			return
		}
		if strings.Contains(err.Error(), "prefix must be at least") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": err.Error(),
			})
			return
		}
		slog.Error("manage: resolve id error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if resolvedID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	block, needsReEmbed, err := store.UpdateBlock(ctx, h.pool, resolvedID, data, ar.HomeScope)
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
		times := store.ExtractDates(block.Content)
		if err := store.UpdateContentTimes(ctx, h.pool, block.ID, times); err != nil {
			slog.Error("manage: content_times update failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
		// Always populate: createdAt is included as meta-anchor even without content times.
		if err := store.PopulateTemporal(ctx, h.pool, block.ID, times, block.CreatedAt); err != nil {
			slog.Error("manage: temporal populate failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}

	// Clear embedding so scheduler backfill regenerates it.
	if needsReEmbed {
		if err := store.ClearEmbedding(ctx, h.pool, block.ID); err != nil {
			slog.Error("manage: clear embedding failed", "error", err, "block_id", block.ID, "request_id", reqID)
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

	// Prefix-resolve within HomeScope only — see handleUpdate for rationale.
	resolvedID, matches, err := store.ResolveBlockID(ctx, h.pool, req.ID, []string{ar.HomeScope})
	if err != nil {
		if errors.Is(err, store.ErrAmbiguousID) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"action":  "delete",
				"success": false,
				"error":   "Ambiguous id prefix — re-specify with a longer prefix or full id",
				"matches": matches,
			})
			return
		}
		if strings.Contains(err.Error(), "prefix must be at least") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": err.Error(),
			})
			return
		}
		slog.Error("manage: resolve id error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if resolvedID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	block, err := store.DeleteBlock(ctx, h.pool, resolvedID, ar.HomeScope)
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

func (h *ManageHandler) handleDreamStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	total, checked, linked, pendingRecheck, err := dream.Stats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream stats failed",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":          "dream-stats",
		"success":         true,
		"total_blocks":    total,
		"dream_checked":   checked,
		"dream_links":     linked,
		"coverage_pct":    float64(checked) / float64(max(total, 1)) * 100,
		"unchecked":       total - checked,
		"pending_recheck": pendingRecheck,
	})
}

func (h *ManageHandler) handleDreamReview(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()

	// 1. Stats overview.
	total, checked, linked, pendingRecheck, err := dream.Stats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream review failed",
		})
		return
	}

	// 2. Low-confidence links (candidates for human review).
	lowConfLinks, err := h.fetchLowConfidenceLinks(ctx, ar)
	if err != nil {
		slog.Warn("dream-review: low confidence fetch failed", "error", err)
	}

	// 3. Supersedes pairs.
	supersedesPairs, err := h.fetchSupersedesPairs(ctx, ar)
	if err != nil {
		slog.Warn("dream-review: supersedes fetch failed", "error", err)
	}

	// 4. Recently checked blocks (last 10).
	recentBlocks, err := h.fetchRecentDreamBlocks(ctx, ar)
	if err != nil {
		slog.Warn("dream-review: recent blocks fetch failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":           "dream-review",
		"success":          true,
		"total_blocks":     total,
		"dream_checked":    checked,
		"dream_links":      linked,
		"pending_recheck":  pendingRecheck,
		"low_confidence":   lowConfLinks,
		"supersedes_pairs": supersedesPairs,
		"recent_checked":   recentBlocks,
	})
}

func (h *ManageHandler) fetchLowConfidenceLinks(ctx context.Context, ar *auth.AuthResult) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT dl.source_block_id::text, dl.target_block_id::text, dl.relationship,
			dl.raw_confidence, dl.confidence, dl.scope,
			s.title AS source_title, t.title AS target_title
		FROM context_dream_links dl
		JOIN context_blocks s ON s.id = dl.source_block_id
		JOIN context_blocks t ON t.id = dl.target_block_id
		WHERE dl.raw_confidence < 0.7
		  AND dl.scope = ANY($1::text[])
		ORDER BY dl.raw_confidence ASC
		LIMIT 20`,
		ar.ReadScopes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var sourceID, targetID, rel, scope, sourceTitle, targetTitle string
		var rawConfidence, confidence float64
		if err := rows.Scan(&sourceID, &targetID, &rel, &rawConfidence, &confidence, &scope, &sourceTitle, &targetTitle); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"source_id":      sourceID,
			"target_id":      targetID,
			"relationship":   rel,
			"raw_confidence": rawConfidence,
			"confidence":     confidence,
			"source_title":   sourceTitle,
			"target_title":   targetTitle,
		})
	}
	return results, rows.Err()
}

func (h *ManageHandler) fetchSupersedesPairs(ctx context.Context, ar *auth.AuthResult) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT dl.source_block_id::text, dl.target_block_id::text,
			dl.confidence,
			s.title AS source_title, s.quality_score AS source_quality,
			t.title AS target_title, t.quality_score AS target_quality
		FROM context_dream_links dl
		JOIN context_blocks s ON s.id = dl.source_block_id
		JOIN context_blocks t ON t.id = dl.target_block_id
		WHERE dl.relationship = 'supersedes'
		  AND dl.scope = ANY($1::text[])
		ORDER BY dl.confidence DESC
		LIMIT 20`,
		ar.ReadScopes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var sourceID, targetID, sourceTitle, targetTitle string
		var confidence, sourceQuality, targetQuality float64
		if err := rows.Scan(&sourceID, &targetID, &confidence, &sourceTitle, &sourceQuality, &targetTitle, &targetQuality); err != nil {
			return nil, err
		}
		// Welle 46 Convention-Switch (2026-05-22): "A supersedes B" → A=source=newer,
		// B=target=outdated. Source is the new replacement, target is the retired block.
		results = append(results, map[string]any{
			"new_block_id": sourceID,
			"new_title":    sourceTitle,
			"new_quality":  sourceQuality,
			"old_block_id": targetID,
			"old_title":    targetTitle,
			"old_quality":  targetQuality,
			"confidence":   confidence,
		})
	}
	return results, rows.Err()
}

func (h *ManageHandler) fetchRecentDreamBlocks(ctx context.Context, ar *auth.AuthResult) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT cb.id::text, cb.title, cb.category, cb.quality_score, cb.dream_checked_at,
			COALESCE(lc.cnt, 0)::int AS link_count
		FROM context_blocks cb
		LEFT JOIN (
			SELECT block_id, count(*) AS cnt FROM (
				SELECT source_block_id AS block_id FROM context_dream_links
				UNION ALL
				SELECT target_block_id AS block_id FROM context_dream_links
			) sub GROUP BY block_id
		) lc ON lc.block_id = cb.id
		WHERE cb.dream_checked_at IS NOT NULL
		  AND NOT cb.is_archived
		  AND cb.scope = ANY($1::text[])
		ORDER BY cb.dream_checked_at DESC
		LIMIT 10`,
		ar.ReadScopes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id, title, category string
		var quality float64
		var checkedAt time.Time
		var linkCount int
		if err := rows.Scan(&id, &title, &category, &quality, &checkedAt, &linkCount); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"id":             id,
			"title":          title,
			"category":       category,
			"quality_score":  quality,
			"dream_checked":  checkedAt.Format(time.RFC3339),
			"link_count":     linkCount,
		})
	}
	return results, rows.Err()
}

func (h *ManageHandler) handleDreamMode(w http.ResponseWriter, _ *http.Request, req manageRequest) {
	if h.dreamController == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error": "Dream not enabled",
		})
		return
	}

	// No data = return current mode.
	if len(req.Data) == 0 || string(req.Data) == "null" {
		mode, interval := h.dreamController.GetDreamMode()
		writeJSON(w, http.StatusOK, map[string]any{
			"success":  true,
			"mode":     dreamModeStr(mode),
			"interval": interval.Seconds(),
		})
		return
	}

	var data struct {
		Mode     string `json:"mode"`
		Interval int    `json:"interval"` // seconds, 0 = default
	}
	if err := json.Unmarshal(req.Data, &data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid data: expected {mode, interval}",
		})
		return
	}

	var mode int32
	switch data.Mode {
	case "on":
		mode = 0 // DreamModeOn
	case "throttled":
		mode = 1 // DreamModeThrottled
	case "off":
		mode = 2 // DreamModeOff
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid mode: use on, throttled, or off",
		})
		return
	}

	var interval time.Duration
	if data.Interval > 0 {
		interval = time.Duration(data.Interval) * time.Second
	}

	h.dreamController.SetDreamMode(mode, interval)
	_, currentInterval := h.dreamController.GetDreamMode()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"mode":     data.Mode,
		"interval": currentInterval.Seconds(),
	})
}

func dreamModeStr(mode int32) string {
	switch mode {
	case 1:
		return "throttled"
	case 2:
		return "off"
	default:
		return "on"
	}
}

func (h *ManageHandler) handleMCPClientCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var data struct {
		Label string `json:"label"`
	}
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &data)
	}
	if data.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "label is required"})
		return
	}

	client, secret, err := store.CreateOAuthClient(r.Context(), h.pool, data.Label, ar.ApiKeyID)
	if err != nil {
		slog.Error("manage: create oauth client failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"client_id":     client.ClientID,
		"client_secret": secret, // Shown once.
		"label":         client.Label,
	})
}

func (h *ManageHandler) handleMCPClientList(w http.ResponseWriter, r *http.Request) {
	clients, err := store.ListOAuthClients(r.Context(), h.pool)
	if err != nil {
		slog.Error("manage: list oauth clients failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "clients": clients})
}

func (h *ManageHandler) handleMCPClientDelete(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data struct {
		ClientID string `json:"client_id"`
	}
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &data)
	}
	if data.ClientID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "client_id is required"})
		return
	}

	deleted, err := store.DeleteOAuthClient(r.Context(), h.pool, data.ClientID)
	if err != nil {
		slog.Error("manage: delete oauth client failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "client not found or already inactive"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": data.ClientID})
}

// apiKeyCreateRequest is the JSON shape under req.Data for api-key-create.
// home_scope is REQUIRED as of v2.0.0 — empty values yield 400.
type apiKeyCreateRequest struct {
	Label         string   `json:"label"`
	HomeScope     string   `json:"home_scope"`
	AllowedScopes []string `json:"allowed_scopes,omitempty"`
}

func (h *ManageHandler) handleApiKeyCreate(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data apiKeyCreateRequest
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &data); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid data: expected {label, home_scope, allowed_scopes?}",
			})
			return
		}
	}

	if data.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "label is required"})
		return
	}
	// v2.0.0 breaking change: home_scope must be explicit. No default to
	// 'private' — callers must declare the tenant boundary at creation time.
	if data.HomeScope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "home_scope is required",
		})
		return
	}

	key, plaintext, err := store.CreateApiKey(r.Context(), h.pool, data.Label, data.HomeScope, data.AllowedScopes)
	if err != nil {
		slog.Error("manage: create api key failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"id":             key.ID,
		"label":          key.Label,
		"home_scope":     key.HomeScope,
		"allowed_scopes": key.AllowedScopes,
		"api_key":        plaintext, // Shown once.
	})
}

func (h *ManageHandler) handleApiKeyList(w http.ResponseWriter, r *http.Request) {
	keys, err := store.ListApiKeys(r.Context(), h.pool)
	if err != nil {
		slog.Error("manage: list api keys failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "keys": keys})
}

func (h *ManageHandler) handleApiKeyDelete(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data struct {
		ID string `json:"id"`
	}
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &data)
	}
	if data.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id is required"})
		return
	}
	deleted, err := store.DeleteApiKey(r.Context(), h.pool, data.ID)
	if err != nil {
		slog.Error("manage: delete api key failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "key not found or already inactive"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": data.ID})
}
