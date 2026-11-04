//go:build integration

// F-W8 G4, DB half (design/04 §4.6, §7.2 no. 1): PUT gateway.enabled=true on
// a router whose federation ingress is not registered is a 409 and leaves no
// override row (row probe); the same write through a router with a
// registered ingress is a 200 with the row. Both routers live in ONE process
// and share the database and the config store — the registration is per
// router, so the second router's mark never opens the first (the "parallel
// routers" gate of the briefing). DELETE of a false override that would fall
// back to CTX_GATEWAY_ENABLED=true is refused the same way, and the row
// survives.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestGatewayEnableRequiresIngress' -v ./internal/handler/
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGatewayEnableRequiresIngress_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv(settings.EnvDisable, "")
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")

	envCfg, envIssues := config.FromEnv()
	envIssues = append(envIssues, config.Validate(envCfg)...)
	if config.HasErrors(envIssues) {
		t.Fatalf("env fixture invalid: %v", envIssues)
	}
	cfgStore := config.NewStore(envCfg)
	actor, _, err := store.CreateApiKey(ctx, pool, "f-w8-ingress-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("actor key: %v", err)
	}
	admin := &auth.AuthResult{ApiKeyID: actor.ID, HomeScope: "private", ReadScopes: []string{"private"}, IsValid: true, IsAdmin: true}

	newRouter := func(in *fedhop.Ingress, ar *auth.AuthResult) *chi.Mux {
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
			})
		})
		MountSettings(r, NewSettingsHandler(pool, cfgStore).WithIngress(in))
		return r
	}
	do := func(r http.Handler, method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/settings/gateway.enabled", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	row := func() (string, bool) { return scopeRowValue(t, pool, "gateway.enabled", store.GlobalScope) }

	plain := &fedhop.Ingress{}
	registered := &fedhop.Ingress{}
	registered.MarkLoopGuard()
	routerWithout, routerWith := newRouter(plain, admin), newRouter(registered, admin)

	if rec := do(routerWithout, http.MethodPut, `{"value":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("PUT true without ingress = %d %s, want 409", rec.Code, rec.Body.Bytes())
	}
	if v, ok := row(); ok {
		t.Fatalf("409 left an override row %q", v)
	}
	if cfgStore.Snapshot().Gateway.Enabled {
		t.Fatal("snapshot says enabled after a refused write")
	}

	if rec := do(routerWith, http.MethodPut, `{"value":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT true with a registered ingress = %d %s, want 200", rec.Code, rec.Body.Bytes())
	}
	if v, ok := row(); !ok || v != "true" {
		t.Fatalf("override row after 200 = %q (%v), want true", v, ok)
	}
	if !cfgStore.Snapshot().Gateway.Enabled {
		t.Fatal("snapshot not enabled after the accepted write")
	}

	// The other router's mark does not leak: the first router still refuses.
	if plain.Registered() {
		t.Fatal("marking one router's ingress marked another's")
	}
	if rec := do(routerWithout, http.MethodPut, `{"value":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("PUT true on the unregistered router after the other one registered = %d, want 409", rec.Code)
	}

	// Turning it off is always allowed.
	if rec := do(routerWithout, http.MethodPut, `{"value":false}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT false without ingress = %d %s, want 200", rec.Code, rec.Body.Bytes())
	}
	if v, ok := row(); !ok || v != "false" {
		t.Fatalf("override row after PUT false = %q (%v), want false", v, ok)
	}

	// The env says on; the false override is what keeps the API off. Deleting
	// it would turn the API on — refused, the row stays.
	t.Setenv("CTX_GATEWAY_ENABLED", "true")
	if rec := do(routerWithout, http.MethodDelete, ""); rec.Code != http.StatusConflict {
		t.Fatalf("DELETE falling back to env true without ingress = %d %s, want 409", rec.Code, rec.Body.Bytes())
	}
	if v, ok := row(); !ok || v != "false" {
		t.Fatalf("override row after the refused DELETE = %q (%v), want false", v, ok)
	}
	if rec := do(routerWith, http.MethodDelete, ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE with a registered ingress = %d %s, want 200", rec.Code, rec.Body.Bytes())
	}
	if v, ok := row(); ok {
		t.Fatalf("override row after the accepted DELETE = %q, want none", v)
	}

	// Open question 2 of the briefing (no code): a tenant-admin's write is
	// refused by the tenancy of the key already — gateway.enabled is
	// global-only, the tenant row would be dropped, so the PUT is a 422 even
	// with a registered ingress, and no tenant row exists.
	t.Setenv("CTX_GATEWAY_ENABLED", "")
	tenant := newRouter(registered, tenantAdmin("f-w8-tenant"))
	if rec := do(tenant, http.MethodPut, `{"value":true}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant-admin PUT true = %d %s, want 422 (global-only)", rec.Code, rec.Body.Bytes())
	}
	if v, ok := scopeRowValue(t, pool, "gateway.enabled", "f-w8-tenant"); ok {
		t.Fatalf("tenant row written: %q", v)
	}
}
