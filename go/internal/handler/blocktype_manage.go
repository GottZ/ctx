// Manage family type-* (WF T10, design/01 §5.4/§7-T10): first API exposure
// of the block-type registry. Reads (type-list/type-get) are tierOpen —
// every UI needs type metadata for badges — but HANDLER-scoped to
// '_global' ∪ the caller's own tenant namespace (K-T1: the gate admits, the
// handler scopes). Mutations are tierServerAdmin in tier 1 (only the
// '_global' namespace exists; the tenant-admin tier for tenant rows is wave
// T12) and deliberately carry NO second admin check here: the actionTier
// entry is the single gate, so the DB-less 403 probe stays a TRUE tier-gate
// probe (remove the entry ⇒ the probe reaches the store layer and fails red
// — the §5.4 fail-open trap made visible).
//
// Validation authority: blocktype.DecodePolicy — the SAME function the
// reload path uses (§3.3). No second validation path exists; a config this
// handler accepts is a config the registry can load.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

// Row-column caps (design/01 §3.3 R1 — resource-exhaustion guard; the
// config-JSONB + array caps live in blocktype.DecodePolicy, these two are
// row columns outside the envelope).
const (
	maxTypeDisplayName = 200
	maxTypeDescription = 2000
)

// typeCreatePayload is the strict type-create shape (unknown keys ⇒ 400,
// §5.2 typo class: a misspelled field must never silently default).
type typeCreatePayload struct {
	Name        string          `json:"name"`
	Scope       string          `json:"scope,omitempty"`
	DisplayName string          `json:"display_name,omitempty"`
	Description string          `json:"description,omitempty"`
	IsDefault   bool            `json:"is_default,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
}

// typeUpdatePayload is the strict type-update PATCH shape. name/scope are
// row identity (builtin name+scope immutable by design §3.2 — and a rename
// would orphan every referencing block), is_default is a two-row swap that
// ships with a real consumer; none of them is accepted here, and the strict
// decoder rejects them loudly instead of ignoring them.
type typeUpdatePayload struct {
	DisplayName *string         `json:"display_name,omitempty"`
	Description *string         `json:"description,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
}

// dispatchTypeAction fans the type-* actions out (split from HandleManage's
// switch for the cyclomatic budget, mirrors dispatchBackendAction). Tier
// gating happened upstream in enforceActionTier.
func (h *ManageHandler) dispatchTypeAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "type-list":
		h.handleTypeList(w, r, ar)
	case "type-get":
		h.handleTypeGet(w, r, ar, req)
	case "type-create":
		h.handleTypeCreate(w, r, ar, req)
	case "type-update":
		h.handleTypeUpdate(w, r, ar, req)
	case "type-delete":
		h.handleTypeDelete(w, r, ar, req)
	}
}

// typeVisibleScopes is the namespace set a caller may SEE (K-T1 handler
// scoping for the tierOpen reads): the shipped '_global' rows plus rows of
// the caller's own tenant namespace. In tier 1 only '_global' rows exist —
// the tenant term is inert until T12 populates tenant rows; it is wired now
// so the read shape does not change when tier 2 lands.
func typeVisibleScopes(ar *auth.AuthResult) []string {
	scopes := []string{store.GlobalScope}
	if ar.TenantID != "" {
		scopes = append(scopes, ar.TenantID)
	}
	return scopes
}

func (h *ManageHandler) handleTypeList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	types, err := store.ListBlockTypes(ctx, h.pool, typeVisibleScopes(ar))
	if err != nil {
		slog.Error("manage: type-list error", "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "type-list",
		"success": true,
		"types":   types,
	})
}

func (h *ManageHandler) handleTypeGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id (type UUID or name)",
		})
		return
	}
	bt, err := store.GetBlockType(ctx, h.pool, req.ID, typeVisibleScopes(ar))
	if err != nil {
		slog.Error("manage: type-get error", "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if bt == nil {
		// 404-no-oracle (T24 shape): a foreign-tenant row reads identically.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": "Type not found",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "type-get",
		"success": true,
		"type":    bt,
	})
}

