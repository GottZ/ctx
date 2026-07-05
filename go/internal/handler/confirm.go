// F6-C6 D-W6b: POST /api/confirm — the HTTP confirm surface the SPA
// ConfirmCard calls. Executes ONE previously staged write, selected by payload
// hash, strictly per-key.
//
// Flow mirrors mcpConfirmHandler (mcp_confirm.go, D-W5) as a deliberate small
// duplication: that file is the MCP surface (parallel wave W6a builds the
// update tool there), so this handler copies the lookup → re-validate →
// consume → execute sequence instead of refactoring shared helpers out of it.
// Consolidation note for a later wave: extract the sequence once both
// surfaces are settled.
//
// AUTH IS HEADER-ONLY (D1-m4): the route mounts behind handler.Auth, which
// reads X-Context-Key / Authorization: Bearer — the server has NO cookie auth
// path at all, so a cross-site POST carries no usable credential. This
// handler additionally fail-closes on a missing AuthResult; it must never be
// mounted outside the Auth group.
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
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

// HandleConfirm executes a staged write. The four miss cases (unknown hash,
// expired, already consumed, foreign key) answer ONE generic 404
// (confirmNotFoundMsg, mcp_confirm.go) — the confirm surface must not be an
// oracle over other principals' staged writes. The scope re-validation runs
// on the un-consumed row (a shrunk right rejects WITHOUT burning the stage
// token, D1-M1); the consume is the atomic replay guard.
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

	pw, err := store.LookupPendingWrite(r.Context(), h.pool, ar.ApiKeyID, req.PayloadHash)
	if errors.Is(err, store.ErrPendingWriteNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": confirmNotFoundMsg})
		return
	}
	if err != nil {
		slog.Error("confirm: lookup failed", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	var cw store.CanonicalWrite
	if err := json.Unmarshal(pw.Payload, &cw); err != nil {
		slog.Error("confirm: staged payload unmarshal failed", "error", err, "pending_id", pw.ID, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "staged payload unreadable"})
		return
	}
	if cw.Op != "store" || cw.Scope != pw.Scope {
		// op 'update' ships with the update wave; a row/payload scope mismatch
		// would mean tampering — both land in the generic miss.
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": confirmNotFoundMsg})
		return
	}

	// D1-M1: re-validate against the CURRENT key rights, on the un-consumed row.
	if !contains(writableBlockScopes(ar), pw.Scope) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false,
			"error":   fmt.Sprintf("confirm rejected: scope %q is no longer writable for this key — the staged write stays pending until it expires", pw.Scope),
		})
		return
	}

	if _, err := store.ConsumePendingWrite(r.Context(), h.pool, ar.ApiKeyID, req.PayloadHash); err != nil {
		if errors.Is(err, store.ErrPendingWriteNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": confirmNotFoundMsg})
			return
		}
		slog.Error("confirm: consume failed", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	// Execute over the SAME sequence as the direct store paths (upsert +
	// classify + temporal — mcp_confirm.go / mcp.go).
	sens := store.SensitivityWrite{
		Value:    backends.Sensitivity(cw.Sensitivity),
		Manual:   cw.SensitivityManual,
		Detector: cw.SensitivityDetect,
	}
	block, err := store.UpsertBlock(r.Context(), h.pool, cw.Category, cw.Title, cw.Content, cw.Tags, cw.Metadata, cw.Scope, false, sens, cw.Type)
	if err != nil {
		// Token consumed, write failed — fail-closed and rare (rejected
		// finding D1-m2): the client re-stages.
		slog.Error("confirm: confirmed write execute failed", "error", err, "pending_id", pw.ID, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"error":   "confirmed write failed to execute — the stage token is consumed; re-stage the write",
		})
		return
	}

	var classifySet *blocktype.Set
	if h.blocktypes != nil {
		classifySet = h.blocktypes.SnapshotForRequest(r.Context())
	}
	if _, err := store.ClassifyBlockAfterUpsert(r.Context(), h.pool, classifySet, block.ID, block.Title, block.Metadata); err != nil {
		slog.Warn("confirm: auto-classify failed", "error", err, "block_id", block.ID)
	}
	times := store.ExtractDates(block.Content)
	_ = store.UpdateContentTimes(r.Context(), h.pool, block.ID, times)
	_ = store.PopulateTemporal(r.Context(), h.pool, block.ID, times, block.CreatedAt)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"block": map[string]any{
			"id": block.ID, "title": block.Title, "category": block.Category, "scope": cw.Scope,
		},
	})
}
