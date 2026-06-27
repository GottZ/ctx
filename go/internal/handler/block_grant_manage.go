package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// Block-level read-grant manage-actions (MT T43, Achse 07-W6; design/07 §4.7).
// The THIRD level of the 3-level architecture: a single block (not a whole scope)
// is shared with a grantee tenant, additive + revocable + immediate. The read-side
// OR-arm (T40a/T40b) is already live; this is the WRITE side — create/list/revoke
// behind the server-admin tier PLUS a hard per-resource ownership gate.
//
// requireAdminAction (the upstream tier gate, context_manage.go) checks ONLY the
// server-global is_admin — it has NO tenant filter (TODO :215). So the ownership
// gate below is the ONLY thing standing between an admin and cross-tenant
// exfiltration via a forged block-grant (design/07 §5.1, KRITISCH). It is NOT
// optional.

type blockGrantSpec struct {
	BlockID       string `json:"block_id"`
	GranteeTenant string `json:"grantee_tenant"`
}

// dispatchBlockGrantAction fans the block-grant-* actions out (split from
// HandleManage's switch for the cyclomatic budget, mirroring dispatchTenantAction).
// All three are server-admin-gated upstream; create/revoke add the ownership gate.
func (h *ManageHandler) dispatchBlockGrantAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "block-grant-create":
		h.handleBlockGrantCreate(w, r, ar, req)
	case "block-grant-list":
		h.handleBlockGrantList(w, r, ar, req)
	case "block-grant-revoke":
		h.handleBlockGrantRevoke(w, r, ar, req)
	}
}

// blockOwnershipGate enforces design/07 §5.1: the caller-tenant must OWN the
// block (block.scope ∈ TenantScopes(ar.TenantID), Modell C — one scope = one
// tenant). It returns (0, "") when the caller owns the block, else an HTTP status
// + a uniform message. Fail-closed on every unresolvability:
//   - a non-existent OR malformed block id collapses to the SAME 403 as a foreign
//     block (no existence oracle past the admin gate);
//   - an Altbestands-scope without a context_tenant_scopes mapping is in NO
//     tenant's owned set → not in the caller's → 403 (the §5.1 unresolvability arm);
//   - an empty caller tenant yields an empty owned set → 403.
func (h *ManageHandler) blockOwnershipGate(ctx context.Context, ar *auth.AuthResult, blockID string) (int, string) {
	blockScope, err := store.BlockScope(ctx, h.pool, blockID)
	if errors.Is(err, store.ErrBlockNotFound) {
		return http.StatusForbidden, "caller does not own the block"
	}
	if err != nil {
		return http.StatusInternalServerError, "ownership check failed"
	}
	ownerScopes, err := store.TenantScopes(ctx, h.pool, ar.TenantID)
	if err != nil {
		return http.StatusInternalServerError, "ownership check failed"
	}
	if !contains(ownerScopes, blockScope) {
		return http.StatusForbidden, "caller does not own the block"
	}
	return 0, ""
}

// handleBlockGrantCreate shares ONE block with a grantee tenant. It enforces, in
// order: (1) §5.1 ownership (the ONLY cross-tenant-exfiltration guard, above), and
// (2) §5.2 cross-tenant opt-in. After ownership passes, ar.TenantID IS the owner
// tenant (Modell C); a grantee that differs from it is cross-tenant and needs the
// per-tenant opt-in (default false). An empty/unresolvable grantee differs from the
// owner → treated as cross-tenant → DENY (fail-closed). The grantee FK is the
// final backstop for an unregistered tenant.
//
// TENANT-DECISION(allow-cross-tenant-block-grant): default false (opt-in), analog
// E5 — umentscheidbar weil additiver Settings-Gate.
func (h *ManageHandler) handleBlockGrantCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var spec blockGrantSpec
	if len(req.Data) == 0 || json.Unmarshal(req.Data, &spec) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required (block_id, grantee_tenant)"})
		return
	}
	spec.BlockID = strings.TrimSpace(spec.BlockID)
	spec.GranteeTenant = strings.TrimSpace(spec.GranteeTenant)
	if spec.BlockID == "" || spec.GranteeTenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "block_id and grantee_tenant are required"})
		return
	}
	ctx := r.Context()

	// §5.1 ownership gate (KRITISCH — the only cross-tenant exfiltration guard).
	if status, msg := h.blockOwnershipGate(ctx, ar, spec.BlockID); status != 0 {
		writeJSON(w, status, map[string]any{"success": false, "error": msg})
		return
	}
	// §5.2 cross-tenant opt-in gate. ar.TenantID == owner tenant here (ownership
	// passed). A grantee != owner is cross-tenant; an empty/unresolvable grantee
	// differs from the (non-empty) owner → also cross-tenant → DENY by default.
	if spec.GranteeTenant != ar.TenantID {
		allowed, aerr := store.TenantAllowsCrossTenantBlockGrant(ctx, h.pool, ar.TenantID)
		if aerr != nil || !allowed {
			// fail-closed: a read error is NEVER treated as opt-in.
			writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "cross-tenant block grant not permitted"})
			return
		}
	}

	g, err := store.CreateBlockGrant(ctx, h.pool, spec.BlockID, spec.GranteeTenant, ar.ApiKeyID)
	switch {
	case errors.Is(err, store.ErrBlockGrantUnknownTarget):
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "unknown block or grantee tenant"})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "create block grant failed"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "grant": g})
	}
}

// handleBlockGrantList returns the grants MADE BY the caller's tenant — the
// owner-side "what have I shared, and to whom" view (the only list a caller is
// entitled to; no global grant-topology oracle). req.ID, if present, narrows to
// one block.
func (h *ManageHandler) handleBlockGrantList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	grants, err := store.ListBlockGrants(r.Context(), h.pool, ar.TenantID, req.ID)
	if errors.Is(err, store.ErrBlockGrantUnknownTarget) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "malformed block_id filter"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "list block grants failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "grants": grants})
}

// handleBlockGrantRevoke removes one block grant. The ownership gate (§5.1)
// applies — you may only revoke a grant on a block you OWN — but the cross-tenant
// opt-in does NOT (removing access is always the safe direction). Revoke takes
// effect at the grantee's next auth/query (design/07 §5.6).
func (h *ManageHandler) handleBlockGrantRevoke(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var spec blockGrantSpec
	if len(req.Data) == 0 || json.Unmarshal(req.Data, &spec) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required (block_id, grantee_tenant)"})
		return
	}
	spec.BlockID = strings.TrimSpace(spec.BlockID)
	spec.GranteeTenant = strings.TrimSpace(spec.GranteeTenant)
	if spec.BlockID == "" || spec.GranteeTenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "block_id and grantee_tenant are required"})
		return
	}
	ctx := r.Context()
	if status, msg := h.blockOwnershipGate(ctx, ar, spec.BlockID); status != 0 {
		writeJSON(w, status, map[string]any{"success": false, "error": msg})
		return
	}
	err := store.RevokeBlockGrant(ctx, h.pool, spec.BlockID, spec.GranteeTenant)
	if errors.Is(err, store.ErrBlockGrantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "block grant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "revoke block grant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "revoked": map[string]string{"block_id": spec.BlockID, "grantee_tenant": spec.GranteeTenant}})
}
