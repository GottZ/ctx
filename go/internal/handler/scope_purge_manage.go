package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// purgeSpec is the data payload of `manage purge` (issue #202). Unknown fields
// are refused: on a delete that drains the scope when no filter is set, a
// misspelt filter key must not silently widen the purge to everything.
type purgeSpec struct {
	Scope         string     `json:"scope"`
	Tags          []string   `json:"tags"`
	Category      string     `json:"category"`
	CreatedBefore *time.Time `json:"created_before"`
	CreatedAfter  *time.Time `json:"created_after"`
	ArchivedOnly  bool       `json:"archived_only"`
	// ConfirmBlocks absent = dry run; otherwise the `counted.blocks` of the
	// dry run, re-checked against a fresh count before anything is deleted.
	ConfirmBlocks *int64 `json:"confirm_blocks"`
}

const purgeSpecHelp = "data payload required: scope; optional tags, category, created_before, created_after (RFC 3339), archived_only, confirm_blocks — unknown fields are refused"

// handlePurge is `manage purge`: the hard delete of block data inside one
// scope (store.PurgeScope). Tier tierTenantAdmin; the per-scope check is the
// store's reader rule — a tenant-admin reaches only the scopes of its own
// tenant (a foreign and an unknown scope answer the same 404), a server-admin
// every non-system scope. Without confirm_blocks it is a dry run.
func (h *ManageHandler) handlePurge(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	if ar == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	var spec purgeSpec
	dec := json.NewDecoder(bytes.NewReader(req.Data))
	dec.DisallowUnknownFields()
	if len(req.Data) == 0 || dec.Decode(&spec) != nil || dec.More() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": purgeSpecHelp})
		return
	}
	if spec.ConfirmBlocks != nil && *spec.ConfirmBlocks < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "confirm_blocks must not be negative"})
		return
	}
	filter := store.PurgeFilter{
		Tags: spec.Tags, Category: spec.Category,
		CreatedBefore: spec.CreatedBefore, CreatedAfter: spec.CreatedAfter,
		ArchivedOnly: spec.ArchivedOnly,
	}
	// The confirmed run is detached from the request: a client that gives up
	// (the CLI's 120 s timeout, a dropped connection) would otherwise roll back
	// the batch in flight and leave the purge to restart from a new dry run.
	// Every batch is its own transaction, so a run that ends at shutdown
	// leaves whole batches. The dry run stays bound to the request.
	runCtx := ctx
	if spec.ConfirmBlocks != nil {
		runCtx = context.WithoutCancel(ctx)
	}
	res, err := store.PurgeScope(runCtx, h.pool, store.PurgeQuery{
		Scope:         spec.Scope,
		AllTenants:    ar.IsServerAdmin(),
		TenantID:      ar.TenantID,
		Filter:        filter,
		ConfirmBlocks: spec.ConfirmBlocks,
		Stamp:         auditStamp(r, ar),
	})
	switch {
	case errors.Is(err, store.ErrPurgeScopeInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "scope must be a non-empty scope outside the '_' system namespace"})
		return
	case errors.Is(err, store.ErrPurgeScopeNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "scope not found"})
		return
	case errors.Is(err, store.ErrPurgeConfirmMismatch):
		writePurgeRefusal(w, res, filter, "the blocks to purge are not the confirmed ones (count changed, or rows came to match or were added below a matched block during the run) — see deleted for what earlier batches removed; check the counts and confirm again")
		return
	case errors.Is(err, store.ErrPurgeChildOutsideScope):
		writePurgeRefusal(w, res, filter, "a block that would go has a child block in another scope (parent_id cascades) — nothing more was deleted")
		return
	case errors.Is(err, store.ErrPurgeDerivedChild):
		writePurgeRefusal(w, res, filter, "a block that would go has a derivative below it (parent_id cascades, a derivative is never purged) — nothing more was deleted")
		return
	case err != nil:
		body := map[string]any{"success": false, "error": "purge failed"}
		var done *store.PurgeDeleted
		if res != nil && res.Deleted != nil {
			done = res.Deleted
			body["deleted"] = done
		}
		slog.Error("manage: purge failed", "scope", spec.Scope, "error", err, "deleted", done,
			"api_key_id", ar.ApiKeyID, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	body := map[string]any{
		"action": "purge", "success": true, "scope": spec.Scope, "filter": filter,
		"dry_run": res.DryRun, "as_of": res.AsOf, "counted": res.Counted,
	}
	if res.DryRun {
		body["confirm_blocks"] = res.Counted.Blocks
	} else {
		body["deleted"] = res.Deleted
		slog.Info("manage: scope purged", "scope", spec.Scope, "blocks", res.Deleted.Blocks,
			"dream_links", res.Deleted.DreamLinks, "blobs", res.Deleted.Blobs,
			"api_key_id", ar.ApiKeyID, "request_id", reqID)
	}
	writeJSON(w, http.StatusOK, body)
}

// writePurgeRefusal answers a refused run: 409 with the fresh count, and what
// earlier batches already removed when the refusal came mid-run.
func writePurgeRefusal(w http.ResponseWriter, res *store.PurgeResult, filter store.PurgeFilter, msg string) {
	body := map[string]any{"action": "purge", "success": false, "error": msg, "filter": filter}
	if res != nil {
		body["as_of"] = res.AsOf
		body["counted"] = res.Counted
		body["confirm_blocks"] = res.Counted.Blocks
		if res.Deleted != nil {
			body["deleted"] = res.Deleted
		}
	}
	writeJSON(w, http.StatusConflict, body)
}
