//go:build integration

// Workflow W2 integration gates (design/03 §4.2/§9.5(c), §7-W2) against a real
// PG18 testcontainer: the /api/types write surface end-to-end through the
// production MountTypes chain (RequireAdminOrTenantAdmin → handler → store).
// Covers the tier matrix (tenant-admin owns its namespace, '_global' is
// operator-only), the reference guard (409 + count) and the builtin guard.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestTypesWrite -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
)

// typesWriteReq drives the PRODUCTION MountTypes chain (reads + writes) with a
// real pool and ar injected — the same function server.go mounts, so the
// RequireAdminOrTenantAdmin gate and the {name} URLParam resolve exactly as in
// production. body "" ⇒ no request body.
func typesWriteReq(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	MountTypes(r, NewTypesHandler(pool, nil))
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestTypesWrite(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	actor, _, err := store.CreateApiKey(ctx, pool, "types-w2-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}

	// A tenant-admin of tenant-a (owner/admin role administers its own tenant)
	// and a server-admin (operator). The type namespace is the tenant id, the
	// same key typeVisibleScopes uses (072: scope is a free VARCHAR).
	tenantAdminA := &auth.AuthResult{
		IsValid: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{"private"}, TenantID: "tenant-a", TenantRole: auth.RoleAdmin,
	}
	serverAdmin := &auth.AuthResult{
		IsValid: true, IsAdmin: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{store.GlobalScope}, TenantID: "_server", TenantRole: auth.RoleMember,
	}

	// ── Tier matrix: '_global' is operator-only ──────────────────────────
	t.Run("tenant_admin_put_global_403", func(t *testing.T) {
		rec := typesWriteReq(t, pool, tenantAdminA, http.MethodPut, "/api/types/knowledge",
			`{"display_name":"Hijack"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("tenant-admin PUT _global builtin: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("tenant_admin_delete_global_403", func(t *testing.T) {
		rec := typesWriteReq(t, pool, tenantAdminA, http.MethodDelete, "/api/types/knowledge", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("tenant-admin DELETE _global builtin: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
		}
		// Constructed probe: the builtin row still exists (403 blocked, no delete).
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM context_block_types WHERE name='knowledge' AND scope=$1)`,
			store.GlobalScope).Scan(&exists); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !exists {
			t.Fatal("builtin knowledge was deleted despite 403 — write gate leaked")
		}
	})

	// ── Tenant-admin owns its own namespace ──────────────────────────────
	t.Run("tenant_admin_creates_own_type", func(t *testing.T) {
		rec := typesWriteReq(t, pool, tenantAdminA, http.MethodPut, "/api/types/sprint",
			`{"display_name":"Sprint","description":"a time-boxed iteration"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant-admin PUT new type: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Success bool `json:"success"`
			Type    struct {
				Name, Scope, Source, DisplayName string
			} `json:"type"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
		}
		if !resp.Success || resp.Type.Scope != "tenant-a" || resp.Type.Source != "tenant" {
			t.Fatalf("created type = scope %q source %q (success %v), want tenant-a/tenant", resp.Type.Scope, resp.Type.Source, resp.Success)
		}
		// The row is a tenant-a-scoped, non-builtin row (pinned by role, NOT by
		// any body field).
		bt, err := store.GetBlockType(ctx, pool, "sprint", []string{"tenant-a"})
		if err != nil || bt == nil {
			t.Fatalf("get created type: %v (bt=%v)", err, bt)
		}
		if bt.Scope != "tenant-a" || bt.Builtin {
			t.Fatalf("created row = scope %q builtin %v, want tenant-a/false", bt.Scope, bt.Builtin)
		}
	})

	t.Run("tenant_admin_updates_own_type", func(t *testing.T) {
		rec := typesWriteReq(t, pool, tenantAdminA, http.MethodPut, "/api/types/sprint",
			`{"description":"updated description"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant-admin PUT update own: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		bt, err := store.GetBlockType(ctx, pool, "sprint", []string{"tenant-a"})
		if err != nil || bt == nil {
			t.Fatalf("get updated type: %v", err)
		}
		if bt.Description != "updated description" {
			t.Fatalf("description = %q, want %q", bt.Description, "updated description")
		}
	})

	// ── Reference guard: 409 + count (design §5.1(c) R1, W2 gate) ─────────
	t.Run("delete_referenced_type_409_count", func(t *testing.T) {
		// A tenant type + one block referencing it (active).
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: "widget", Scope: "tenant-a", DisplayName: "Widget", Config: json.RawMessage(`{"v":1}`),
		}, &actor.ID, ""); err != nil {
			t.Fatalf("seed widget type: %v", err)
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", "uses-widget", "body",
			nil, nil, "tenant-a", true, store.SensitivityWrite{}, "widget"); err != nil {
			t.Fatalf("seed referencing block: %v", err)
		}
		// Constructed probe: the reference genuinely exists (this is what makes
		// the guard's 409 real, not a no-op) — without the store ref-check the
		// DELETE would 200 and orphan this block.
		var refs int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks WHERE type_name='widget'`).Scan(&refs); err != nil {
			t.Fatalf("ref probe: %v", err)
		}
		if refs != 1 {
			t.Fatalf("probe precondition: %d blocks reference widget, want 1", refs)
		}

		rec := typesWriteReq(t, pool, tenantAdminA, http.MethodDelete, "/api/types/widget", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("DELETE referenced type: status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Success bool `json:"success"`
			Blocks  struct {
				Active, Archived int
			} `json:"blocks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode 409: %v (body %s)", err, rec.Body.String())
		}
		if resp.Success || resp.Blocks.Active != 1 {
			t.Fatalf("409 body = success %v active %d, want false/1 (body %s)", resp.Success, resp.Blocks.Active, rec.Body.String())
		}
		// The type survives a refused delete.
		bt, _ := store.GetBlockType(ctx, pool, "widget", []string{"tenant-a"})
		if bt == nil {
			t.Fatal("widget type deleted despite 409 — ref guard leaked")
		}
	})

	// ── Builtin guard: even the operator cannot delete a builtin ──────────
	t.Run("server_admin_delete_builtin_409", func(t *testing.T) {
		rec := typesWriteReq(t, pool, serverAdmin, http.MethodDelete, "/api/types/knowledge", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("server-admin DELETE builtin: status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "builtin") {
			t.Fatalf("409 body = %q, want a builtin-guard message", rec.Body.String())
		}
	})

	// ── Operator writes '_global'; tenant-admin deletes its own type ──────
	t.Run("server_admin_creates_global_type", func(t *testing.T) {
		rec := typesWriteReq(t, pool, serverAdmin, http.MethodPut, "/api/types/incident",
			`{"display_name":"Incident"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("server-admin PUT _global: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		bt, err := store.GetBlockType(ctx, pool, "incident", []string{store.GlobalScope})
		if err != nil || bt == nil || bt.Scope != store.GlobalScope {
			t.Fatalf("global create: bt=%v err=%v", bt, err)
		}
	})

	t.Run("tenant_admin_deletes_own_unreferenced_type", func(t *testing.T) {
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: "ephemeral", Scope: "tenant-a", DisplayName: "Ephemeral", Config: json.RawMessage(`{"v":1}`),
		}, &actor.ID, ""); err != nil {
			t.Fatalf("seed ephemeral: %v", err)
		}
		rec := typesWriteReq(t, pool, tenantAdminA, http.MethodDelete, "/api/types/ephemeral", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("delete own unreferenced: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		bt, _ := store.GetBlockType(ctx, pool, "ephemeral", []string{"tenant-a"})
		if bt != nil {
			t.Fatal("ephemeral still present after 200 delete")
		}
	})
}
