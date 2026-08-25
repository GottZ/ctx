package handler

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"

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
	// ContextBlockID is the optional blob-to-block edge (W02-10). Absent or
	// empty writes NULL, which is what every request before this wave meant
	// and still means. The referenced block must be visible in the key's read
	// scopes; if it is not, the write is refused with the constraint verdict
	// (blobBlockRefGate) rather than silently building a cross-scope edge.
	ContextBlockID string `json:"context_block_id"`
}

// HandleBlobStore processes POST /api/blob/store.
func (h *BlobHandler) HandleBlobStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSONReject(w, classUnauthorized.reject("unauthorized"))
		return
	}

	var req blobStoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("blob-store: invalid request body", "error", err, "request_id", reqID)
		writeJSONReject(w, classInvalidBody.reject("Invalid request body"))
		return
	}

	// The three steps of the shared core, in the order this handler has always
	// run them (blob_core.go): required fields + name caps → base64 decode +
	// payload caps → scope gate, budget, upsert, attribution. The MCP
	// blob_store tool calls the identical three, which is what makes the two
	// surfaces one write path (W02-8, BP-6) instead of two implementations of
	// the same intent — the split this surface already paid for once, when its
	// own narrower scope formula refused blob writes a key could make as block
	// writes (Gap-C0-c).
	if rej := blobFieldGates(req.File != "", req.Filename, req.Category, req.Title, req.MimeType); rej != nil {
		writeJSONReject(w, rej)
		return
	}
	data, rej := decodeBlobFile(req.File)
	if rej != nil {
		writeJSONReject(w, rej)
		return
	}

	blob, blobRej := executeBlobWrite(ctx, h.pool, h.cfg, authResult, blobWriteInput{
		Category: req.Category,
		Title:    req.Title,
		Filename: req.Filename,
		MimeType: req.MimeType,
		Scope:    req.Scope,
		Data:     data,
		Tags:     req.Tags,
		Metadata: req.Metadata,

		ContextBlockID: req.ContextBlockID,
	}, reqID)
	if blobRej != nil {
		writeBlobReject(w, blobRej)
		return
	}

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
		writeJSONReject(w, classUnknownAction.reject("Unknown action (valid: stats, get, list, delete)"))
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
