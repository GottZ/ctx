package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
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

	// Scope validation through writableBlockScopes — the SINGLE eval point of
	// the write gate (078/E4b), shared verbatim with the block write path.
	// This surface used to carry a second, narrower formula of its own
	// (home_scope | 'shared'-if-allowed), which ignored ar.WriteScopes: a key
	// minted with write_scopes=[b] wrote BLOCKS in b but was refused every
	// BLOB in b — one principal, two authorisation answers for one scope
	// (Gap-C0-c). Reusing the function, not restating its formula, is what
	// keeps the two surfaces from drifting apart again; the home_scope,
	// 'shared'-if-allowed and stale-write_scope semantics all come along
	// unchanged because they ARE that function's semantics.
	//
	// The gate stays AHEAD of meterBlobWrite on purpose: a scope the key may
	// not write is refused before any budget is booked, so spraying foreign
	// scopes cannot drain a legitimate key's quota (B2 books the intent
	// synchronously, before the upsert).
	writeScope := authResult.HomeScope
	if req.Scope != "" {
		if !contains(writableBlockScopes(authResult), req.Scope) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"success": false, "error": "Cannot write to requested scope",
			})
			return
		}
		writeScope = req.Scope
	}

	logID, ok := h.meterBlobWrite(ctx, w, authResult, reqID)
	if !ok {
		return
	}

	// Execute upsert.
	blob, err := store.UpsertBlob(ctx, h.pool, req.Category, req.Title, req.Filename, req.MimeType, writeScope, data, req.Tags, req.Metadata)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23") {
			status, reason := blobConstraintError(pgErr)
			slog.Warn("blob-store: constraint violation", "error", err,
				"sqlstate", pgErr.Code, "constraint", pgErr.ConstraintName, "request_id", reqID)
			writeJSON(w, status, map[string]any{
				"success": false, "error": reason,
				"sqlstate": pgErr.Code, "constraint": pgErr.ConstraintName,
			})
			return
		}
		slog.Error("blob-store: upsert error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	// Attribute the stored blob to the row booked above (fire-and-forget). Only
	// the ATTRIBUTION is async — the metering row itself already exists, so a
	// lost goroutine costs the audit trail one reference, never a budget entry.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if logErr := store.AttributeAccessBlob(bgCtx, h.pool, logID, blob.ID); logErr != nil {
			slog.Error("blob-store: write attribution error", "error", logErr, "request_id", reqID)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"blob":    blob,
	})
}

// meterBlobWrite applies the blob write budget and books the row that pays into
// it, returning that row's id for the later blob attribution. ok=false means
// the response is already written and the caller must stop.
//
// Gate and booking live in ONE function because they are one mechanism: the
// gate counts exactly the rows the booking writes (store.ActionBlobWrite), and
// a caller that ran one without the other would either meter an ungated surface
// or gate an unfed counter — the second is precisely the Ist-Stand this wave
// repairs. The budget is the blob's OWN (pool.blob_rate_limit_write, with
// Config.BlobWriteLimit owning the fallback to query.rate_limit_write; 0 =
// disabled), NOT the block-write budget this path used to borrow. MT 06-C5:
// per-tenant via the request context (tenant's own override, else _global).
//
// The booking is SYNCHRONOUS and precedes the upsert, on purpose. A write that
// dies in a constraint violation has already cost the decode, the checksum and
// an INSERT attempt of up to 50 MB, so it must cost budget too — otherwise the
// surface is free to hammer with payloads that never commit. And the row IS the
// budget: booked after the fact, the gate reads a counter that lags its own
// writes and N concurrent requests all pass an empty one. A booking failure is
// fail-closed (500) for the same reason: an unbookable write is an ungated one.
func (h *BlobHandler) meterBlobWrite(ctx context.Context, w http.ResponseWriter, ar *auth.AuthResult, reqID string) (string, bool) {
	if limit := h.cfg.SnapshotForRequest(ctx).BlobWriteLimit(); limit > 0 {
		writeCount, err := store.CheckRateLimitByAction(ctx, h.pool, ar.ApiKeyID, store.ActionBlobWrite)
		if err != nil {
			slog.Error("blob-store: rate limit check error", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "Internal server error",
			})
			return "", false
		}
		if writeCount >= limit {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"success": false, "error": fmt.Sprintf("Rate limit exceeded: max %d blob writes per 60 seconds", limit),
			})
			return "", false
		}
	}

	logID, err := store.LogAccessRef(ctx, h.pool, ar.ApiKeyID, "", store.ActionBlobWrite)
	if err != nil {
		slog.Error("blob-store: write metering error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return "", false
	}
	return logID, true
}

