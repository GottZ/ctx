// Manage family dream-* dispatch + dream-link-resolve (review-governance
// wave 2026-07-26). dream-link-resolve is the WRITE side of the dream-review
// read surface: confirm pins a link (survives the dream replace sweep, M119)
// and optionally records a durable rationale; delete removes it and reverts a
// supersedes snapshot-marking. Built after the guard-resolve pattern
// (context_manage.go handleGuardResolve → store.GuardResolveBatch): the write
// gate is writableBlockScopes in the store layer (source-block scope, uniform
// not found — no existence oracle), never an admin tier. The actionTier entry
// (tierOpen, EXPLICIT — S9 fail-open default) lives in actionTierExplicit.
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
)

// dispatchDreamAction fans the dream-* actions out (split from HandleManage's
// switch for the cyclomatic budget when dream-link-resolve landed; the
// established dispatchGuardAction idiom). Tier gating happened upstream in
// enforceActionTier.
func (h *ManageHandler) dispatchDreamAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "dream-stats":
		h.handleDreamStats(w, r, ar)
	case "dream-review":
		h.handleDreamReview(w, r, ar)
	case "dream-mode":
		h.handleDreamMode(w, r, req)
	case "dream-link-resolve":
		h.handleDreamLinkResolve(w, r, ar, req)
	case "dream-backoff-restamp":
		h.handleDreamBackoffRestamp(w, r, ar)
	}
}

// handleDreamBackoffRestamp re-evaluates every existing cooldown stamp under
// the CURRENT back-off policy (dream.RestampBackoff): the settings UI calls
// this right after a dream.backoff_* save, so the new curve governs the
// whole corpus immediately instead of only future cycles (stamps written
// under the old policy would otherwise persist for up to the old cap).
//
// Scope binding: HomeScope + AllowedScopes — the key's OWN entitlement,
// deliberately NOT ar.ReadScopes: read scopes may carry cross-tenant grants
// (T17), and a granted reader must not mutate a foreign tenant's dream
// scheduling. The policy snapshot mirrors handleDreamStats so a stats fetch
// right after the restamp renders exactly the applied generation.
func (h *ManageHandler) handleDreamBackoffRestamp(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	scopes := append([]string{ar.HomeScope}, ar.AllowedScopes...)
	linkable := h.dreamLinkableTypes(ctx)
	restamped, skippedTransient, err := dream.RestampBackoff(ctx, h.pool, scopes, linkable, h.cfg.Snapshot().DreamBackoff()) //nolint:forbidigo // MT 06 BLIND: dream back-off is a server-global scheduler policy (the dream loop is process-wide), not tenant-scoped — same marker as handleDreamStats.
	if err != nil {
		slog.Error("manage: dream-backoff-restamp failed", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream backoff restamp failed",
		})
		return
	}
	slog.Info("manage: dream back-off pipeline re-evaluated",
		"restamped", restamped, "skipped_transient", skippedTransient, "request_id", reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success":           true,
		"action":            "dream-backoff-restamp",
		"restamped":         restamped,
		"skipped_transient": skippedTransient,
	})
}

// handleDreamLinkResolve resolves ONE dream link (confirm|delete). Wire shape
// mirrors guard-resolve: 400 on malformed input, 200 {success:false, "Link
// not found"} for foreign/invisible/absent links (uniform, no oracle), 200
// with the resolution record on success.
func (h *ManageHandler) handleDreamLinkResolve(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	var data struct {
		SourceID     string `json:"source_id"`
		TargetID     string `json:"target_id"`
		Relationship string `json:"relationship"`
		Resolution   string `json:"resolution"`
		Rationale    string `json:"rationale"`
	}
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &data); err != nil {
			slog.Warn("manage: invalid dream-link-resolve data", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid data format",
			})
			return
		}
	}

	if data.SourceID == "" || data.TargetID == "" || data.Relationship == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required fields: source_id, target_id, relationship",
		})
		return
	}
	if data.Resolution != "confirm" && data.Resolution != "delete" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Resolution must be 'confirm' or 'delete'",
		})
		return
	}

	resolved, err := store.DreamLinkResolve(ctx, h.pool,
		data.SourceID, data.TargetID, data.Relationship, data.Resolution, data.Rationale,
		writableBlockScopes(ar))
	if err != nil {
		slog.Error("manage: dream-link-resolve error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Link not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":     "dream-link-resolve",
		"success":    true,
		"resolved":   resolved,
		"resolution": resolved.Resolution,
	})
}
