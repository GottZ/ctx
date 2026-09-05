// Manage family forge-* (Achse 02, Welle I-F, design/02 §4.3/§4.5): the operator
// transport for the forge PULL sync. Under masterplan K14 there is NO separate
// forge-repo registry — the project register (W4, /api/project) IS the
// registration — so this family carries only the SYNC surface:
//
//	forge-token-set    seal a PAT for a project (reveal-never, §5.4)
//	forge-sync-start   on-demand pull run (S7: 409 on double-start, fail-closed gates)
//	forge-sync-status  run-state + last run + conflict count
//
// All three are tierTenantAdmin (actionTier, S9-pinned): they inject credentials
// / trigger outbound sync, so a plain member must not reach them. Ownership is
// re-checked per project (ownsProject) — a tenant-admin of tenant A can not
// touch tenant B's project (uniform 404, no oracle). The PAT plaintext is never
// echoed: token-set returns token_set=true, sync-status returns token_set=bool.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/forge"
	"github.com/GottZ/ctx/internal/store"
)

// ForgeController is the sync-engine surface the forge-* actions consume
// (DreamController/AuditController pattern). *forge.SyncManager implements it.
type ForgeController interface {
	StartSync(ctx context.Context, project store.ProjectRow, dryRun bool) (forge.SyncStatus, error)
	Status(projectID string) forge.SyncStatus
	SetToken(ctx context.Context, project store.ProjectRow, plaintext string) error
}

// SetForgeController wires the sync engine after construction. Kept OUT of
// NewManageHandler to avoid churning its 28 call sites; server.go calls this once,
// tests that exercise forge-* set it directly. nil ⇒ the actions answer 503.
func (h *ManageHandler) SetForgeController(fc ForgeController) { h.forge = fc }

// dispatchForgeAction routes the forge-* family (one arm in HandleManage, cyclop
// budget). Every action loads + ownership-checks the project first (404 uniform).
func (h *ManageHandler) dispatchForgeAction(w http.ResponseWriter, r *http.Request, req manageRequest) {
	if h.forge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "Sync engine not enabled"})
		return
	}
	switch req.Action {
	case "forge-token-set":
		h.handleForgeTokenSet(w, r, req)
	case "forge-sync-start":
		h.handleForgeSyncStart(w, r, req)
	case "forge-sync-status":
		h.handleForgeSyncStatus(w, r, req)
	}
}

// loadOwnedProject resolves the project_id from req.Data and enforces ownership.
// A missing/foreign/absent project is a uniform 404 (no existence oracle, §5.2).
// ok=false means a response was already written.
func (h *ManageHandler) loadOwnedProject(w http.ResponseWriter, r *http.Request, projectID string) (*store.ProjectRow, bool) {
	ar := AuthResultFromContext(r.Context())
	row, err := store.GetProjectByID(r.Context(), h.pool, projectID)
	if err != nil {
		internalError(w, r.Context(), "forge: project load error", err)
		return nil, false
	}
	if row == nil || !ownsProject(ar, row) {
		projectNotFound(w)
		return nil, false
	}
	return row, true
}

type forgeProjectRef struct {
	ProjectID string `json:"project_id"`
	Token     string `json:"token"`
	DryRun    bool   `json:"dry_run"`
}

func decodeForgeData(w http.ResponseWriter, req manageRequest) (forgeProjectRef, bool) {
	var d forgeProjectRef
	if len(req.Data) > 0 && string(req.Data) != "null" {
		if err := json.Unmarshal(req.Data, &d); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid data"})
			return d, false
		}
	}
	if d.ProjectID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "project_id is required"})
		return d, false
	}
	return d, true
}

// handleForgeTokenSet seals a PAT for a project. The token arrives in the data
// field of the tenant-admin-gated action and is sealed immediately; the plaintext
// never survives the request and is never echoed (§5.4).
func (h *ManageHandler) handleForgeTokenSet(w http.ResponseWriter, r *http.Request, req manageRequest) {
	d, ok := decodeForgeData(w, req)
	if !ok {
		return
	}
	if d.Token == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": "token is required (write-only: set it here, read it never)"})
		return
	}
	row, ok := h.loadOwnedProject(w, r, d.ProjectID)
	if !ok {
		return
	}
	if err := h.forge.SetToken(r.Context(), *row, d.Token); err != nil {
		// Never surface the error detail (may embed sealbox/secret context, §5.4).
		slog.Error("forge: token-set failed", "project", row.ID, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "failed to set token"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "project_id": row.ID, "token_set": true})
}

// handleForgeSyncStart launches an on-demand pull run. The fail-closed gates run
// synchronously inside StartSync (so the caller gets the refusal): ErrSyncRunning
// ⇒ 409, ErrNoTenant/ErrIssuePolicy ⇒ 422 (the digest-flood + owner-less-scope
// guards), ErrTenantSuspended ⇒ 409. Success ⇒ 200 with the live run status.
func (h *ManageHandler) handleForgeSyncStart(w http.ResponseWriter, r *http.Request, req manageRequest) {
	d, ok := decodeForgeData(w, req)
	if !ok {
		return
	}
	row, ok := h.loadOwnedProject(w, r, d.ProjectID)
	if !ok {
		return
	}
	st, err := h.forge.StartSync(r.Context(), *row, d.DryRun)
	if err != nil {
		switch {
		case errors.Is(err, forge.ErrSyncRunning):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "sync already running for this project"})
		case errors.Is(err, forge.ErrSyncSaturated):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "sync concurrency limit reached — retry shortly", "retry_after_s": syncSaturatedRetryAfterS})
		case errors.Is(err, forge.ErrTenantSuspended):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "owning tenant is suspended"})
		case errors.Is(err, forge.ErrNoTenant):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": "scope has no owning tenant — sync disabled"})
		case errors.Is(err, forge.ErrIssuePolicy):
			// The clear §6.4 refusal — pass the message (no sensitive content).
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		default:
			slog.Error("forge: sync-start failed", "project", row.ID, "error", err, "request_id", RequestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "failed to start sync"})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "run": st})
}

// handleForgeSyncStatus merges the in-memory run state with the DB history (last
// run + open conflict count). token_set exposes ONLY whether a token exists.
func (h *ManageHandler) handleForgeSyncStatus(w http.ResponseWriter, r *http.Request, req manageRequest) {
	d, ok := decodeForgeData(w, req)
	if !ok {
		return
	}
	row, ok := h.loadOwnedProject(w, r, d.ProjectID)
	if !ok {
		return
	}
	last, err := store.LatestSyncRun(r.Context(), h.pool, row.ID)
	if err != nil {
		internalError(w, r.Context(), "forge: latest run error", err)
		return
	}
	conflicts, err := store.ConflictCount(r.Context(), h.pool, row.ID)
	if err != nil {
		internalError(w, r.Context(), "forge: conflict count error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"project_id":    row.ID,
		"sync_enabled":  row.SyncEnabled,
		"push_enabled":  row.PushEnabled,
		"token_set":     row.TokenSecret != nil,
		"sync_status":   row.SyncStatus,
		"last_sync_at":  row.LastSyncAt,
		"backoff_until": row.BackoffUntil,
		"last_error":    row.LastError,
		"conflicts":     conflicts,
		"run":           h.forge.Status(row.ID),
		"last_run":      last,
	})
}
