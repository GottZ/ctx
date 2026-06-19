package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// Tenant lifecycle manage-actions (MT T05a + T05b, Achse 01-T5; design/01 §4.3).
// All admin-gated via actionRequiresAdmin. T05a = create/list/get/update; T05b =
// delete = full-prune (this slice). The one remaining T05 concern is carved out
// along the design's own seam and deferred:
//   - E6 engine-turn suspend gate (per-turn status re-check, addendum §6.4,
//     chat/engine.go:208) = T05c. Note: status='suspended' set via tenant-update
//     ALREADY bites at the next auth through the 060 ctx_auth status gate; T05c
//     only adds the cut for already-running long-lived sessions (R-LEAK6).

// reservedSlug reports whether a tenant slug is in the system-reserved namespace
// ('_'-prefix, e.g. '_global' = the settings identity). design/01 §4.3 Finding
// 17: this is a SEPARATE gate from firstReservedScope (which checks home/allowed
// SCOPES, not a tenant slug — a new slug field does NOT inherit that gate). The
// 'default' slug is NOT '_'-prefixed and is protected only by UNIQUE(slug), so a
// second tenant-create slug='default' is a 409 (ErrTenantSlugExists), not 400.
func reservedSlug(slug string) bool {
	return strings.HasPrefix(slug, "_")
}

type tenantSpec struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

// validTenantStatus is the lifecycle CHECK domain (059). Validated in the handler
// (→ 400) ahead of the DB CHECK backstop (23514).
func validTenantStatus(s string) bool {
	return s == "active" || s == "suspended" || s == "offboarding"
}

func (h *ManageHandler) handleTenantCreate(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	var spec tenantSpec
	if len(req.Data) == 0 || json.Unmarshal(req.Data, &spec) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required (slug, display_name)"})
		return
	}
	// Normalize the slug BEFORE the reserved-namespace check: a raw HasPrefix
	// would let a leading-whitespace slug (" _global") slip past the '_'-gate and
	// plant a near-homograph of the system sentinel. Trim, then validate + store
	// the trimmed value.
	spec.Slug = strings.TrimSpace(spec.Slug)
	if spec.Slug == "" || spec.DisplayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "slug and display_name are required"})
		return
	}
	if reservedSlug(spec.Slug) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "slug names starting with '_' are reserved (system namespace)"})
		return
	}
	tn, err := store.CreateTenant(r.Context(), h.pool, spec.Slug, spec.DisplayName)
	if errors.Is(err, store.ErrTenantSlugExists) {
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "tenant slug already exists"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "create tenant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tenant": tn})
}

func (h *ManageHandler) handleTenantList(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, _ manageRequest) {
	tenants, err := store.ListTenants(r.Context(), h.pool)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "list tenants failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tenants": tenants})
}

func (h *ManageHandler) handleTenantGet(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	tn, err := store.GetTenant(r.Context(), h.pool, req.ID)
	if errors.Is(err, store.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "get tenant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tenant": tn})
}

func (h *ManageHandler) handleTenantUpdate(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	// status comes from req.Status; an optional display_name patch from data.
	displayName := ""
	if len(req.Data) > 0 {
		var spec tenantSpec
		if err := json.Unmarshal(req.Data, &spec); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "unparseable data payload"})
			return
		}
		displayName = spec.DisplayName
	}
	if req.Status == "" && displayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "nothing to update (provide status and/or display_name)"})
		return
	}
	if req.Status != "" && !validTenantStatus(req.Status) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "status must be active, suspended, or offboarding"})
		return
	}
	tn, err := store.UpdateTenant(r.Context(), h.pool, req.ID, req.Status, displayName)
	if errors.Is(err, store.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "update tenant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tenant": tn})
}

// defaultTenantID is the fixed UUID of the single-tenant default tenant (059, E9
// slug 'default') that carries the ENTIRE legacy corpus (all of private/work/
// shared). It is referenced here only to GUARD it against tenant-delete. The
// canonical definition lives in the store layer (store.DefaultTenantID, T06);
// this is a package-local alias so the guard reads cleanly.
//
// TENANT-DECISION(prune-default-guard): tenant-delete REFUSES the default tenant
// (400) — a full-prune of it would destroy the whole single-tenant corpus (every
// block in every scope), irreversibly (W17 minimal blast-radius). The guard lives
// at the handler (policy) layer, NOT in store.PruneTenant (mechanism), so
// test-tenant teardown and a future deliberate operator path can still reach the
// raw prune. Umentscheidbar: an operator who genuinely offboards the default
// tenant must lift this guard explicitly.
const defaultTenantID = store.DefaultTenantID

