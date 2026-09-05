// The shared start path of the two blocks-* background runs (design 03 §4.5):
// the G41 sensitivity audit and the G40 credentials classify answered the same
// five branches through two copies of one body. The copies differed in nothing
// but the controller entry point, the "already running" sentinel and three
// prose strings — so those became data (blocksRunSpec) and the body became one
// handler. Every string is carried verbatim: no answer changes.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// blocksRunSpec is the per-family half of a blocks-* start action: the wire
// action name (it also stems the slog.Error message), the sentinel that means
// "this family is already running", the two error prose strings, the noun of
// the start log line, the controller entry point and the status render.
//
// start takes the controller as a PARAMETER instead of closing over it: a
// method value on a nil interface panics when it is BUILT, not when it is
// called, so binding h.auditController.Start… inside the caller's spec literal
// would panic before handleBlocksRunStart could answer 503. A method
// expression (AuditController.StartSensitivityAudit) has no receiver until the
// call, which is after the nil check.
type blocksRunSpec struct {
	action  string
	running error
	busyMsg string
	failMsg string
	logNoun string
	start   func(c AuditController, dryRun bool, limit int) error
	render  func(w http.ResponseWriter, r *http.Request)
}

// handleBlocksRunStart runs one blocks-* start action: 503 without a
// scheduler, 400 on an unreadable payload, 422 on a negative limit, 409 when
// the family's run is already going, 500 on anything else — and on success the
// family's own status render IS the answer (start and status share one
// envelope per family, web/src/lib/api/corpus.ts:6).
func (h *ManageHandler) handleBlocksRunStart(w http.ResponseWriter, r *http.Request, req manageRequest, spec blocksRunSpec) {
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

	if err := spec.start(h.auditController, params.DryRun, params.Limit); err != nil {
		if errors.Is(err, spec.running) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"success": false, "error": spec.busyMsg,
			})
			return
		}
		slog.Error("manage: "+spec.action+" failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": spec.failMsg,
		})
		return
	}

	slog.Info("manage: "+spec.logNoun+" started", "dry_run", params.DryRun, "limit", params.Limit)
	spec.render(w, r)
}
