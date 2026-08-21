//go:build integration

// MT3-W5 (03-W5) per-tenant settings API probes against a real PG18
// testcontainer — the two-write-worlds resolution (operator → _global,
// tenant-admin → own scope) end to end through the handler→store→overlay path:
//
//   - Gate 1: tenant A's PUT is isolated — B's GET shows B's value, not A's
//   - Gate 2: a member (no tenant-admin role) is 403; a tenant-admin is ADMITTED
//     (red under the old server-admin-only RequireAdmin); operator writes _global
//     only, never a foreign tenant scope
//   - Gate 3: operator PUT of a global-only key (tenant.allow_shared_secrets)
//     writes _global (the §4.5 regression probe: a pauschal TenantOf(ar) would
//     422 a global-only key)
//   - Gate 4: a tenant PUT writes the TENANT row, the _global row stays unchanged
//   - Gate 9: no plaintext secret/env marker in any tenant response
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSettingsTenantAPI -count=1 -v
package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// tenantAPI drives a production settings/secrets router whose injected
// AuthResult is swappable per request (sequential tests, no t.Parallel).
type tenantAPI struct {
	router *chi.Mux
	cfg    *config.Store
	pool   *pgxpool.Pool
	ar     *auth.AuthResult
}

func (s *tenantAPI) as(ar *auth.AuthResult) *tenantAPI { s.ar = ar; return s }

func (s *tenantAPI) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

func (s *tenantAPI) envelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return resp
}

// operatorAR is a server-admin whose HomeScope is a scope with NO own rows, so
// its SnapshotForRequest resolves to the _global base (the operator administers
// _global). writeScope(operator) == _global regardless.
func operatorAR() *auth.AuthResult {
	return &auth.AuthResult{HomeScope: "opscope", ReadScopes: []string{"opscope"}, IsValid: true, IsAdmin: true}
}

// tenantAdmin builds a non-server-admin tenant-admin of its own tenant scope.
func tenantAdmin(scope string) *auth.AuthResult {
	return &auth.AuthResult{
		HomeScope: scope, ReadScopes: []string{scope}, IsValid: true, IsAdmin: false,
		TenantID: "tid-" + scope, TenantRole: auth.RoleAdmin,
	}
}

func tenantMember(scope string) *auth.AuthResult {
	ar := tenantAdmin(scope)
	ar.TenantRole = auth.RoleMember
	return ar
}

func newTenantAPI(t *testing.T, pool *pgxpool.Pool) *tenantAPI {
	t.Helper()
	envCfg, envIssues := config.FromEnv()
	envIssues = append(envIssues, config.Validate(envCfg)...)
	if config.HasErrors(envIssues) {
		t.Fatalf("env fixture invalid: %v", envIssues)
	}
	cfgStore := config.NewStore(envCfg)
	cfgStore.SetOverlay(settings.TenantOverlay(pool))
	config.SetRequestScopeHook(RequestTenantScope)
	t.Cleanup(func() { config.SetRequestScopeHook(nil) })

	api := &tenantAPI{cfg: cfgStore, pool: pool}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, api.ar)))
		})
	})
	MountSettings(router, NewSettingsHandler(pool, cfgStore))
	MountSecrets(router, NewSecretsHandler(pool, cfgStore))
	api.router = router
	return api
}

func freshMasterKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(raw)
}

