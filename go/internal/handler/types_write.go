// /api/types write surface (workflow W2, design/03 §4.1/§4.2/§9.5(c)).
// PUT and DELETE mount in the SAME MountTypes function as the W1 reads
// (types.go), behind RequireAdminOrTenantAdmin — the §5.1 doctrine: the gate
// lives with the routes, so the fail-open class is impossible (a forgotten gate
// deletes the route, not the protection).
//
// Write-scope is PINNED by role, never taken from the request body (§5.2(2)
// scope-injection class): a server-admin writes the shipped '_global'
// namespace, a tenant-admin writes ONLY its own tenant namespace. This is the
// backendWriteScopes precedent named in §9.5(c) — the same two-write-worlds
// split writeScope() applies to settings/secrets (tenant_scope.go:20-28).
// A tenant-admin that targets a '_global' type is refused 403, NOT 404: global
// types are member-visible via GET /api/types, so there is no existence oracle
// to protect — the honest "operator-only" answer is correct.
//
// ONE write LOGIC, two transports (Iteration-9 reconciliation, §4.1). This
// handler and the manage type-create/update/delete family (T10,
// blocktype_manage.go) call the IDENTICAL store functions
// (store.Create/Update/DeleteBlockType), the IDENTICAL validation authority
// (blocktype.DecodePolicy), the IDENTICAL row caps (typeRowCaps) and the
// IDENTICAL error mapper (writeBlockTypeStoreError). No mutation logic is
// duplicated. The '_global' write path carries the SAME authority
// (server-admin) on both transports — there is no divergent gate (the §5.1
// anti-pattern). The tenant-admin write path exists ONLY on REST; the manage
// family stays operator-/'_global'-only per its T10 tier-1 contract and remains
// fully functional for its MCP/CLI consumers (no silent removal — the manage
// actions are NOT dropped, they are the operator transport of the same logic).

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
)

// typeWritePayload is the strict PUT body. name comes from the URL, scope is
// role-pinned — NEITHER is accepted here, and the strict decoder rejects them
// loudly (§5.2 typo/scope-injection class) instead of silently ignoring them.
// Pointers distinguish "field absent" (keep on update / default on create) from
// an explicit zero value, so a bare PUT never accidentally clears a field.
type typeWritePayload struct {
	DisplayName *string         `json:"display_name,omitempty"`
	Description *string         `json:"description,omitempty"`
	IsDefault   *bool           `json:"is_default,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
}

// typeWriteScope is the scope a CREATE targets, pinned by role (§9.5(c),
// backendWriteScopes precedent): server-admin → '_global', tenant-admin → its
// own tenant namespace (the tenant UUID, the same key typeVisibleScopes uses).
// A nil/invalid ar yields "" — fail-closed, canWriteTypeScope then refuses.
func typeWriteScope(ar *auth.AuthResult) string {
	if ar == nil {
		return ""
	}
	if ar.IsServerAdmin() {
		return store.GlobalScope
	}
	return ar.TenantID
}

// canWriteTypeScope reports whether ar may mutate a type in the given scope:
// '_global' is server-admin-only; any tenant scope requires the caller to be
// that tenant's admin (IsTenantAdminOf also returns true for a server-admin).
// The mount gate already admitted only server-admins and tenant-admins-of-
// their-own-tenant, so this is the second, data-side half of the K-T1 pairing.
func canWriteTypeScope(ar *auth.AuthResult, scope string) bool {
	if ar == nil || scope == "" {
		return false
	}
	if scope == store.GlobalScope {
		return ar.IsServerAdmin()
	}
	return ar.IsTenantAdminOf(scope)
}

// forbidTypeWrite writes the uniform 403 for a caller that is admitted by the
// mount gate but not authorized for the TARGET scope (tenant-admin vs a
// '_global' type). Not an oracle: '_global' types are member-visible.
func forbidTypeWrite(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"success": false,
		"error":   "global block types are managed by the server operator",
	})
}

// HandlePut implements PUT /api/types/{name} — an upsert. The row is resolved
// in the caller's visible namespaces; an existing row is updated in place
// (authority = its own scope), an unknown name is created in the caller's
// role-pinned write scope. '_global' rows are server-admin-only (403 for a
// tenant-admin — the §7-W2 gate).
func (h *TypesHandler) HandlePut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	ar := AuthResultFromContext(ctx)
	name := chi.URLParam(r, "name")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		internalTypesError(w, ctx, "types: put read body", err)
		return
	}
	var p typeWritePayload
	if len(bytes.TrimSpace(body)) > 0 {
		if msg := decodeStrict(body, &p); msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": msg})
			return
		}
	}
	if msg := typeRowCaps(strOrEmpty(p.DisplayName), strOrEmpty(p.Description)); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
		return
	}

	cur, err := store.GetBlockType(ctx, h.pool, name, typeVisibleScopes(ar))
	if err != nil {
		internalTypesError(w, ctx, "types: put lookup", err)
		return
	}
	if cur != nil {
		h.putUpdate(w, r, ar, cur, p, reqID)
		return
	}
	h.putCreate(w, r, ar, name, p, reqID)
}

// putCreate creates a new type in the caller's role-pinned write scope.
func (h *TypesHandler) putCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, name string, p typeWritePayload, reqID string) {
	ctx := r.Context()
	ws := typeWriteScope(ar)
	if !canWriteTypeScope(ar, ws) {
		forbidTypeWrite(w)
		return
	}
	isDefault := p.IsDefault != nil && *p.IsDefault
	cfg := p.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{"v": 1}`) // all-defaults envelope (design/01 §3.3)
	}
	// THE validation authority (§3.3): name format, envelope version, strict
	// keys, cross-field rules, caps — the SAME code path the reload decoder runs.
	if _, err := blocktype.DecodePolicy(name, ws, false, isDefault, cfg); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}
	bt, err := store.CreateBlockType(ctx, h.pool, store.BlockTypeWrite{
		Name:        name,
		Scope:       ws,
		DisplayName: strOrEmpty(p.DisplayName),
		Description: strOrEmpty(p.Description),
		IsDefault:   isDefault,
		Config:      cfg,
	}, &ar.ApiKeyID, reqID)
	if err != nil {
		writeBlockTypeStoreError(w, "types put-create", err, reqID)
		return
	}
	h.reloadBlockTypes(ctx, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"type":    typeView{BlockTypeRow: *bt, Source: typeSourceForScope(bt.Scope)},
	})
}

