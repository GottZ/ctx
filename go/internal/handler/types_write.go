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
// split mutationScope() applies to settings/secrets (tenant_scope.go:20-28).
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
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// ── D6 overlay write gate (board decision E2-6, wave C2-4) ───────────────────
//
// A tenant overlay may only NARROW the '_global' base policy of the same name.
// The rule itself lives in blocktype.NarrowingViolation (overlay_narrowing.go,
// with the per-axis provenance); this seam resolves the base and turns a
// violation into the handler's 422.
//
// It closes ONE finding with two sites: reports/bau/ops-w1-review.md #3 / A4
// (an overlay lifting `checkpoint` to full-pass puts the name into
// p_types_visible, where migration 145's static deny conjunct cuts its FTS
// contribution — measured 67 -> 0 rows) and reports/bau/w01-7.md §6 finding 3
// ("on the write path there is NO lock against a type-update that overwrites a
// derived row per tenant").
//
// 422 and not 403: the refusal is a property of the CONFIG, not of the caller.
// A server-admin writing into a tenant namespace through the manage transport
// gets the same answer, and DecodePolicy's own rejections — the other reason a
// well-authorized write fails on its content — already answer 422.
//
// The gate is on CREATE and UPDATE only. DELETE returns the tenant to the base,
// which is by definition the widest ADMISSIBLE position, so it cannot violate
// the rule.

// overlayWriteViolation is the gate. Empty message = admissible. The error
// return is a genuine failure to RESOLVE the base (DB error, corrupt '_global'
// row) and must fail the request closed — never fall through to the write.
func overlayWriteViolation(ctx context.Context, pool *pgxpool.Pool, name, scope string, overlay blocktype.Policy) (string, error) {
	// The narrowing clauses first, unchanged, and ONLY outside the base
	// namespace: '_global' IS the base, it has nothing to narrow against.
	if scope != store.GlobalScope {
		if msg := hardDenyOverlayViolation(name, overlay); msg != "" {
			return msg, nil
		}
		base, ok, err := overlayBasePolicy(ctx, pool, name)
		if err != nil {
			return "", err
		}
		// no base = a genuinely tenant-own type; the narrowing rule does not
		// apply — but the C2-8 clause below still does, and it is a no-op for
		// such a name (no compiled floor, nothing to keep).
		if ok {
			if msg := blocktype.NarrowingViolation(base, overlay); msg != "" {
				return msg, nil
			}
		}
	}
	// C2-8, LAST and in EVERY scope. Last, because it would otherwise rename
	// the axis of an already-covered refusal: the C2-4 gate probes a tenant
	// overlay that lifts `insight` to full-pass and asserts the answer names
	// retrieval.policy — such a body drops the write lock as well, and this
	// clause running first turned that 422 into a write.internal_only verdict
	// (measured: TestC24OverlayWriteGate/A2 red on exactly that string). Every
	// scope, because unlike the axes above its base is COMPILED IN rather than
	// a row, so the base namespace is the one place it has something to say.
	return internalOnlyWriteViolation(name, overlay), nil
}

// internalOnlyWriteViolation is the Zero-Value bolt of wave C2-8 (design D-02
// §3.1 "Zero-Value-Falle in DecodePolicy", §7 A02-1 negative probe
// "Zero-Value-Registry"). Empty string = admissible.
//
// THE HOLE, measured on the pre-wave tree (2026-08-27): a server-admin
// `PUT /api/types/insight` with the body `{"config":{"v":1}}` answered 200 and
// left the row at retrieval=full-pass, guard.check=true, guard.candidate=true,
// dream.linkable=true, digest=true, overview=true, untrusted=false,
// classify.priority=100 — every promise of migration 143 inverted in ONE write.
// Two properties combined into it: a config is a COMPLETE policy and
// DecodePolicy default-fills every absent section with its WIDE value
// (policy.go), and overlayWriteViolation left a '_global' write ungated
// entirely, because there is no base ROW to narrow against. The I7/S1 name lock
// does not cover it either — that one
// governs who may CLAIM the type on a BLOCK write, never what the type's policy
// says.
//
// WHY THE BASE IS THE COMPILED FLOOR AND NOT THE ROW. Reading the current row
// would make the rule self-referential: once a row had lost the flag, every
// later write would be measured against the loosened value and the bolt would
// never fire again. blocktype.BuiltinPolicy is the one base an API write cannot
// have moved. It is also why this clause is NOT an eleventh axis in
// NarrowingViolation: that function compares an overlay against a BASE ROW, and
// on the '_global' row itself there is no such base.
//
// ONE AXIS, DELIBERATELY. This is not "a '_global' builtin may only narrow its
// floor", and it must not grow into one: masterplan K7 / board decision E-4
// plan exactly one WIDENING data position on these two rows (retrieval
// excluded -> damped with the swept factor from M-W8), and a floor check over
// the retrieval axis would take that reversible setting away from the API. The
// remaining axes of a zero-value body stay reachable for a server-admin on a
// '_global' row; that is the general narrowing doctrine, which overlay_
// narrowing.go names as D-01's decision rather than this wave's.
func internalOnlyWriteViolation(name string, overlay blocktype.Policy) string {
	floor, ok := blocktype.BuiltinPolicy(name)
	if !ok || !floor.Write.InternalOnly || overlay.Write.InternalOnly {
		return ""
	}
	return fmt.Sprintf("write.internal_only: %q is a server write target and stays "+
		"write.internal_only=true in every scope — its blocks are written by ctxd through the "+
		"internal path, never claimed by a client; send the complete policy including "+
		`{"write":{"internal_only":true}}`, name)
}

