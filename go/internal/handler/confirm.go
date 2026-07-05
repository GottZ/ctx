// F6-C6 D-W6b: POST /api/confirm — the HTTP confirm surface the SPA
// ConfirmCard calls. Executes ONE previously staged write (op 'store' or,
// since D-W6c, op 'update'), selected by payload hash, strictly per-key.
//
// The sequence itself (lookup → validate → D1-M1/D1-M3 re-checks → atomic
// consume → execute) lives in confirm_core.go, shared with the MCP confirm
// tool (the D-W6b duplication note is redeemed, D-W6c). This handler only
// maps the typed outcome onto HTTP statuses and laundered bodies.
//
// AUTH IS HEADER-ONLY (D1-m4): the route mounts behind handler.Auth, which
// reads X-Context-Key / Authorization: Bearer — the server has NO cookie auth
// path at all, so a cross-site POST carries no usable credential. This
// handler additionally fail-closes on a missing AuthResult; it must never be
// mounted outside the Auth group.
package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
)

// ConfirmHandler serves POST /api/confirm.
type ConfirmHandler struct {
	pool       *pgxpool.Pool
	blocktypes *blocktype.Registry
}

// NewConfirmHandler wires the confirm surface.
func NewConfirmHandler(pool *pgxpool.Pool, blocktypes *blocktype.Registry) *ConfirmHandler {
	return &ConfirmHandler{pool: pool, blocktypes: blocktypes}
}

type confirmRequest struct {
	PayloadHash string `json:"payload_hash"`
}

// HandleConfirm executes a staged write (op 'store' AND — since D-W6c — op
// 'update'). The sequence lives in confirm_core.go, shared byte-for-byte with
// the MCP confirm tool; this handler only maps the typed outcome onto HTTP.
// The four miss cases answer ONE generic 404 (no oracle, D1-M4); infra and
// execute errors are LAUNDERED to generic bodies (unlike MCP, this surface
// reaches a browser). Scope- (D1-M1) and TOCTOU-rejects (D1-M3) happen on the
// un-consumed row — the stage token survives them.
func (h *ConfirmHandler) HandleConfirm(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	var req confirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PayloadHash == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "payload_hash is required"})
		return
	}
	reqID := RequestIDFromContext(r.Context())

	out := executeConfirm(r.Context(), h.pool, h.blocktypes, ar, req.PayloadHash)
	switch out.Kind {
	case confirmOK:
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"op":      out.Op,
			"block": map[string]any{
				"id": out.Block.ID, "title": out.Block.Title, "category": out.Block.Category, "scope": out.Block.Scope,
			},
		})
	case confirmMiss:
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": confirmNotFoundMsg})
	case confirmScopeRejected:
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": confirmScopeRejectMsg(out.Scope)})
	case confirmTOCTOUGone:
		// 409: the stage is intact but the world moved — re-read and re-stage.
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": confirmTOCTOUGoneMsg})
	case confirmTOCTOUDrift:
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": confirmTOCTOUDriftMsg(out.BlockID)})
	case confirmUnreadable:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "staged payload unreadable"})
	case confirmExecErr:
		slog.Error("confirm: confirmed write execute failed", "error", out.Err, "op", out.Op, "request_id", reqID)
		verb, noun := "write", "write"
		if out.Op == "update" {
			verb, noun = "update", "update"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"error":   fmt.Sprintf("confirmed %s failed to execute — the stage token is consumed; re-stage the %s", verb, noun),
		})
	case confirmExecGone:
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"error":   "confirmed update failed to execute — block no longer accessible; the stage token is consumed; re-stage the update",
		})
	default: // confirmInfraErr
		slog.Error("confirm: infrastructure error", "error", out.Err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
	}
}