// putUpdate updates an existing type in place. Authority = the row's own scope:
// '_global' rows are operator-only (403 for a tenant-admin). is_default is a
// two-row swap the store update deliberately does not carry (matches manage
// type-update); an attempt to change it on an existing type is a loud 422, never
// a silent no-op.
func (h *TypesHandler) putUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, cur *store.BlockTypeRow, p typeWritePayload, reqID string) {
	ctx := r.Context()
	if !canWriteTypeScope(ar, cur.Scope) {
		forbidTypeWrite(w)
		return
	}
	if p.IsDefault != nil && *p.IsDefault != cur.IsDefault {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false,
			"error":   "is_default cannot be changed on an existing type (default-swap is a separate operation)",
		})
		return
	}
	upd := store.BlockTypeUpdate{DisplayName: p.DisplayName, Description: p.Description}
	if len(p.Config) > 0 {
		// Config validates against the ROW's identity (name/scope/builtin/
		// is_default feed the cross-field rules) — same as manage type-update.
		if _, err := blocktype.DecodePolicy(cur.Name, cur.Scope, cur.Builtin, cur.IsDefault, p.Config); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
			return
		}
		upd.Config = p.Config
	}
	bt, err := store.UpdateBlockType(ctx, h.pool, cur.ID, typeVisibleScopes(ar), upd, &ar.ApiKeyID, reqID)
	if err != nil {
		writeBlockTypeStoreError(w, "types put-update", err, reqID)
		return
	}
	if bt == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Type not found"})
		return
	}
	h.reloadBlockTypes(ctx, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"type":    typeView{BlockTypeRow: *bt, Source: typeSourceForScope(bt.Scope)},
	})
}

// HandleDelete implements DELETE /api/types/{name}. The row is resolved in the
// caller's visible namespaces (404-no-oracle on unknown/foreign, matching the
// W1 get). '_global' rows are operator-only (403 for a tenant-admin). The store
// guard turns a builtin into ErrBlockTypeBuiltin (409) and a still-referenced
// type into *BlockTypeInUseError (409 + active/archived count) — the §7-W2
// gates.
func (h *TypesHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	ar := AuthResultFromContext(ctx)
	name := chi.URLParam(r, "name")

	cur, err := store.GetBlockType(ctx, h.pool, name, typeVisibleScopes(ar))
	if err != nil {
		internalTypesError(w, ctx, "types: delete lookup", err)
		return
	}
	if cur == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Type not found"})
		return
	}
	if !canWriteTypeScope(ar, cur.Scope) {
		forbidTypeWrite(w)
		return
	}
	found, err := store.DeleteBlockType(ctx, h.pool, cur.ID, typeVisibleScopes(ar), &ar.ApiKeyID, reqID)
	if err != nil {
		writeBlockTypeStoreError(w, "types delete", err, reqID)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Type not found"})
		return
	}
	h.reloadBlockTypes(ctx, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"deleted": cur.Name,
	})
}

// reloadBlockTypes refreshes the in-memory registry snapshot after a successful
// mutation so the writer's next request validates against the new policy at once
// (mirrors ManageHandler.reloadBlockTypes). The NOTIFY listener is the
// consistency mechanism; this is a latency optimization, so a failure only logs.
// nil registry = test wiring / read-only deployment.
func (h *TypesHandler) reloadBlockTypes(ctx context.Context, reqID string) {
	if h.blocktypes == nil {
		return
	}
	if err := h.blocktypes.Reload(ctx, h.pool); err != nil {
		slog.Warn("types: block-type registry reload after mutation failed — NOTIFY listener will retry",
			"error", err, "request_id", reqID)
	}
}