// hardDenyOverlayViolation is the one clause that does NOT depend on the base.
//
// shadowDenyTypes (query_shadow.go:50) is the hard deny-list — the SAME map,
// not a second copy: `checkpoint` carries 5 955 blocks, 13 of them
// sensitivity=credentials, and §5 B3 rests their protection on "not
// retrievable". Migration 145's index predicate and the static FTS conjunct are
// literals over exactly these two names, so their invisibility may not hang on
// what the '_global' row happens to say at write time.
func hardDenyOverlayViolation(name string, overlay blocktype.Policy) string {
	if !shadowDenyTypes[name] {
		return ""
	}
	if overlay.Retrieval.Kind != blocktype.RetrievalExcluded {
		return fmt.Sprintf("retrieval.policy: %q is on the hard deny-list and stays retrieval.policy=%q "+
			"in every tenant overlay (got %q)", name, blocktype.RetrievalExcluded, overlay.Retrieval.Kind)
	}
	if overlay.Retrieval.ShadowMeasurable {
		return fmt.Sprintf("retrieval.shadow_measurable: %q is on the hard deny-list and can never be "+
			"measurable, in any scope", name)
	}
	return ""
}

// overlayBasePolicy resolves the '_global' base policy of name the way Reload
// composes it: the compiled-in floor with the '_global' TABLE row over it
// (registry.go:393-411, the row wins wholesale). Deliberately read from the
// pool rather than from a *blocktype.Registry — the registry may be nil (test
// wiring, read-only deployments) or degraded to builtin-fallback, and a write
// gate that silently loosens in either state would be the fail-open class.
// false = the name has no '_global' base at all: a tenant-own type, unrestricted.
func overlayBasePolicy(ctx context.Context, pool *pgxpool.Pool, name string) (blocktype.Policy, bool, error) {
	row, err := store.GetBlockType(ctx, pool, name, []string{store.GlobalScope})
	if err != nil {
		return blocktype.Policy{}, false, err
	}
	if row == nil {
		p, ok := blocktype.BuiltinPolicy(name)
		return p, ok, nil
	}
	p, err := blocktype.DecodePolicy(row.Name, row.Scope, row.Builtin, row.IsDefault, row.Config)
	if err != nil {
		return blocktype.Policy{}, false, err
	}
	return p, true, nil
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
		internalError(w, ctx, "types: put read body", err)
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
		internalError(w, ctx, "types: put lookup", err)
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
	pol, err := blocktype.DecodePolicy(name, ws, false, isDefault, cfg)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}
	// D6 overlay gate: this is the create path that reaches a '_global' name
	// whenever its TABLE row is absent (HandlePut resolves against the table,
	// the registry resolves off the compiled-in floor too).
	if msg, err := overlayWriteViolation(ctx, h.pool, name, ws, pol); err != nil {
		internalError(w, ctx, "types: put-create overlay base", err)
		return
	} else if msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
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
		pol, err := blocktype.DecodePolicy(cur.Name, cur.Scope, cur.Builtin, cur.IsDefault, p.Config)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
			return
		}
		// D6 overlay gate on the row's OWN scope: this is the path an existing
		// tenant row takes, whatever put it there.
		if msg, err := overlayWriteViolation(ctx, h.pool, cur.Name, cur.Scope, pol); err != nil {
			internalError(w, ctx, "types: put-update overlay base", err)
			return
		} else if msg != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
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
		internalError(w, ctx, "types: delete lookup", err)
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
