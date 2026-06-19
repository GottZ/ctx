//go:build integration

// Integration test for MT wave T06 (Achse 01-T6): tenant-bound api-key creation
// with a tenant-AWARE {shared} default (design/01 §3.6 / E3 / Finding 5 =
// R-LEAK5).
//
// Today CreateApiKey has no tenant parameter: every key it mints lands on the
// default tenant (the DB DEFAULT on context_api_keys.tenant_id, 059) and a nil
// allowed_scopes inherits the column DEFAULT '{shared}' (api_keys.go:69).
// Harmless with one tenant — but the moment a FOREIGN tenant's key is minted,
// that '{shared}' default is a cross-tenant read BY DEFAULT into the default
// tenant's 21 shared blocks. T06 makes the {shared} default tenant-aware: only
// the default tenant inherits shared automatically; any other tenant's key with
// no explicit allowed_scopes gets an empty set.
//
// TENANT-DECISION(shared-scope-owner): shared stays a default-tenant scope
// (Weg a) — Alt: system-wide scope like _global (Weg b), reversible by rehanging
// one context_tenant_scopes row. The read_scopes consequence is proved
// end-to-end through ctx_auth (the masterplan T06 §5.3 negative probe).
//
// RED (today): the package fails to COMPILE — CreateApiKey takes 5 args; the
// tenant-aware signature appends tenantID (the honest red for a signature-change
// wave, analogous to the new-symbol red of T05a). GREEN: compiles; the
// foreign-tenant key carries {} not {shared}, and ctx_auth resolves its
// read_scopes WITHOUT shared. The verify-lens mutation that drops the tenant
// check turns the foreign-tenant case red ({shared} leaks back in).
//
// tgTenant / tgMapScope live in the same store_test package
// (tenant_grants_crud_integration_test.go) and are reused.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestCreateApiKey_Tenant -count=1 -v
package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestCreateApiKey_TenantAwareSharedDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tenantB := tgTenant(t, pool, "t06-tenant-b")
	tgMapScope(t, pool, "t06-b", tenantB)

	keyTenant := func(t *testing.T, id string) string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_id::text FROM context_api_keys WHERE id = $1::uuid`, id).Scan(&got); err != nil {
			t.Fatalf("read tenant_id(%s): %v", id, err)
		}
		return got
	}

	// 1. THE security fix: a foreign tenant's key with nil allowed_scopes must
	//    NOT inherit '{shared}' — it gets an empty set. (Mutation: drop the
	//    tenant check → '{shared}' leaks → this case goes red.)
	t.Run("foreign_tenant_no_shared_inherit", func(t *testing.T) {
		key, _, err := store.CreateApiKey(ctx, pool, "t06-foreign", "t06-b", nil, tenantB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if len(key.AllowedScopes) != 0 {
			t.Errorf("R-LEAK5: foreign-tenant key inherited %v, want empty (no implicit shared)", key.AllowedScopes)
		}
		if got := keyTenant(t, key.ID); got != tenantB {
			t.Errorf("tenant_id = %q, want creator tenant %q", got, tenantB)
		}
	})

	// 2. End-to-end (§5.3 negative probe): the foreign-tenant key's resolved
	//    read_scopes must NOT contain 'shared' (only its own home scope).
	t.Run("foreign_tenant_read_scopes_exclude_shared", func(t *testing.T) {
		_, plaintext, err := store.CreateApiKey(ctx, pool, "t06-foreign-e2e", "t06-b", nil, tenantB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		var readScopes []string
		if err := pool.QueryRow(ctx,
			`SELECT read_scopes FROM ctx_auth($1)`, plaintext).Scan(&readScopes); err != nil {
			t.Fatalf("ctx_auth: %v", err)
		}
		if slices.Contains(readScopes, "shared") {
			t.Errorf("R-LEAK5: foreign-tenant read_scopes %v leaked the default tenant's shared scope", readScopes)
		}
		if !slices.Contains(readScopes, "t06-b") {
			t.Errorf("read_scopes %v missing own home scope t06-b", readScopes)
		}
	})

	// 3. Pausability invariant: the default tenant's behaviour is byte-identical
	//    to today — nil allowed_scopes still inherits '{shared}'.
	t.Run("default_tenant_still_inherits_shared", func(t *testing.T) {
		key, _, err := store.CreateApiKey(ctx, pool, "t06-default", "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !slices.Equal(key.AllowedScopes, []string{"shared"}) {
			t.Errorf("default-tenant key allowed_scopes = %v, want {shared} (byte-identical to today)", key.AllowedScopes)
		}
		if got := keyTenant(t, key.ID); got != store.DefaultTenantID {
			t.Errorf("tenant_id = %q, want default %q", got, store.DefaultTenantID)
		}
	})

	// 4. An empty tenantID falls back to the default tenant (single-tenant /
	//    059-backfill semantics) — and thus still inherits shared. This keeps
	//    every "tenant doesn't matter" call site verbatim-equivalent to today.
	t.Run("empty_tenant_falls_back_to_default", func(t *testing.T) {
		key, _, err := store.CreateApiKey(ctx, pool, "t06-empty-tenant", "private", nil, "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := keyTenant(t, key.ID); got != store.DefaultTenantID {
			t.Errorf("empty tenantID → tenant_id %q, want default %q", got, store.DefaultTenantID)
		}
		if !slices.Equal(key.AllowedScopes, []string{"shared"}) {
			t.Errorf("empty-tenant (=default) key allowed_scopes = %v, want {shared}", key.AllowedScopes)
		}
	})

	// 5. Explicit allowed_scopes are untouched for ANY tenant — the tenant-aware
	//    default only governs the nil case.
	t.Run("explicit_allowed_scopes_untouched", func(t *testing.T) {
		key, _, err := store.CreateApiKey(ctx, pool, "t06-explicit", "t06-b", []string{"t06-b"}, tenantB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !slices.Equal(key.AllowedScopes, []string{"t06-b"}) {
			t.Errorf("explicit allowed_scopes mangled: got %v, want {t06-b}", key.AllowedScopes)
		}
	})
}
