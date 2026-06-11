package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BlobHandler handles all blob storage endpoints.
type BlobHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
}

// NewBlobHandler creates a new BlobHandler. The write rate limit comes from
// a config snapshot per request (F1-W7), not a boot copy.
func NewBlobHandler(pool *pgxpool.Pool, cfg ConfigStore) *BlobHandler {
	return &BlobHandler{pool: pool, cfg: cfg}
}

// -- blob-store --.

type blobStoreRequest struct {
	File     string         `json:"file"`     // base64 encoded data
	Filename string         `json:"filename"`
	Category string         `json:"category"`
	Title    string         `json:"title"`
	MimeType string         `json:"mime_type"`
	Tags     []string       `json:"tags"`
	Metadata map[string]any `json:"metadata"`
	Scope    string         `json:"scope"`
}

// HandleBlobStore processes POST /api/blob/store.
func (h *BlobHandler) HandleBlobStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	var req blobStoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("blob-store: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// Validate required fields.
	if req.File == "" || req.Filename == "" || req.Category == "" || req.Title == "" || req.MimeType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required fields: file, filename, category, title, mime_type",
		})
		return
	}

	// Size limits.
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

	// OOM pre-check: reject oversized base64 before allocating decode buffer.
	// 70MB base64 ≈ 52.5MB decoded, which exceeds the 50MB limit.
	if len(req.File) > 70*1024*1024 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": "File exceeds 50MB limit",
		})
		return
	}

	// Decode base64 data.
	data, err := base64.StdEncoding.DecodeString(req.File)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid base64 encoding in 'file' field",
		})
		return
	}

	// Check blob size limit (50 MB).
	if len(data) > 50*1024*1024 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": "File exceeds 50 MB limit",
		})
		return
	}

	// Scope validation: write scope must be home_scope or "shared" if allowed.
	writeScope := authResult.HomeScope
	if req.Scope != "" {
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
	if limit := h.cfg.Snapshot().Query.RateLimitWrite; limit > 0 {
		writeCount, err := store.CheckRateLimit(ctx, h.pool, authResult.ApiKeyID)
		if err != nil {
			slog.Error("blob-store: rate limit check error", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "Internal server error",
			})
			return
		}
		if writeCount >= limit {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"success": false, "error": fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", limit),
			})
			return
		}
	}

	// Execute upsert.
	blob, err := store.UpsertBlob(ctx, h.pool, req.Category, req.Title, req.Filename, req.MimeType, writeScope, data, req.Tags, req.Metadata)
	if err != nil {
		slog.Error("blob-store: upsert error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	// Log write (fire-and-forget).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if logErr := store.LogAccess(bgCtx, h.pool, authResult.ApiKeyID, blob.ID, "blob-write"); logErr != nil {
			slog.Error("blob-store: write log error", "error", logErr, "request_id", reqID)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"blob":    blob,
	})
}

// -- blob-fetch --.

type blobFetchRequest struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Title    string `json:"title"`
	MetaOnly bool   `json:"meta_only"`
}

// HandleBlobFetch processes POST /api/blob/fetch.
func (h *BlobHandler) HandleBlobFetch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	var req blobFetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("blob-fetch: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// Must provide id OR category+title.
	if req.ID == "" && (req.Category == "" || req.Title == "") {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Provide 'id' or both 'category' and 'title'",
		})
		return
	}

	var blob *store.Blob
	var err error
	if req.ID != "" {
		blob, err = store.GetBlobByID(ctx, h.pool, req.ID, authResult.ReadScopes, req.MetaOnly)
	} else {
		blob, err = store.GetBlobByCategoryTitle(ctx, h.pool, req.Category, req.Title, authResult.ReadScopes, req.MetaOnly)
	}
	if err != nil {
		slog.Error("blob-fetch: query error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if blob == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Blob not found",
		})
		return
	}

	// Build response. Encode data as base64 if present.
	resp := map[string]any{
		"success":      true,
		"id":           blob.ID,
		"category":     blob.Category,
		"title":        blob.Title,
		"filename":     blob.Filename,
		"mime_type":    blob.MimeType,
		"file_size":    blob.FileSize,
		"checksum":     blob.Checksum,
		"storage_type": blob.StorageType,
		"tags":         blob.Tags,
		"metadata":     blob.Metadata,
		"scope":        blob.Scope,
		"created_at":   blob.CreatedAt,
		"updated_at":   blob.UpdatedAt,
	}
	if !req.MetaOnly && blob.Data != nil {
		resp["file"] = base64.StdEncoding.EncodeToString(blob.Data)
	}

	writeJSON(w, http.StatusOK, resp)
}

// -- blob-search --.

type blobSearchRequest struct {
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
	MimeType string   `json:"mime_type"`
	Limit    int      `json:"limit"`
}

// HandleBlobSearch processes POST /api/blob/search.
func (h *BlobHandler) HandleBlobSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	var req blobSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("blob-search: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	results, err := store.SearchBlobs(ctx, h.pool, authResult.ReadScopes, req.Category, req.Tags, req.MimeType, req.Limit)
	if err != nil {
		slog.Error("blob-search: query error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"count":   len(results),
		"results": results,
	})
}

// -- blob-manage --.

type blobManageRequest struct {
	Action string `json:"action"`
	ID     string `json:"id"`
	Limit  int    `json:"limit"`
}

// HandleBlobManage processes POST /api/blob/manage.
func (h *BlobHandler) HandleBlobManage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	var req blobManageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("blob-manage: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	switch req.Action {
	case "stats":
		h.handleBlobStats(w, r, authResult)
	case "get":
		h.handleBlobGet(w, r, authResult, req)
	case "list":
		h.handleBlobList(w, r, authResult, req)
	case "delete":
		h.handleBlobDelete(w, r, authResult, req)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "Unknown action (valid: stats, get, list, delete)",
		})
	}
}

func (h *BlobHandler) handleBlobStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	stats, err := store.GetBlobStats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("blob-manage: stats error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"stats":   stats,
	})
}

func (h *BlobHandler) handleBlobGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req blobManageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}

	blob, err := store.GetBlobByID(ctx, h.pool, req.ID, ar.ReadScopes, true)
	if err != nil {
		slog.Error("blob-manage: get error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if blob == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Blob not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"blob":    blob,
	})
}

func (h *BlobHandler) handleBlobList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req blobManageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	blobs, err := store.ListBlobs(ctx, h.pool, ar.ReadScopes, req.Limit)
	if err != nil {
		slog.Error("blob-manage: list error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"count":   len(blobs),
		"blobs":   blobs,
	})
}

func (h *BlobHandler) handleBlobDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req blobManageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}

	blob, err := store.DeleteBlob(ctx, h.pool, req.ID, ar.HomeScope)
	if err != nil {
		slog.Error("blob-manage: delete error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if blob == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Blob not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"deleted": blob,
	})
}
