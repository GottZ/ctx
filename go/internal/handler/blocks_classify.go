// blocks-classify-* manage actions (G40): trigger + status of the credentials
// PATTERN re-audit. Admin-gated in full — even though the detector is
// upgrade-only, it is a corpus-wide mutation and the status/samples disclose
// block titles and the classification topology (same opsec surface as the G41
// audit). Scope-bound to the caller's home_scope by the store layer (2.3d).
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package handler

import (
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
)

// handleBlocksClassifyStart launches a run. data: {"dry_run":bool,"limit":int}
// — dry_run scans WITHOUT writing (the gate: see what WOULD be raised on the
// real corpus first), limit 0 on a live run scans the whole candidate set.
func (h *ManageHandler) handleBlocksClassifyStart(w http.ResponseWriter, r *http.Request, req manageRequest) {
	h.handleBlocksRunStart(w, r, req, blocksRunSpec{
		action:  "blocks-classify-start",
		running: events.ErrClassifyRunning,
		busyMsg: "Credentials classify already running",
		failMsg: "Failed to start classify",
		logNoun: "credentials classify",
		start:   AuditController.StartCredentialsClassify,
		render:  h.writeBlocksClassifyStatus,
	})
}

// handleBlocksClassifyStatus reports run state + live per-source DB breakdown.
func (h *ManageHandler) handleBlocksClassifyStatus(w http.ResponseWriter, r *http.Request) {
	if h.auditController == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error": "Scheduler not enabled",
		})
		return
	}
	h.writeBlocksClassifyStatus(w, r)
}

func (h *ManageHandler) writeBlocksClassifyStatus(w http.ResponseWriter, r *http.Request) {
	scope := h.cfg.Snapshot().Scheduler.HomeScope //nolint:forbidigo // MT 06 gated: classify status stays Snapshot() until the per-tenant-admin cut (§5.7); deliberately tenant-less for now.
	if scope == "" {
		scope = "private"
	}
	_, bySource, err := store.AuditProgress(r.Context(), h.pool, scope)
	if err != nil {
		slog.Error("manage: classify progress query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Failed to read classify progress",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"scope":     scope,
		"by_source": bySource,
		"run":       h.auditController.CredentialsClassifyStatus(),
	})
}