// handleTenantDelete runs the T05b full-prune (store.PruneTenant): the FK-ordered,
// batched mass-DELETE of the tenant's scope-carried data, its keys, then the
// tenant row (design/01 §4.3.1). Admin-gated upstream (actionRequiresAdmin); the
// default tenant is additionally guarded (400). Unknown/malformed id → 404.
func (h *ManageHandler) handleTenantDelete(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	if req.ID == defaultTenantID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "the default tenant cannot be deleted"})
		return
	}
	err := store.PruneTenant(r.Context(), h.pool, req.ID)
	if errors.Is(err, store.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "delete tenant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": req.ID})
}

// Cross-tenant grant manage-actions (MT T17, Achse 02-V4; design/02 §V4). The
// friend-tenant read channel: an admin grants tenant B read access to a scope
// owned by tenant A. Read takes effect at the grantee's NEXT auth (ctx_auth, 060,
// re-resolves read_scopes per request) — these actions never mutate a running
// session, and never touch the WRITE side. Admin-gated via actionRequiresAdmin
// using the server-global is_admin; per-tenant-admin scoping is Achse 05 (T25,
// §V4 deps note) — explicitly named, no hidden coupling.

type grantSpec struct {
	GranteeTenant string `json:"grantee_tenant"`
	GrantedScope  string `json:"granted_scope"`
}

func (h *ManageHandler) handleTenantGrantCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var spec grantSpec
	if len(req.Data) == 0 || json.Unmarshal(req.Data, &spec) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required (grantee_tenant, granted_scope)"})
		return
	}
	spec.GrantedScope = strings.TrimSpace(spec.GrantedScope)
	if spec.GranteeTenant == "" || spec.GrantedScope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "grantee_tenant and granted_scope are required"})
		return
	}
	// '_'-system scopes are never grantable. The granted_scope FK to
	// context_tenant_scopes is the fail-closed backstop (a '_'-scope is not an
	// ownable, registered scope → store ErrGrantUnknownTarget), but reject early
	// with a clear 400 — same reserved-namespace rule as reservedSlug/api_keys.go:44.
	if strings.HasPrefix(spec.GrantedScope, "_") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "system scopes ('_'-prefixed) cannot be granted"})
		return
	}
	g, err := store.CreateTenantGrant(r.Context(), h.pool, spec.GranteeTenant, spec.GrantedScope, ar.ApiKeyID)
	switch {
	case errors.Is(err, store.ErrGrantExists):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "grant already exists"})
	case errors.Is(err, store.ErrGrantUnknownTarget):
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "unknown grantee tenant or unregistered scope"})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "create grant failed"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "grant": g})
	}
}

func (h *ManageHandler) handleTenantGrantList(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	// req.ID, if present, narrows to one grantee tenant's grants; empty = all.
	grants, err := store.ListTenantGrants(r.Context(), h.pool, req.ID)
	if errors.Is(err, store.ErrGrantUnknownTarget) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "malformed grantee_tenant filter"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "list grants failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "grants": grants})
}

func (h *ManageHandler) handleTenantGrantDelete(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	err := store.DeleteTenantGrant(r.Context(), h.pool, req.ID)
	if errors.Is(err, store.ErrGrantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "grant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "delete grant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": req.ID})
}

// dispatchTenantAction fans the tenant-* lifecycle actions out (split from
// HandleManage's switch for cyclomatic budget, mirroring dispatchBackendAction).
// It also routes the tenant-grant-* cross-tenant read-grant actions (T17, §V4):
// same tenant family, same admin gate.
func (h *ManageHandler) dispatchTenantAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "tenant-create":
		h.handleTenantCreate(w, r, ar, req)
	case "tenant-list":
		h.handleTenantList(w, r, ar, req)
	case "tenant-get":
		h.handleTenantGet(w, r, ar, req)
	case "tenant-update":
		h.handleTenantUpdate(w, r, ar, req)
	case "tenant-delete":
		h.handleTenantDelete(w, r, ar, req)
	case "tenant-grant-create":
		h.handleTenantGrantCreate(w, r, ar, req)
	case "tenant-grant-list":
		h.handleTenantGrantList(w, r, ar, req)
	case "tenant-grant-delete":
		h.handleTenantGrantDelete(w, r, ar, req)
	}
}
