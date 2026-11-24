package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// backfillReleaseSpec is the data payload of `manage embed-backfill-release`
// (issue #236). Unknown fields are refused, as for purge: a misspelt
// selector must not turn into a different release.
type backfillReleaseSpec struct {
	BlockID string `json:"block_id"`
	Class   string `json:"class"`
	// Limit is the page size, 1 to store.BackfillReleaseMaxLimit; absent or
	// 0 is the maximum, a larger value is clamped to it.
	Limit int `json:"limit"`
	// ConfirmMemos absent = dry run; otherwise the counted (or remaining)
	// number of the call before, re-checked against a fresh count.
	ConfirmMemos *int64 `json:"confirm_memos"`
}

const backfillReleaseHelp = "data payload required: exactly one of block_id (a uuid) or class (wire, oversize, caller_timeout); optional limit (the page size, at most 1000), confirm_memos — unknown fields are refused"

// handleEmbedBackfillRelease is `manage embed-backfill-release`: it drops the
// regular backfill memos (context_embed_failures, migration_id IS NULL) of
// one block or of one failure class, so the backfill takes those blocks
// again (store.ReleaseBackfillMemos) — after embed_backfill.max_tokens was
// raised, the embed backend moved to a larger context, or a backend outage
// ended. Tier tierTenantAdmin; the range is the store's reader rule: a
// server-admin reaches every memo, a tenant-admin the memos of blocks in its
// own tenant's scopes, so a block of another tenant counts like one that
// does not exist. Without confirm_memos it is a dry run; a confirmed call
// deletes at most limit memos and answers what is left, with the number to
// confirm for the next page.
func (h *ManageHandler) handleEmbedBackfillRelease(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	var spec backfillReleaseSpec
	dec := json.NewDecoder(bytes.NewReader(req.Data))
	dec.DisallowUnknownFields()
	if len(req.Data) == 0 || dec.Decode(&spec) != nil || dec.More() ||
		spec.Limit < 0 || (spec.ConfirmMemos != nil && *spec.ConfirmMemos < 0) ||
		(spec.BlockID != "" && !uuidRe.MatchString(spec.BlockID)) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": backfillReleaseHelp})
		return
	}
	res, err := store.ReleaseBackfillMemos(ctx, h.pool, store.BackfillRelease{
		BlockID:    spec.BlockID,
		Class:      store.EmbedFailureClass(spec.Class),
		AllTenants: ar.IsServerAdmin(),
		TenantID:   ar.TenantID,
		Limit:      min(spec.Limit, store.BackfillReleaseMaxLimit),
		Confirm:    spec.ConfirmMemos,
	})
	switch {
	case errors.Is(err, store.ErrBackfillReleaseSelector):
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": backfillReleaseHelp})
		return
	case errors.Is(err, store.ErrBackfillReleaseConfirmMismatch):
		writeJSON(w, http.StatusConflict, map[string]any{
			"action": req.Action, "success": false,
			"error":   "the memos in range are not the confirmed number — check the count and confirm again",
			"counted": res.Counted, "confirm_memos": res.Counted,
		})
		return
	case err != nil:
		adminWriteFailed(r, "manage: "+req.Action+" failed", err, statementOutcome(err))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "embed-backfill-release failed"})
		return
	}
	body := map[string]any{
		"action": req.Action, "success": true, "dry_run": res.DryRun,
		"counted": res.Counted, "confirm_memos": res.Remaining,
	}
	if !res.DryRun {
		body["released"] = res.Released
		body["remaining"] = res.Remaining
		slog.Info("manage: backfill memos released", "class", spec.Class, "block_id", spec.BlockID,
			"released", res.Released, "remaining", res.Remaining,
			"api_key_id", ar.ApiKeyID, "request_id", RequestIDFromContext(ctx))
	}
	writeJSON(w, http.StatusOK, body)
}
