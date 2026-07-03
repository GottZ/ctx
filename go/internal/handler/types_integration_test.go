//go:build integration

// Workflow W1 integration gates (design/03 §5.2(5), §7-W1) against a real PG18
// testcontainer: the read-only /api/types surface end-to-end through the
// production MountTypes chain (RequireMember → handler → store), the tenant
// isolation of the effective list, and the 404-no-oracle on a foreign / unknown
// type. The tenant-isolation probe is CONSTRUCTED: an unfiltered SELECT is run
// alongside the handler call to prove the row exists and ONLY the scope filter
// hides it (red = what a naive handler leaks; green = the filtered handler).
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestTypesRead -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
)

// typesIntegReq drives the PRODUCTION MountTypes chain with a real pool and ar
// injected — the same function server.go mounts, so URLParam {name} resolves
// and the member gate runs exactly as in production.
func typesIntegReq(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	MountTypes(r, NewTypesHandler(pool, nil))
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func listedTypeNames(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/types: status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Types   []struct {
			Name   string `json:"name"`
			Scope  string `json:"scope"`
			Source string `json:"source"`
		} `json:"types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, rec.Body.String())
	}
	if !resp.Success {
		t.Fatalf("list success=false: %s", rec.Body.String())
	}
	out := map[string]string{}
	for _, x := range resp.Types {
		out[x.Name] = x.Source
	}
	return out
}

func TestTypesReadIsolation(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	actor, _, err := store.CreateApiKey(ctx, pool, "types-w1-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}

	// Two tenants each get one custom (non-_global) type. scope is a free
	// VARCHAR (072: no FK), so direct store inserts model the tier-2 overlay
	// the handler must isolate — no tenant-row wave needed to prove the filter.
	mk := func(name, scope string) {
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: name, Scope: scope, DisplayName: name, Config: json.RawMessage(`{"v":1}`),
		}, &actor.ID, ""); err != nil {
			t.Fatalf("seed type %s@%s: %v", name, scope, err)
		}
	}
	mk("team-a-secret", "tenant-a")
	mk("team-b-secret", "tenant-b")

	tenantB := &auth.AuthResult{
		IsValid: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{"private"}, TenantID: "tenant-b", TenantRole: auth.RoleMember,
	}
	tenantA := &auth.AuthResult{
		IsValid: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{"private"}, TenantID: "tenant-a", TenantRole: auth.RoleMember,
	}

	// CONSTRUCTED probe: the unfiltered SELECT (what a naive handler would run)
	// DOES return team-a-secret — proving the row exists and only the scope
	// filter hides it from tenant B. This is the "red with an ungefiltertes
	// SELECT" the W1 gate demands, run inline against the same DB.
	t.Run("unfiltered_select_would_leak", func(t *testing.T) {
		var leaked bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM context_block_types WHERE name = 'team-a-secret')`).Scan(&leaked)
		if err != nil {
			t.Fatalf("unfiltered probe query: %v", err)
		}
		if !leaked {
			t.Fatal("probe precondition broken: team-a-secret not in the table at all")
		}
	})

	t.Run("tenant_b_does_not_see_tenant_a_type", func(t *testing.T) {
		names := listedTypeNames(t, typesIntegReq(t, pool, tenantB, http.MethodGet, "/api/types"))
		if _, leaked := names["team-a-secret"]; leaked {
			t.Errorf("tenant B saw tenant A's custom type (scope filter broken): %v", names)
		}
		if src, ok := names["team-b-secret"]; !ok || src != "tenant" {
			t.Errorf("tenant B missing own type team-b-secret (src=%q, ok=%v): %v", src, ok, names)
		}
		// The shipped _global builtins are always visible, badged builtin.
		for _, want := range []string{"knowledge", "reference", "audit-trail", "system-meta"} {
			if src, ok := names[want]; !ok || src != "builtin" {
				t.Errorf("builtin %q missing/mis-badged (src=%q, ok=%v)", want, src, ok)
			}
		}
	})

	t.Run("get_foreign_type_404_no_oracle", func(t *testing.T) {
		rec := typesIntegReq(t, pool, tenantB, http.MethodGet, "/api/types/team-a-secret")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("foreign type get: status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
		}
		// Unknown type reads with the SAME body — no existence oracle.
		unknown := typesIntegReq(t, pool, tenantB, http.MethodGet, "/api/types/does-not-exist")
		if unknown.Code != http.StatusNotFound {
			t.Fatalf("unknown type get: status = %d, want 404", unknown.Code)
		}
		if rec.Body.String() != unknown.Body.String() {
			t.Errorf("404 oracle: foreign body %q != unknown body %q", rec.Body.String(), unknown.Body.String())
		}
	})

	t.Run("owner_sees_own_type_with_source_tenant", func(t *testing.T) {
		rec := typesIntegReq(t, pool, tenantA, http.MethodGet, "/api/types/team-a-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("owner get own type: status = %d (body %s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Type struct {
				Name   string `json:"name"`
				Scope  string `json:"scope"`
				Source string `json:"source"`
			} `json:"type"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode get: %v", err)
		}
		if resp.Type.Scope != "tenant-a" || resp.Type.Source != "tenant" {
			t.Errorf("own type = scope %q source %q, want tenant-a/tenant", resp.Type.Scope, resp.Type.Source)
		}
	})

	t.Run("get_builtin_source_builtin", func(t *testing.T) {
		rec := typesIntegReq(t, pool, tenantB, http.MethodGet, "/api/types/knowledge")
		if rec.Code != http.StatusOK {
			t.Fatalf("get builtin: status = %d (body %s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Type struct {
				Source string `json:"source"`
			} `json:"type"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Type.Source != "builtin" {
			t.Errorf("knowledge source = %q, want builtin", resp.Type.Source)
		}
	})
}
