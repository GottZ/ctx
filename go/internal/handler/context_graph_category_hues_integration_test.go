//go:build integration

// Integration probes for Web-UX U02-W5 (the /api/graph/category-hues API)
// against a real PG18 testcontainer, driving a production router whose injected
// AuthResult is swappable per request (sequential, no t.Parallel). Each sub-probe
// maps to a §A4-W5 gate:
//
//   - member-key GET is 200 + the resolved map (RED against a naive admin-gated
//     GET, which would 403 a member — proven by mounting the GET both ways)
//   - member PUT/DELETE are 403 (RequireAdminOrTenantAdmin), tenant-admin admits
//   - PUT ignores a body/URL scope and writes mutationScope (RED against a naive
//     body-scope handler: the row lands in the tenant's own scope, never _global)
//   - hue 360 / non-integer ⇒ 422; round-trip PUT → GET → DELETE → GET(empty)
//   - GET precedence per category through the HTTP surface
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/handler/ \
//	  -run 'TestCategoryHuesAPI' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/testdb"
)

// huesAPI drives a production category-hues router with a swappable AuthResult.
type huesAPI struct {
	router *chi.Mux
	pool   *pgxpool.Pool
	ar     *auth.AuthResult
}

func (s *huesAPI) as(ar *auth.AuthResult) *huesAPI { s.ar = ar; return s }

func (s *huesAPI) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

func (s *huesAPI) envelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return resp
}

// newHuesAPI mounts the PRODUCTION MountGraphCategoryHues (GET member-tier,
// PUT/DELETE behind RequireAdminOrTenantAdmin) behind an AuthResult-injecting
// middleware — the exact chain server.go wires inside the Auth group.
func newHuesAPI(t *testing.T, pool *pgxpool.Pool) *huesAPI {
	t.Helper()
	api := &huesAPI{pool: pool}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, api.ar)))
		})
	})
	MountGraphCategoryHues(router, NewGraphCategoryHuesHandler(pool))
	api.router = router
	return api
}

func huesScopeRow(t *testing.T, pool *pgxpool.Pool, scope, category string) (int16, bool) {
	t.Helper()
	var hue int16
	err := pool.QueryRow(context.Background(),
		`SELECT hue FROM context_graph_category_hues WHERE scope=$1 AND category=$2`, scope, category).Scan(&hue)
	if err != nil {
		return 0, false
	}
	return hue, true
}

func TestCategoryHuesAPI_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	api := newHuesAPI(t, pool)

	member := tenantMember("tenant-a")
	admin := tenantAdmin("tenant-a")
	operator := operatorAR()

	// ── Tier gate: member GET is 200 (member-tier). RED against an admin gate. ──
	rec := api.as(member).do(t, http.MethodGet, "/api/graph/category-hues", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("member GET = %d, want 200 (member-tier); body=%s", rec.Code, rec.Body.String())
	}
	// empty override table ⇒ empty map object (not null).
	if got := api.envelope(t, rec)["hues"]; got == nil {
		t.Errorf("member GET hues = nil, want {} object")
	}

	// ── Tier gate: member PUT/DELETE are 403. ──
	rec = api.as(member).do(t, http.MethodPut, "/api/graph/category-hues/decisions", `{"hue":210}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("member PUT = %d, want 403", rec.Code)
	}
	rec = api.as(member).do(t, http.MethodDelete, "/api/graph/category-hues/decisions", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("member DELETE = %d, want 403", rec.Code)
	}

	// ── mutationScope: tenant-admin PUT with a body scope field is IGNORED — the row
	//    lands in the tenant's OWN scope, NEVER _global. RED against a body-scope
	//    handler (which would honour the "_global" in the body). ──
	rec = api.as(admin).do(t, http.MethodPut, "/api/graph/category-hues/decisions",
		`{"hue":210,"scope":"_global"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant-admin PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := huesScopeRow(t, pool, "_global", "decisions"); ok {
		t.Error("body scope was honoured — row wrongly written to _global")
	}
	if hue, ok := huesScopeRow(t, pool, "tenant-a", "decisions"); !ok || hue != 210 {
		t.Errorf("tenant scope row = (%d,%v), want (210,true)", hue, ok)
	}
	if got := api.envelope(t, rec)["scope"]; got != "tenant-a" {
		t.Errorf("PUT response scope = %v, want tenant-a", got)
	}

	// ── Hue validation: 360 and a non-integer ⇒ 422. ──
	for _, bad := range []string{`{"hue":360}`, `{"hue":-1}`, `{"hue":210.5}`, `{}`} {
		rec = api.as(admin).do(t, http.MethodPut, "/api/graph/category-hues/x", bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s = %d, want 422", bad, rec.Code)
		}
	}

	// ── GET precedence per category: operator sets a _global override, tenant its
	//    own; the tenant view resolves both, operator view sees only _global. ──
	rec = api.as(operator).do(t, http.MethodPut, "/api/graph/category-hues/infra", `{"hue":100}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if hue, ok := huesScopeRow(t, pool, "_global", "infra"); !ok || hue != 100 {
		t.Errorf("operator wrote (%d,%v)@_global, want (100,true)", hue, ok)
	}

	rec = api.as(admin).do(t, http.MethodGet, "/api/graph/category-hues", "")
	huesM, _ := api.envelope(t, rec)["hues"].(map[string]any)
	if huesM["infra"] != float64(100) {
		t.Errorf("tenant view infra = %v, want 100 (global-only category must surface)", huesM["infra"])
	}
	if huesM["decisions"] != float64(210) {
		t.Errorf("tenant view decisions = %v, want 210 (tenant override)", huesM["decisions"])
	}

	rec = api.as(operator).do(t, http.MethodGet, "/api/graph/category-hues", "")
	opM, _ := api.envelope(t, rec)["hues"].(map[string]any)
	if _, leaked := opM["decisions"]; leaked {
		t.Errorf("operator view leaked the tenant's 'decisions' override: %v", opM)
	}
	if opM["infra"] != float64(100) {
		t.Errorf("operator view infra = %v, want 100", opM["infra"])
	}

	// ── Round-trip DELETE → GET(reverted). ──
	rec = api.as(admin).do(t, http.MethodDelete, "/api/graph/category-hues/decisions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant-admin DELETE = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Second delete ⇒ 404 (override already gone).
	rec = api.as(admin).do(t, http.MethodDelete, "/api/graph/category-hues/decisions", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("repeat DELETE = %d, want 404", rec.Code)
	}
	rec = api.as(admin).do(t, http.MethodGet, "/api/graph/category-hues", "")
	huesM, _ = api.envelope(t, rec)["hues"].(map[string]any)
	if _, still := huesM["decisions"]; still {
		t.Errorf("after DELETE, tenant view still carries decisions: %v", huesM)
	}
}
