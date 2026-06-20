// blocks-audit-* manage actions (G41): trigger + status of the sensitivity
// LLM audit. Admin-gated in full — the trigger causes bulk downgrades (the
// opsec direction, design 03 §3.5) and the status discloses block titles plus
// classification topology.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
)

// AuditController is the scheduler surface the blocks-* maintenance actions
// consume (DreamController pattern): the G41 sensitivity LLM audit and the G40
// credentials pattern classify. Status carries only in-memory run state; the
// pending/per-source counts come from the DB here.
type AuditController interface {
	StartSensitivityAudit(dryRun bool, limit int) error
	SensitivityAuditStatus() events.AuditStatus
	StartCredentialsClassify(dryRun bool, limit int) error
	CredentialsClassifyStatus() events.ClassifyStatus
}

// handleBlocksAuditStart launches a run. data: {"dry_run":bool,"limit":int} —
// dry_run is the N=30 sample gate (random order, no writes), limit 0 on a
// live run drains the full pick set.
func (h *ManageHandler) handleBlocksAuditStart(w http.ResponseWriter, r *http.Request, req manageRequest) {
	if h.auditController == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error": "Scheduler not enabled",
		})
		return
	}

	var params struct {
		DryRun bool `json:"dry_run"`
		Limit  int  `json:"limit"`
	}
	if len(req.Data) > 0 && string(req.Data) != "null" {
		if err := json.Unmarshal(req.Data, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid data: expected {\"dry_run\":bool,\"limit\":int}",
			})
			return
		}
	}
	if params.Limit < 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false, "error": "limit must be >= 0",
		})
		return
	}

	if err := h.auditController.StartSensitivityAudit(params.DryRun, params.Limit); err != nil {
		if errors.Is(err, events.ErrAuditRunning) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"success": false, "error": "Sensitivity audit already running",
			})
			return
		}
		slog.Error("manage: blocks-audit-start failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Failed to start audit",
		})
		return
	}

	slog.Info("manage: sensitivity audit started", "dry_run", params.DryRun, "limit", params.Limit)
	h.writeBlocksAuditStatus(w, r)
}

// handleBlocksAuditStatus reports run state + live DB progress.
func (h *ManageHandler) handleBlocksAuditStatus(w http.ResponseWriter, r *http.Request) {
	if h.auditController == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error": "Scheduler not enabled",
		})
		return
	}
	h.writeBlocksAuditStatus(w, r)
}

func (h *ManageHandler) writeBlocksAuditStatus(w http.ResponseWriter, r *http.Request) {
	scope := h.cfg.Snapshot().Scheduler.HomeScope //nolint:forbidigo // MT 06 gated: audit status stays Snapshot() until the per-tenant-admin cut (§5.7); deliberately tenant-less for now.
	if scope == "" {
		scope = "private"
	}
	pending, bySource, err := store.AuditProgress(r.Context(), h.pool, scope)
	if err != nil {
		slog.Error("manage: audit progress query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Failed to read audit progress",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"scope":     scope,
		"pending":   pending,
		"by_source": bySource,
		"run":       h.auditController.SensitivityAuditStatus(),
	})
}
