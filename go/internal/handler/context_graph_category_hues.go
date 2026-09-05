// /api/graph/category-hues — the U02-W5 per-category graph HUE override API
// (design 02a §A3/§A4-W5).
//
// GET    /api/graph/category-hues            resolved sparse override map for the
//                                            caller's effective view (MEMBER tier)
// PUT    /api/graph/category-hues/{category}  set an override {"hue":210}  (admin)
// DELETE /api/graph/category-hues/{category}  remove the override (revert to seed)
//
// TIER-SPLIT (02a §A4-W5, security lens): the GET is MEMBER-tier — the graph is a
// member surface (server.go: /api/graph/ego has no tier gate), and an admin-gated
// GET would make members render the seed instead of their tenant's colours,
// hiding AM-2 from the majority. Isolation comes from readScopes (the effective
// {_global, tenant} view), NOT from the tier. ONLY PUT/DELETE are
// RequireAdminOrTenantAdmin, and they write writeScope (operator → _global,
// tenant-admin → own scope), NEVER a body/URL scope (02a §A5-MT).

package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"unicode"

	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphCategoryHuesHandler implements the /api/graph/category-hues routes.
type GraphCategoryHuesHandler struct {
	pool *pgxpool.Pool
}

// NewGraphCategoryHuesHandler creates a new GraphCategoryHuesHandler.
func NewGraphCategoryHuesHandler(pool *pgxpool.Pool) *GraphCategoryHuesHandler {
	return &GraphCategoryHuesHandler{pool: pool}
}

// MountGraphCategoryHues mounts the routes: the GET stays MEMBER-tier (registered
// on the passed router, which is already behind Auth in server.go), while
// PUT/DELETE live in a RequireAdminOrTenantAdmin sub-group. ONE function used by
// server.go and the gate tests, so the 403/200 probes exercise exactly the chain
// production mounts.
func MountGraphCategoryHues(r chi.Router, h *GraphCategoryHuesHandler) {
	r.Get("/api/graph/category-hues", h.HandleList) // member-tier (02a §A4-W5)
	r.Group(func(r chi.Router) {
		r.Use(RequireAdminOrTenantAdmin)
		r.Put("/api/graph/category-hues/{category}", h.HandlePut)
		r.Delete("/api/graph/category-hues/{category}", h.HandleDelete)
	})
}

// HandleList implements GET /api/graph/category-hues. The resolved sparse map is
// scoped to readScopes(ar) = {_global, tenant} (tenant beats _global per
// category); a member sees its own tenant's overrides over the global layer. NOTE
// (deviation, see report): readScopes(ar) — the helper (tenant_scope.go:35) —
// yields the _global-first / tenant-last ordering LoadCategoryHues needs for
// Tenant>_global precedence, whereas authResult.ReadScopes is HomeScope-first
// (would INVERT precedence) and pulls in AllowedScopes/grants irrelevant to the
// AM-2 chain. The member TIER (auth-only route) is preserved either way.
func (h *GraphCategoryHuesHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	hues, err := store.LoadCategoryHues(r.Context(), h.pool, readScopes(ar))
	if err != nil {
		h.internalError(w, r, "graph hues: load failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "hues": hues})
}

// HandlePut implements PUT /api/graph/category-hues/{category}. The target scope
// is writeScope(ar) — NEVER a body/URL scope. The hue must be an INTEGER 0..359
// (a non-integer or out-of-range value is 422, never a silent clamp).
func (h *GraphCategoryHuesHandler) HandlePut(w http.ResponseWriter, r *http.Request) {
	category := chi.URLParam(r, "category")
	if code, msg := validCategoryName(category); msg != "" {
		writeJSON(w, code, map[string]any{"success": false, "error": msg})
		return
	}

	var body struct {
		Hue *json.Number `json:"hue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid JSON body"})
		return
	}
	if body.Hue == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": "hue is required"})
		return
	}
	n, err := body.Hue.Int64() // fails on a non-integer number (e.g. 210.5)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": "hue must be an integer 0..359"})
		return
	}
	if n < 0 || n > 359 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": fmt.Sprintf("hue must be between 0 and 359 (got %d)", n)})
		return
	}

	scope := writeScope(AuthResultFromContext(r.Context()))
	if err := h.upsert(r, scope, category, int16(n)); err != nil {
		h.internalError(w, r, "graph hues: persist failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"category": category,
		"hue":      n,
		"scope":    scope,
	})
}

// HandleDelete implements DELETE /api/graph/category-hues/{category} — remove the
// override in the caller's writeScope, reverting the category to its seed. An
// absent override is a 404 (the settings-DELETE shape).
func (h *GraphCategoryHuesHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	category := chi.URLParam(r, "category")
	if code, msg := validCategoryName(category); msg != "" {
		writeJSON(w, code, map[string]any{"success": false, "error": msg})
		return
	}
	scope := writeScope(AuthResultFromContext(r.Context()))
	found, err := h.delete(r, scope, category)
	if err != nil {
		h.internalError(w, r, "graph hues: delete failed", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": fmt.Sprintf("no hue override set for category %q", category)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "category": category, "deleted": true, "scope": scope})
}

// validCategoryName mirrors the 093 DB CHECK (length 1..128, no control chars) so
// a bad path segment yields a clean 4xx instead of a raw constraint 500. Returns
// (status, message); empty message = pass.
func validCategoryName(category string) (int, string) {
	if category == "" {
		return http.StatusBadRequest, "category is required"
	}
	if len(category) > 128 {
		return http.StatusUnprocessableEntity, "category must be at most 128 characters"
	}
	for _, ru := range category {
		if unicode.IsControl(ru) {
			return http.StatusUnprocessableEntity, "category must not contain control characters"
		}
	}
	return 0, ""
}

// upsert persists one override in an attributed transaction (the 093 audit
// trigger fires atomically with the row).
func (h *GraphCategoryHuesHandler) upsert(r *http.Request, scope, category string, hue int16) error {
	ctx := r.Context()
	_, err := attributedTx(ctx, h.pool, func(tx pgx.Tx) (struct{}, error) {
		return struct{}{}, store.UpsertCategoryHue(ctx, tx, scope, category, hue, actorID(r))
	})
	return err
}

// delete removes one override in an attributed transaction.
func (h *GraphCategoryHuesHandler) delete(r *http.Request, scope, category string) (bool, error) {
	ctx := r.Context()
	return attributedTx(ctx, h.pool, func(tx pgx.Tx) (bool, error) {
		return store.DeleteCategoryHue(ctx, tx, scope, category, actorID(r))
	})
}

func (h *GraphCategoryHuesHandler) internalError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	slog.Error(msg, "error", err, "request_id", RequestIDFromContext(r.Context()))
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
}
