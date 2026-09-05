package handler

// T03-7: the 500 envelope of the REST handlers that used to own a private
// internal*Error helper. Five helpers collapsed onto two — writeInternal (the
// silent writer) and internalError (its logging wrapper) — and with them the
// two 500 prose variants collapsed onto one. This test is the guard for BOTH
// properties: every one of these answers must carry the identical status, the
// identical envelope and the identical text. It goes red if a handler grows a
// private 500 body again, or if the category-hues/settings prose drifts back
// to the pre-wave "internal error" (design/03 §5.5, the wave's one deliberate
// behaviour change).
//
// The failure is forced without a database: the pool points at a dead port, so
// the FIRST store call of each handler fails and the handler takes its 500
// branch.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
)

// internalErrorBody is the ONE 500 body of these surfaces — writeInternal's
// map, rendered by writeJSON (keys sorted by encoding/json).
const internalErrorBody = `{"error":"Internal server error","success":false}`

// deadPool is a pool whose every acquire fails (connection refused on a
// reserved port, bounded by connect_timeout so a DROPping firewall cannot hang
// the test).
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), "postgres://nobody:nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("dead pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// adminReq builds an authenticated request; param (when non-empty) is offered
// under both URL param names these routes use ({category} / {key}).
func adminReq(method, target, body, param string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	ctx := context.WithValue(r.Context(), authResultKey,
		&auth.AuthResult{IsValid: true, IsAdmin: true, HomeScope: "_global"})
	if param != "" {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("category", param)
		rctx.URLParams.Add("key", param)
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return r.WithContext(ctx)
}

func TestInternalErrorEnvelopeIsOne(t *testing.T) {
	pool := deadPool(t)
	hues := NewGraphCategoryHuesHandler(pool)
	settings := &SettingsHandler{pool: pool}
	types := NewTypesHandler(pool, nil)

	cases := []struct {
		name string
		call func(w http.ResponseWriter)
	}{
		{"GET /api/graph/category-hues", func(w http.ResponseWriter) {
			hues.HandleList(w, adminReq(http.MethodGet, "/api/graph/category-hues", "", ""))
		}},
		{"PUT /api/graph/category-hues/{category}", func(w http.ResponseWriter) {
			hues.HandlePut(w, adminReq(http.MethodPut, "/api/graph/category-hues/infrastructure", `{"hue":210}`, "infrastructure"))
		}},
		{"DELETE /api/graph/category-hues/{category}", func(w http.ResponseWriter) {
			hues.HandleDelete(w, adminReq(http.MethodDelete, "/api/graph/category-hues/infrastructure", "", "infrastructure"))
		}},
		{"GET /api/settings", func(w http.ResponseWriter) {
			settings.HandleList(w, adminReq(http.MethodGet, "/api/settings", "", ""))
		}},
		{"GET /api/settings/{key}", func(w http.ResponseWriter) {
			settings.HandleGet(w, adminReq(http.MethodGet, "/api/settings/tenant.devmode", "", "tenant.devmode"))
		}},
		{"GET /api/types", func(w http.ResponseWriter) {
			types.HandleList(w, adminReq(http.MethodGet, "/api/types", "", ""))
		}},
	}

	for _, c := range cases {
		rr := httptest.NewRecorder()
		c.call(rr)
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500", c.name, rr.Code)
		}
		if got := strings.TrimRight(rr.Body.String(), "\n"); got != internalErrorBody {
			t.Errorf("%s: body = %s, want %s", c.name, got, internalErrorBody)
		}
	}
}
