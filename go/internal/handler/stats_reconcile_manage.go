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

// statsReconcileMaxCorrections bounds the corrections one stats-reconcile
// answer carries (issue #271). A correction is one drifted counter key, and
// after a restore into an existing database every key of the store drifts:
// at the 1M–10M-block target hundreds of thousands, an answer of tens of
// megabytes that the CLI's 10 MB read cap would cut into invalid JSON. 1 000
// corrections are a few hundred kilobytes at most. The pass applies and logs
// every correction whatever the bound; corrections_total is their number.
const statsReconcileMaxCorrections = 1000

// handleStatsReconcile (issue #266) compares the block counters that
// stats, guard-stats and list-categories read (context_block_counts) with a
// real count, scope by scope, and corrects every difference; the answer names
// at most statsReconcileMaxCorrections of them (issue #271). Server-admin
// (tierServerAdmin): the pass reads and repairs every tenant's scopes, and the
// answer names them. Each scope is its own transaction, so a request that ends
// early leaves the scopes it finished corrected; a scope that fails is named in
// "failed" (status 500, success false) and the pass goes on with the others. The scheduler runs the same
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
	if err != nil && res == nil {
		slog.Error("manage: stats-reconcile error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	// A pass in which some scopes failed still reports what it corrected and
	// names the failed scopes; the database errors stay in the log. The answer
	// carries the first statsReconcileMaxCorrections corrections of the pass
	// and their total.
	corrections := res.Corrections
	if len(corrections) > statsReconcileMaxCorrections {
		corrections = corrections[:statsReconcileMaxCorrections]
	}
	body := map[string]any{
		"action":            "stats-reconcile",
		"success":           err == nil,
		"scopes":            res.Scopes,
		"corrections":       corrections,
		"corrections_total": len(res.Corrections),
		"pruned":            res.Pruned,
		"failed":            res.Failed,
	}
	status := http.StatusOK
	if err != nil {
		slog.Error("manage: stats-reconcile incomplete", "error", err, "failed", len(res.Failed), "request_id", reqID)
		body["error"] = "reconcile failed for some scopes"
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, body)
}
