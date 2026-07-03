// /api/types — the read-only block-type registry surface (workflow W1,
// design/03 §4.2/§5.1). This is the REST namespace the workflow axis puts OVER
// the Achse-01 type registry (context_block_types, migration 072); it consumes
// store.ListBlockTypes/GetBlockType read-only — the write surface (PUT/DELETE)
// is W2, the manage type-* family is Achse-01's first exposure and stays.
//
//	GET /api/types         effective type list (_global ∪ own tenant), member-gated
//	GET /api/types/{name}  single type incl. policy config + source badge
//
// Both routes are member-gated inside MountTypes (RequireMember): any valid key
// reads, an invalid/missing one gets 401. The gate only ADMITS — the handler
// scopes every read to typeVisibleScopes(ar) = ['_global'] ∪ the caller's own
// tenant namespace (K-T1: the gate admits, the handler scopes), so a foreign
// tenant's custom type is never listed and reads as 404-no-oracle on a direct
// get (§5.2(5): type names can leak project internals). The wire shape is the
// K5 freeze anchor (masterplan §3 K5: 01-§3.3 is the field truth) — pinned by
// TestTypesGoldenShape here and web/src/lib/api/types.ts on the TS side.

package handler

import (
	"context"
	"log/slog"
	"net/http"
	"sort"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TypesHandler serves the /api/types surface: the W1 read routes and the W2
// write routes (PUT/DELETE, types_write.go). blocktypes is the shared registry
// snapshot refreshed after a mutation so the writer's next request sees the new
// policy at once (latency optimization; the NOTIFY listener is the consistency
// mechanism — nil-safe for test wiring without a registry).
type TypesHandler struct {
	pool       *pgxpool.Pool
	blocktypes *blocktype.Registry
}

// NewTypesHandler creates a new TypesHandler. blocktypes may be nil (test
// wiring / read-only deployments); the write path then skips the eager reload
// and relies on the NOTIFY listener alone.
func NewTypesHandler(pool *pgxpool.Pool, blocktypes *blocktype.Registry) *TypesHandler {
	return &TypesHandler{pool: pool, blocktypes: blocktypes}
}

// MountTypes mounts GET /api/types[/{name}] behind RequireMember. ONE function
// used by server.go AND the gate tests (settings.go:73 doctrine), so the 401
// probe exercises exactly the chain production mounts. The gate lives in the
// SAME function as the routes — it cannot be forgotten without the routes
// themselves vanishing (design/03 §5.1: the fail-open class eliminated
// structurally, unlike a manage-action tier entry that defaults tierOpen).
func MountTypes(r chi.Router, h *TypesHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireMember)
		r.Get("/api/types", h.HandleList)
		r.Get("/api/types/{name}", h.HandleGet)
	})
	// W2 write surface (design/03 §4.2/§9.5(c)): PUT/DELETE live in the SAME
	// mount function as the reads, behind RequireAdminOrTenantAdmin — the gate
	// cannot be forgotten without the routes vanishing (§5.1, the fail-open
	// class eliminated structurally). The 1 MB body cap is inherited from the
	// enclosing server.go group (DefaultMaxBodySize, server.go:118). The write
	// scope is pinned by role inside the handler (typeWriteScope), never read
	// from the body — the gate ADMITS, the handler SCOPES (K-T1).
	r.Group(func(r chi.Router) {
		r.Use(RequireAdminOrTenantAdmin)
		r.Put("/api/types/{name}", h.HandlePut)
		r.Delete("/api/types/{name}", h.HandleDelete)
	})
}

// typeView is the /api/types wire shape (K5 freeze). It embeds the stored row
// verbatim (the API shows what the row stores; config stays raw JSON) and adds
// the derived `source` badge. Field drift on either part is caught by
// TestTypesGoldenShape.
type typeView struct {
	store.BlockTypeRow
	// Source is the origin namespace of the effective type: "builtin" for a row
	// in the shipped '_global' namespace, "tenant" for the caller's own
	// tenant-scoped overlay. Derived from scope, NOT the `builtin` column — a
	// server-admin-authored non-builtin '_global' row is still a shipped/global
	// type from a tenant caller's point of view (§5.2(5): the badge tells the UI
	// "global vs. my own", which is exactly the scope split).
	Source string `json:"source"`
}

// typeSourceForScope maps a row scope to the source badge.
func typeSourceForScope(scope string) string {
	if scope == store.GlobalScope {
		return "builtin"
	}
	return "tenant"
}

// HandleList implements GET /api/types — the effective type list for the
// caller: '_global' ∪ the caller's own tenant namespace, resolved so a
// tenant-scoped type SHADOWS the '_global' row of the same name (the Achse-01
// resolver order, store.GetBlockType: ORDER BY (scope='_global') LIMIT 1). In
// tier 1 no tenant rows exist, so the effective list is the '_global' set.
func (h *TypesHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	rows, err := store.ListBlockTypes(ctx, h.pool, typeVisibleScopes(ar))
	if err != nil {
		internalTypesError(w, ctx, "types: list error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"types":   resolveEffectiveTypes(rows),
	})
}

// resolveEffectiveTypes collapses the multi-scope row set to the effective
// per-name view: a non-'_global' (tenant) row shadows the '_global' row of the
// same name (Achse-01 resolver order). Output is name-sorted for a stable wire
// order (and a stable golden). Empty input ⇒ empty slice, never nil (the wire
// always carries a JSON array).
func resolveEffectiveTypes(rows []store.BlockTypeRow) []typeView {
	byName := make(map[string]store.BlockTypeRow, len(rows))
	for _, row := range rows {
		cur, ok := byName[row.Name]
		// Prefer a non-'_global' row over a '_global' one for the same name.
		if !ok || (cur.Scope == store.GlobalScope && row.Scope != store.GlobalScope) {
			byName[row.Name] = row
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]typeView, 0, len(names))
	for _, n := range names {
		row := byName[n]
		out = append(out, typeView{BlockTypeRow: row, Source: typeSourceForScope(row.Scope)})
	}
	return out
}

// HandleGet implements GET /api/types/{name} — a single type incl. its policy
// config + source badge. name resolves against the caller-visible namespace
// set only; an unknown name AND a foreign-tenant type both read as 404 with the
// SAME body (no existence oracle, §5.2(5) / T24 shape).
func (h *TypesHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	name := chi.URLParam(r, "name")
	bt, err := store.GetBlockType(ctx, h.pool, name, typeVisibleScopes(ar))
	if err != nil {
		internalTypesError(w, ctx, "types: get error", err)
		return
	}
	if bt == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": "Type not found",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"type":    typeView{BlockTypeRow: *bt, Source: typeSourceForScope(bt.Scope)},
	})
}

// internalTypesError logs a store failure with the request id and writes the
// generic 500 envelope (no internal detail leaks to the wire).
func internalTypesError(w http.ResponseWriter, ctx context.Context, msg string, err error) {
	slog.Error(msg, "error", err, "request_id", RequestIDFromContext(ctx))
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"success": false, "error": "Internal server error",
	})
}
