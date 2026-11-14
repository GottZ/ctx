package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/store"
)

// statsReconcileSpec is the optional payload of stats-reconcile: one scope,
// or every scope when absent.
type statsReconcileSpec struct {
	Scope string `json:"scope"`
}

// handleStatsReconcile (issue #266) compares the block counters that
// stats, guard-stats and list-categories read (context_block_counts) with a
// real count, scope by scope, and corrects every difference. Server-admin
// (tierServerAdmin): the pass reads and repairs every tenant's scopes, and the
// answer names them. Each scope is its own transaction, so a request that ends
// early leaves the scopes it finished corrected. The scheduler runs the same
// pass on scheduler.block_count_reconcile_interval.
func (h *ManageHandler) handleStatsReconcile(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	var spec statsReconcileSpec
	if len(req.Data) > 0 && string(req.Data) != "null" {
		dec := json.NewDecoder(bytes.NewReader(req.Data))
		dec.DisallowUnknownFields()
		if dec.Decode(&spec) != nil || dec.More() {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": `data must be {} or {"scope": "<scope>"}`,
			})
			return
		}
	}
	res, err := store.ReconcileBlockCounts(ctx, h.pool, spec.Scope)
	if err != nil {
		slog.Error("manage: stats-reconcile error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":      "stats-reconcile",
		"success":     true,
		"scopes":      res.Scopes,
		"corrections": res.Corrections,
		"pruned":      res.Pruned,
	})
}