// blobConstraintError maps a Postgres integrity-constraint violation (SQLSTATE
// class 23) onto an HTTP status and a named reason. Such a violation is a
// property of the REQUEST, not of the server: a uniqueness collision is the
// caller's to resolve (409), every other class-23 violation means the payload
// itself cannot be stored as sent (422). Callers outside class 23 keep the
// opaque 500 — those are server faults and must not leak SQL detail.
func blobConstraintError(pgErr *pgconn.PgError) (int, string) {
	name := pgErr.ConstraintName
	if name == "" {
		name = "unknown"
	}
	switch pgErr.Code {
	case "23505":
		return http.StatusConflict, fmt.Sprintf("Blob violates unique constraint %q", name)
	case "23503":
		return http.StatusUnprocessableEntity, fmt.Sprintf("Blob violates foreign key constraint %q", name)
	case "23502":
		return http.StatusUnprocessableEntity, fmt.Sprintf("Blob is missing a required value for column %q", pgErr.ColumnName)
	case "23514":
		return http.StatusUnprocessableEntity, fmt.Sprintf("Blob violates check constraint %q", name)
	default:
		return http.StatusUnprocessableEntity, fmt.Sprintf("Blob violates constraint %q", name)
	}
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

// blobActionFunc is the uniform shape of a blob-manage action handler. It is a
// METHOD EXPRESSION signature (the receiver is the first parameter) so the
// dispatch table can name the existing methods directly — no closure per row,
// no place for a row to drift away from the method it claims to call.
type blobActionFunc func(*BlobHandler, http.ResponseWriter, *http.Request, *auth.AuthResult, blobManageRequest)

// blobAction binds ONE dispatchable /api/blob/manage action to its admin tier
// and its handler. Tier and routing living in the same row is the point (Gap-
// C0-d): in the manage dispatcher they are two structures that must agree
// (actionTierExplicit's table vs. HandleManage's switch), which is why that one
// needs an enumeration gate to catch a dispatch arm added without a tier entry.
// Here a row without a tier does not compile.
type blobAction struct {
	tier   adminTier
	handle blobActionFunc
}

// blobActions is the SINGLE dispatch source of /api/blob/manage: the switch it
// replaced could route an action the tier table never saw, this table cannot.
//
// All four actions are tierOpen — the pre-table behaviour, unchanged: they are
// auth+scope gated only, and every read path resolves through ar.ReadScopes
// while delete is pinned to ar.HomeScope in the store layer. The tier column
// exists so a future blob action can be gated in the row that declares it,
// NOT to re-cut today's permissions (that would be a separate, arguable wave).
var blobActions = map[string]blobAction{
	"stats":  {tier: tierOpen, handle: (*BlobHandler).handleBlobStats},
	"get":    {tier: tierOpen, handle: (*BlobHandler).handleBlobGet},
	"list":   {tier: tierOpen, handle: (*BlobHandler).handleBlobList},
	"delete": {tier: tierOpen, handle: (*BlobHandler).handleBlobDelete},
}

// enforceBlobActionTier applies the admin tier of a blob-manage action and
// reports whether dispatch may proceed; on a violation it has already written
// the 403. Mirror of enforceActionTier (context_manage.go) with the tier taken
// from the dispatch row instead of a parallel classification: server-global
// actions need a server-admin, per-tenant actions also admit a tenant-admin of
// the caller's OWN tenant (the per-resource target check then belongs IN the
// handler), tierOpen skips the gate. The 403 body is the shared
// requireAdminAction text — no tier oracle for the caller.
func enforceBlobActionTier(w http.ResponseWriter, tier adminTier, ar *auth.AuthResult) bool {
	switch tier {
	case tierServerAdmin:
		return requireAdminAction(w, ar)
	case tierTenantAdmin:
		return requireTenantAdmin(w, ar, ar.TenantID)
	}
	return true // tierOpen
}

// HandleBlobManage processes POST /api/blob/manage.
func (h *BlobHandler) HandleBlobManage(w http.ResponseWriter, r *http.Request) {
	h.handleBlobManage(w, r, blobActions)
}

// handleBlobManage is HandleBlobManage against an INJECTED action table. The
// seam exists for the wiring probe (blob_action_tier_gate_test.go): with every
// production action on tierOpen, a dispatcher that classifies the tier but
// never calls enforceBlobActionTier would be indistinguishable from a correct
// one — the probe dispatches a table carrying gated rows through this very
// function and pins that a member is stopped before the handler runs.
func (h *BlobHandler) handleBlobManage(w http.ResponseWriter, r *http.Request, actions map[string]blobAction) {
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

	// Unknown first: an action that is not in the table has no tier to enforce
	// (the manage dispatcher reaches the same outcome via its fail-open tierOpen
	// default plus the switch default — here the miss IS the default).
	action, ok := actions[req.Action]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "Unknown action (valid: stats, get, list, delete)",
		})
		return
	}
	if !enforceBlobActionTier(w, action.tier, authResult) {
		return
	}
	action.handle(h, w, r, authResult, req)
}

// handleBlobStats takes the unused blobManageRequest to match blobActionFunc —
// a uniform row shape is worth one ignored parameter.
func (h *BlobHandler) handleBlobStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, _ blobManageRequest) {
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