func (h *ManageHandler) handleTypeCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	var p typeCreatePayload
	if msg := decodeStrict(req.Data, &p); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": msg})
		return
	}
	// Scope binding (§5.4 R1): tier 1 writes ONLY the '_global' namespace.
	// The tier gate already restricted the caller to server-admin; any other
	// requested scope is rejected until T12 ships the tenant-row tier — a
	// silently accepted foreign scope would be the S2 anti-pattern.
	if p.Scope != "" && p.Scope != store.GlobalScope {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false,
			"error":   "scope: only '_global' is writable in tier 1 (tenant-scoped types ship with the tenant-override wave)",
		})
		return
	}
	if msg := typeRowCaps(p.DisplayName, p.Description); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
		return
	}
	cfg := p.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{"v": 1}`) // all-defaults envelope (§3.3)
	}
	// THE validation authority (§3.3): name format, envelope version, strict
	// keys, cross-field rules, caps — same code path as the reload decoder.
	if _, err := blocktype.DecodePolicy(p.Name, store.GlobalScope, false, p.IsDefault, cfg); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}

	bt, err := store.CreateBlockType(ctx, h.pool, store.BlockTypeWrite{
		Name:        p.Name,
		Scope:       store.GlobalScope,
		DisplayName: p.DisplayName,
		Description: p.Description,
		IsDefault:   p.IsDefault,
		Config:      cfg,
	}, &ar.ApiKeyID, reqID)
	if err != nil {
		h.writeTypeStoreError(w, "type-create", err, reqID)
		return
	}
	h.reloadBlockTypes(ctx, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "type-create",
		"success": true,
		"type":    bt,
	})
}

func (h *ManageHandler) handleTypeUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id (type UUID or name)",
		})
		return
	}
	var p typeUpdatePayload
	if msg := decodeStrict(req.Data, &p); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": msg})
		return
	}
	if p.DisplayName == nil && p.Description == nil && len(p.Config) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "No fields to update (display_name, description, config)",
		})
		return
	}
	if msg := typeRowCaps(strOrEmpty(p.DisplayName), strOrEmpty(p.Description)); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
		return
	}

	// Resolve the row first: the new config validates against the ROW's
	// identity (name/scope/builtin/is_default feed the cross-field rules).
	cur, err := store.GetBlockType(ctx, h.pool, req.ID, typeVisibleScopes(ar))
	if err != nil {
		slog.Error("manage: type-update lookup error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
		return
	}
	if cur == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Type not found"})
		return
	}
	upd := store.BlockTypeUpdate{DisplayName: p.DisplayName, Description: p.Description}
	if len(p.Config) > 0 {
		if _, err := blocktype.DecodePolicy(cur.Name, cur.Scope, cur.Builtin, cur.IsDefault, p.Config); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
			return
		}
		upd.Config = p.Config
	}

	bt, err := store.UpdateBlockType(ctx, h.pool, cur.ID, typeVisibleScopes(ar), upd, &ar.ApiKeyID, reqID)
	if err != nil {
		h.writeTypeStoreError(w, "type-update", err, reqID)
		return
	}
	if bt == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Type not found"})
		return
	}
	h.reloadBlockTypes(ctx, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "type-update",
		"success": true,
		"type":    bt,
	})
}

func (h *ManageHandler) handleTypeDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id (type UUID or name)",
		})
		return
	}
	cur, err := store.GetBlockType(ctx, h.pool, req.ID, typeVisibleScopes(ar))
	if err != nil {
		slog.Error("manage: type-delete lookup error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
		return
	}
	if cur == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Type not found"})
		return
	}

	found, err := store.DeleteBlockType(ctx, h.pool, cur.ID, typeVisibleScopes(ar), &ar.ApiKeyID, reqID)
	if err != nil {
		h.writeTypeStoreError(w, "type-delete", err, reqID)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Type not found"})
		return
	}
	h.reloadBlockTypes(ctx, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "type-delete",
		"success": true,
		"deleted": cur.Name,
	})
}

// reloadBlockTypes refreshes the registry snapshot synchronously after a
// successful mutation (pattern: backend-* post-mutation pool reload) so the
// caller's next request sees the new policy at once. The 072 NOTIFY trigger
// fires too — this is a latency optimization, not the consistency mechanism
// — so a failure only logs (the listener reload heals eventually). nil
// registry = test wiring without blocktype consumers.
func (h *ManageHandler) reloadBlockTypes(ctx context.Context, reqID string) {
	if h.blocktypes == nil {
		return
	}
	if err := h.blocktypes.Reload(ctx, h.pool); err != nil {
		slog.Warn("manage: block-type registry reload after mutation failed — NOTIFY listener will retry",
			"error", err, "request_id", reqID)
	}
}

// decodeStrict decodes a manage payload with DisallowUnknownFields (§5.2
// typo class). Empty msg = ok; otherwise the 400 message with the offending
// key in the decoder error.
func decodeStrict(data json.RawMessage, into any) string {
	if len(data) == 0 {
		return "Missing required field: data"
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Sprintf("Invalid data format: %v", err)
	}
	return ""
}

// validateBlockTypeName checks a block-write `type` value against the
// registry snapshot (WF T10). Empty msg = registered. Fail-closed: a nil
// registry (test wiring without blocktype consumers) REJECTS — writing an
// unvalidated type name past a missing registry would be the §5.1(b) hole.
func (h *ManageHandler) validateBlockTypeName(ctx context.Context, name string) string {
	if h.blocktypes == nil {
		return "type: block-type registry not wired — cannot validate type names"
	}
	if _, ok := h.blocktypes.SnapshotForRequest(ctx).Resolve(name); !ok {
		return fmt.Sprintf("type: unknown block type %q (see manage type-list)", name)
	}
	return ""
}

// unionExcludes merges the canonical types_exclude with its legacy alias
// block_roles_exclude (seam 17). Both present ⇒ the UNION applies —
// monotone-restrictive (more excluded = narrower result), so the defined
// behaviour can never widen what either name alone would return. Order-
// preserving dedup, nil when both are empty (= no filter conjunct).
func unionExcludes(canonical, alias []string) []string {
	if len(alias) == 0 {
		return canonical
	}
	if len(canonical) == 0 {
		return alias
	}
	seen := make(map[string]bool, len(canonical)+len(alias))
	out := make([]string, 0, len(canonical)+len(alias))
	for _, s := range append(append([]string{}, canonical...), alias...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// typeRowCaps enforces the §3.3 R1 row-column caps with the field path in
// the message (the config/array caps live in blocktype.DecodePolicy).
func typeRowCaps(displayName, description string) string {
	if len(displayName) > maxTypeDisplayName {
		return fmt.Sprintf("display_name exceeds %d characters (%d)", maxTypeDisplayName, len(displayName))
	}
	if len(description) > maxTypeDescription {
		return fmt.Sprintf("description exceeds %d characters (%d)", maxTypeDescription, len(description))
	}
	return ""
}

// writeTypeStoreError maps the store sentinel errors of the type-* family
// onto HTTP statuses: exists/default-collision/in-use ⇒ 409, builtin-delete
// ⇒ 409 (a permanent conflict with the shipped registry), rest ⇒ 500.
func (h *ManageHandler) writeTypeStoreError(w http.ResponseWriter, action string, err error, reqID string) {
	var inUse *store.BlockTypeInUseError
	switch {
	case errors.Is(err, store.ErrBlockTypeExists),
		errors.Is(err, store.ErrBlockTypeDefaultExists),
		errors.Is(err, store.ErrBlockTypeBuiltin):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": err.Error()})
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false,
			"error":   inUse.Error(),
			"blocks":  map[string]int{"active": inUse.Active, "archived": inUse.Archived},
		})
	default:
		slog.Error("manage: "+action+" error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
	}
}
