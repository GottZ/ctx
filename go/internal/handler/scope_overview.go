package handler

import (
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/store"
)

// handleScopeOverview answers the server-admin scope-overview read (MT 04-W6/A0,
// design/04 §3): one row per scope with block + key counts and the owning tenant,
// for the scope landscape, the tenant-delete blast-radius count, and the
// QuotaForm's tenant→scope mapping. Server-admin only (tierServerAdmin at the
// dispatch gate); the store query is deliberately UNSCOPED (counts only, no block
// content → no cross-tenant content leak, design/04 §3 A0). It takes no AuthResult
// — there is no per-tenant filter to apply (the whole point is the global
// landscape), mirroring handleMCPClientList.
func (h *ManageHandler) handleScopeOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	scopes, err := store.ScopeOverviews(ctx, h.pool)
	if err != nil {
		slog.Error("manage: scope-overview error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "scope-overview",
		"success": true,
		"scopes":  scopes,
	})
}