func scopeRowValue(t *testing.T, pool *pgxpool.Pool, key, scope string) (string, bool) {
	t.Helper()
	var v string
	err := pool.QueryRow(context.Background(),
		`SELECT value::text FROM context_settings WHERE key=$1 AND scope=$2`, key, scope).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func TestSettingsTenantAPI_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv(settings.EnvDisable, "")
	// MARKER VEHICLE (β8): the leak-scan marker used to ride on
	// CTX_CHAT_API_KEY. With the chat tuple gone, server.db_password is the
	// registry's only sensitive key (secret:"presence", env-only), so
	// CONTEXT_DB_PASSWORD is what an env-sourced secret looks like now.
	const envMarker = "ENV-PLAINTEXT-MARKER-aaaaaaaaaaaaaaaaaaaaaaaa"
	t.Setenv("CONTEXT_DB_PASSWORD", envMarker)
	t.Setenv(sealbox.EnvKey, freshMasterKey(t))
	t.Setenv(sealbox.EnvKeyPrev, "")

	api := newTenantAPI(t, pool)
	scan := func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
		t.Helper()
		if strings.Contains(rec.Body.String(), envMarker) {
			t.Errorf("%s leaks the env plaintext marker: %s", label, rec.Body.String())
		}
	}

	// Gate 1: per-tenant isolation — A's write does not become B's value.
	t.Run("Gate1_PerTenantSettingsIsolation", func(t *testing.T) {
		recA := api.as(tenantAdmin("tenanta")).do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":"0.6"}`)
		if recA.Code != http.StatusOK {
			t.Fatalf("A PUT = %d body=%s", recA.Code, recA.Body.String())
		}
		recB := api.as(tenantAdmin("tenantb")).do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":"0.8"}`)
		if recB.Code != http.StatusOK {
			t.Fatalf("B PUT = %d body=%s", recB.Code, recB.Body.String())
		}
		// A's effective GET is A's value, B's is B's — each tenant sees its own.
		gA := api.envelope(t, api.as(tenantAdmin("tenanta")).do(t, http.MethodGet, "/api/settings/rerank.blend_weight", ""))
		sA, _ := gA["setting"].(map[string]any)
		if sA["value"] != 0.6 {
			t.Errorf("A GET value = %v, want 0.6 (red if writeScope is forced to _global)", sA["value"])
		}
		gB := api.envelope(t, api.as(tenantAdmin("tenantb")).do(t, http.MethodGet, "/api/settings/rerank.blend_weight", ""))
		sB, _ := gB["setting"].(map[string]any)
		if sB["value"] != 0.8 {
			t.Errorf("B GET value = %v, want 0.8, NOT A's 0.6", sB["value"])
		}
		// DB rows: distinct per scope, no _global row written.
		if v, ok := scopeRowValue(t, pool, "rerank.blend_weight", "tenanta"); !ok || v != "0.6" {
			t.Errorf("tenanta row = %q ok=%v, want 0.6", v, ok)
		}
		if v, ok := scopeRowValue(t, pool, "rerank.blend_weight", "tenantb"); !ok || v != "0.8" {
			t.Errorf("tenantb row = %q ok=%v, want 0.8", v, ok)
		}
		if _, ok := scopeRowValue(t, pool, "rerank.blend_weight", store.GlobalScope); ok {
			t.Errorf("a tenant write created a _global row — isolation broken")
		}
	})

	// Gate 2: role gate. A member is 403; a tenant-admin is ADMITTED (red under
	// the old server-admin-only gate); an operator never writes a foreign scope.
	t.Run("Gate2_RoleGate", func(t *testing.T) {
		rec := api.as(tenantMember("tenanta")).do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":"0.9"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("member PUT = %d, want 403", rec.Code)
		}
		// Tenant-admin admitted: 200 (red under RequireAdmin = server-admin only).
		rec = api.as(tenantAdmin("tenanta")).do(t, http.MethodPut, "/api/settings/rerank.max_docs", `{"value":"7"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant-admin PUT = %d, want 200 (admitted) body=%s", rec.Code, rec.Body.String())
		}
		// Operator writes _global ONLY — never a foreign tenant scope.
		rec = api.as(operatorAR()).do(t, http.MethodPut, "/api/settings/rerank.max_docs", `{"value":"11"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator PUT = %d, want 200 body=%s", rec.Code, rec.Body.String())
		}
		if v, ok := scopeRowValue(t, pool, "rerank.max_docs", store.GlobalScope); !ok || v != "11" {
			t.Errorf("operator wrote _global row = %q ok=%v, want 11", v, ok)
		}
		if _, ok := scopeRowValue(t, pool, "rerank.max_docs", "opscope"); ok {
			t.Errorf("operator wrote its HomeScope instead of _global — wrong write world")
		}
	})

	// Gate 3: operator PUT of a global-only key writes _global (the §4.5
	// regression probe: a pauschal TenantOf(ar) would 422 a global-only key).
	// Exemplar key updated in U01-W5: the former gaming.active key was retired;
	// tenant.allow_shared_secrets is the current global-only bool.
	t.Run("Gate3_OperatorGlobalOnlyWritesGlobal", func(t *testing.T) {
		rec := api.as(operatorAR()).do(t, http.MethodPut, "/api/settings/tenant.allow_shared_secrets", `{"value":"true"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator tenant.allow_shared_secrets PUT = %d, want 200 (red if operator writeScope is a tenant scope: global-only gate would 422) body=%s",
				rec.Code, rec.Body.String())
		}
		if v, ok := scopeRowValue(t, pool, "tenant.allow_shared_secrets", store.GlobalScope); !ok || v != "true" {
			t.Errorf("tenant.allow_shared_secrets _global row = %q ok=%v, want true", v, ok)
		}
	})

	// Gate 4: tenant PUT writes the tenant row, leaves the _global row UNCHANGED.
	// rerank.blend_weight is a standalone float (no cross-field constraint).
	t.Run("Gate4_TenantWriteLeavesGlobalUnchanged", func(t *testing.T) {
		// Operator seeds a _global base value.
		rec := api.as(operatorAR()).do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":"0.3"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator seed = %d body=%s", rec.Code, rec.Body.String())
		}
		// A fresh tenant overrides it in its own scope.
		rec = api.as(tenantAdmin("tenantc")).do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":"0.55"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant PUT = %d body=%s", rec.Code, rec.Body.String())
		}
		if v, ok := scopeRowValue(t, pool, "rerank.blend_weight", "tenantc"); !ok || v != "0.55" {
			t.Errorf("tenantc row = %q ok=%v, want 0.55", v, ok)
		}
		if v, ok := scopeRowValue(t, pool, "rerank.blend_weight", store.GlobalScope); !ok || v != "0.3" {
			t.Errorf("_global row = %q ok=%v, want 0.3 UNCHANGED (red if tenant write colors _global)", v, ok)
		}
	})

	// Gate 10 (β10, E13): the retirement has no tier. The six *.api_key keys
	// were the tenant-overridable half of the 29 — a tenant admin could hold a
	// private override on them, which is why design/04 §4.6 C singled them out.
	// After the cut they are unknown keys in EVERY world: tenant admin and
	// operator alike get the ordinary 404, and no scope grows a row from the
	// attempt.
	//
	// The member arm is the part that only this file can state: the admin gate
	// is middleware and runs BEFORE the registry lookup, so a non-admin still
	// gets 403 rather than 404 — the retirement did not turn /api/settings into
	// a key-existence oracle for tenant members.
	//
	// Negative probe (2026-08-21): RequireAdminOrTenantAdmin was dropped from
	// MountSettings — the member arm went 404 (the probing hole), while the
	// tenant-admin arm stayed green, i.e. the two arms are independent.
	t.Run("Gate10_RetiredKeysAre404InEveryTier", func(t *testing.T) {
		var tenantOverridable []string
		for _, key := range config.RetiredKeyNames() {
			if strings.HasSuffix(key, ".api_key") {
				tenantOverridable = append(tenantOverridable, key)
			}
		}
		if len(tenantOverridable) != 6 {
			t.Fatalf("retired *.api_key keys = %d, want 6 (the tenant-overridable half of the 29)", len(tenantOverridable))
		}
		for _, key := range tenantOverridable {
			for _, caller := range []struct {
				name string
				ar   *auth.AuthResult
			}{
				{"tenant-admin", tenantAdmin("tenanta")},
				{"operator", operatorAR()},
			} {
				for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
					rec := api.as(caller.ar).do(t, method, "/api/settings/"+key, `{"value":"sneak"}`)
					if rec.Code != http.StatusNotFound {
						t.Errorf("%s %s %s = %d body=%s, want 404 (unknown key, E13)", caller.name, method, key, rec.Code, rec.Body.String())
					}
				}
			}
			if v, ok := scopeRowValue(t, pool, key, "tenanta"); ok {
				t.Errorf("%s grew a tenanta row = %q — a 404 must not persist", key, v)
			}
			if v, ok := scopeRowValue(t, pool, key, store.GlobalScope); ok {
				t.Errorf("%s grew a _global row = %q — a 404 must not persist", key, v)
			}
			// The gate still fires first: a member is refused before the
			// registry is consulted, so the answer cannot distinguish a retired
			// key from a live one.
			rec := api.as(tenantMember("tenanta")).do(t, http.MethodPut, "/api/settings/"+key, `{"value":"sneak"}`)
			if rec.Code != http.StatusForbidden {
				t.Errorf("member PUT %s = %d, want 403 — the admin gate must precede the registry lookup", key, rec.Code)
			}
		}
	})

	// Gate 9: no env plaintext marker in any tenant response surface.
	t.Run("Gate9_NoPlaintextLeak", func(t *testing.T) {
		scan(t, api.as(tenantAdmin("tenanta")).do(t, http.MethodGet, "/api/settings", ""), "tenant GET list")
		scan(t, api.as(operatorAR()).do(t, http.MethodGet, "/api/settings", ""), "operator GET list")
		// Vehicle swap (β8): chat.api_key left the registry. The single-key GET
		// runs on server.db_password so the scan still meets a REAL rendered
		// response (masked env-sourced secret) instead of a 404 body.
		rec := api.as(tenantAdmin("tenanta")).do(t, http.MethodGet, "/api/settings/server.db_password", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant GET server.db_password = %d body=%s — the scan below would prove nothing", rec.Code, rec.Body.String())
		}
		scan(t, rec, "tenant GET server.db_password")
	})
}
