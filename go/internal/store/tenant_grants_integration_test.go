//go:build integration

// Integration test for Multi-Tenant wave T14 (migration 061 "tenant_grants").
//
// T14 (Achse 02-V1, the genuine schema contribution of the visibility axis)
// adds context_tenant_grants — the cross-tenant READ channel: one tenant grants
// another tenant read access to one of its OWN scopes (the "friend-tenant" case,
// design/02-scope-visibility.md §3.1/§3.3). It is an ADDITIVE table with NO
// consumer yet — the read_scopes resolver body in migration 060 (a LATER wave)
// will UNION it into ctx_auth; 061 only lays the table. No backfill (empty
// initially: cross-tenant sharing is opt-in, least-privilege).
//
// Model C (E1): grantee_tenant is a real FK on context_tenants (UUID), and
// granted_scope is a FK on context_tenant_scopes(scope). That second FK is the
// fail-closed-by-construction guard: a grant can only target a registered,
// tenant-owned scope. System scopes ('_global', '_'-prefixed) are NEVER in
// context_tenant_scopes (059 maps only private/work/shared; '_'-prefix is
// double-enforced on key creation), so a grant on them is rejected by the FK
// automatically — no extra CHECK needed. uq_tenant_grant (grantee_tenant,
// granted_scope) makes a double-grant idempotent. ON DELETE CASCADE on both FKs:
// tenant gone → grants gone; scope removed at offboarding → grant gone.
// created_by is ON DELETE SET NULL (key delete anonymizes history, never
// deletes the grant).
//
// RED (today, WITH migrations up to 059 but WITHOUT 061): every sub-case fails
// with a DB-level Postgres error, NOT a compile error. The table does not exist
// yet, so the first statement against it yields:
//   - 42P01 undefined_table "context_tenant_grants"
// (context_tenants + context_tenant_scopes DO exist from 059, so the failure
// pins specifically the missing grants table.)
// GREEN (after 061): all sub-cases pass.
//
// Note: pgCode, defaultTenantID and seedAPIKey are declared in
// tenants_hybrid_integration_test.go (same store_test package, T02) and reused
// here — they are NOT redeclared.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTenantGrants -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantGrants_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// The table must exist and accept a valid grant: the default tenant
	// (fixed UUID, registered by 059) gains read access to a registered,
	// tenant-owned scope ('shared' is mapped to the default tenant by 059).
	// (Self-grant is semantically a no-op the resolver ignores, but it is a
	// STRUCTURALLY valid row — both FKs satisfied — so it exercises the happy
	// path without needing a second tenant.) RED: 42P01 undefined_table.
	t.Run("valid_grant_inserts", func(t *testing.T) {
		var id string
		err := pool.QueryRow(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope)
			 VALUES ($1::uuid, 'shared')
			 RETURNING id::text`, defaultTenantID).Scan(&id)
		if err != nil {
			t.Fatalf("INSERT valid grant failed (expected RED without 061 with 42P01 undefined_table \"context_tenant_grants\", GREEN with): code=%q err=%v",
				pgCode(err), err)
		}
		if id == "" {
			t.Fatalf("valid grant returned empty id, want a uuidv7 PK")
		}
	})

	// grantee_tenant FK → context_tenants: a random, non-registered tenant UUID
	// must be rejected with 23503 foreign_key_violation. A grant can only be
	// HELD by a registered tenant. RED: 42P01 (table missing) — the FK guard is
	// only reachable GREEN, but it is the load-bearing grantee integrity check.
	t.Run("grantee_tenant_fk_rejects_unregistered", func(t *testing.T) {
		// A syntactically valid but unregistered tenant UUID.
		const ghostTenant = "11111111-2222-3333-4444-555566667777"
		_, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope)
			 VALUES ($1::uuid, 'shared')`, ghostTenant)
		if err == nil {
			t.Fatal("INSERT grant with unregistered grantee_tenant succeeded, want 23503 foreign_key_violation")
		}
		if code := pgCode(err); code != "23503" {
			t.Fatalf("grantee_tenant FK on unregistered tenant: got code=%q (%v), want 23503 foreign_key_violation (RED without 061: 42P01 undefined_table)", code, err)
		}
	})

	// granted_scope FK → context_tenant_scopes(scope): the fail-closed-by-
	// construction guard. '_global' is the reserved system namespace and is
	// NEVER mapped in context_tenant_scopes (059 maps only private/work/shared),
	// so a grant targeting it must be rejected by the FK with 23503 — WITHOUT any
	// dedicated _-prefix CHECK on this table. RED: 42P01.
	t.Run("granted_scope_fk_rejects_global_system_scope", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope)
			 VALUES ($1::uuid, '_global')`, defaultTenantID)
		if err == nil {
			t.Fatal("INSERT grant on '_global' system scope succeeded, want 23503 foreign_key_violation (fail-closed-by-construction)")
		}
		if code := pgCode(err); code != "23503" {
			t.Fatalf("granted_scope FK on '_global': got code=%q (%v), want 23503 foreign_key_violation (system scope is not in context_tenant_scopes; RED without 061: 42P01)", code, err)
		}
	})

	// granted_scope FK also rejects a freely-invented, never-registered scope
	// name — the FK does not special-case '_global'; ANY unmapped scope fails.
	// RED: 42P01.
	t.Run("granted_scope_fk_rejects_unregistered_scope", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope)
			 VALUES ($1::uuid, 'tenant:never-registered-q1')`, defaultTenantID)
		if err == nil {
			t.Fatal("INSERT grant on unregistered scope succeeded, want 23503 foreign_key_violation")
		}
		if code := pgCode(err); code != "23503" {
			t.Fatalf("granted_scope FK on unregistered scope: got code=%q (%v), want 23503 foreign_key_violation (RED without 061: 42P01)", code, err)
		}
	})

	// uq_tenant_grant (grantee_tenant, granted_scope): a double grant of the same
	// (tenant, scope) pair is rejected with 23505 unique_violation — the grant is
	// idempotent at the schema level (re-granting is a no-op, never a duplicate
	// row). Done in a rolled-back sub-tx so the first INSERT does not pollute the
	// table for later sub-cases. RED: 42P01 on the first INSERT.
	t.Run("duplicate_grant_rejected_by_uq", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// First grant: succeeds (default tenant → 'work', a registered scope).
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope)
			 VALUES ($1::uuid, 'work')`, defaultTenantID); err != nil {
			t.Fatalf("first grant (default→work) failed (RED without 061: 42P01 undefined_table): code=%q err=%v", pgCode(err), err)
		}

		// Identical second grant: rejected by uq_tenant_grant with 23505.
		_, err = tx.Exec(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope)
			 VALUES ($1::uuid, 'work')`, defaultTenantID)
		if err == nil {
			t.Fatal("duplicate grant (same grantee_tenant + granted_scope) succeeded, want 23505 unique_violation")
		}
		if code := pgCode(err); code != "23505" {
			t.Fatalf("duplicate grant: got code=%q (%v), want 23505 unique_violation (uq_tenant_grant)", code, err)
		}
	})

	// ON DELETE CASCADE on granted_scope: when a scope is removed from
	// context_tenant_scopes (a tenant offboarding cascade in real life), every
	// grant targeting that scope vanishes automatically. Modelled in a
	// rolled-back sub-tx: register a fresh probe tenant + its own scope, grant on
	// it, then DELETE the scope mapping and assert the grant is gone (count 0).
	// Non-destructive (rolled back). RED: 42P01 on the grant INSERT (the tenant +
	// scope INSERTs already work via 059).
	t.Run("grant_cascades_on_scope_removal", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// A fresh probe tenant owning a fresh probe scope (059 tables, work today).
		var probeID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name)
			 VALUES ('t14-grant-cascade-probe','probe') RETURNING id::text`).Scan(&probeID); err != nil {
			t.Fatalf("insert probe tenant: code=%q err=%v", pgCode(err), err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id)
			 VALUES ('t14-cascade-scope', $1::uuid)`, probeID); err != nil {
			t.Fatalf("map probe scope to probe tenant: code=%q err=%v", pgCode(err), err)
		}

		// Grant the default tenant read access to the probe scope. RED dies here
		// (42P01) — the grants table does not exist yet.
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope)
			 VALUES ($1::uuid, 't14-cascade-scope')`, defaultTenantID); err != nil {
			t.Fatalf("insert grant on probe scope failed (RED without 061: 42P01 undefined_table \"context_tenant_grants\"): code=%q err=%v", pgCode(err), err)
		}

		// Remove the scope mapping → the granted_scope FK CASCADE must delete the
		// grant. (Deleting from context_tenant_scopes is allowed here because the
		// probe scope owns no blocks; in production the offboarding prune handles
		// data first.)
		if _, err := tx.Exec(ctx,
			`DELETE FROM context_tenant_scopes WHERE scope = 't14-cascade-scope'`); err != nil {
			t.Fatalf("delete probe scope mapping: code=%q err=%v", pgCode(err), err)
		}

		var remaining int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_grants WHERE granted_scope = 't14-cascade-scope'`).Scan(&remaining); err != nil {
			t.Fatalf("count grants after scope removal: code=%q err=%v", pgCode(err), err)
		}
		if remaining != 0 {
			t.Fatalf("grants on removed scope = %d, want 0 (ON DELETE CASCADE on granted_scope FK)", remaining)
		}
	})

	// grantee_tenant ON DELETE CASCADE: deleting the tenant that HOLDS a grant
	// removes the grant. Fresh key-less probe tenant (the default tenant is
	// RESTRICT-blocked by its api_keys, 059) so the tenant DELETE itself succeeds.
	t.Run("grant_cascades_on_grantee_tenant_removal", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		var granteeID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ('t14-grantee-probe','probe') RETURNING id::text`).Scan(&granteeID); err != nil {
			t.Fatalf("insert key-less grantee probe tenant: code=%q err=%v", pgCode(err), err)
		}
		// Grant the probe tenant read access to the default tenant's 'shared'
		// scope. RED dies here (42P01) without 061.
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope)
			 VALUES ($1::uuid, 'shared')`, granteeID); err != nil {
			t.Fatalf("insert grant for probe grantee (RED without 061: 42P01): code=%q err=%v", pgCode(err), err)
		}
		// Delete the key-less grantee tenant → grantee_tenant FK CASCADE clears it.
		if _, err := tx.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, granteeID); err != nil {
			t.Fatalf("delete key-less grantee tenant failed: code=%q err=%v", pgCode(err), err)
		}
		var remaining int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_grants WHERE grantee_tenant = $1::uuid`, granteeID).Scan(&remaining); err != nil {
			t.Fatalf("count grants after grantee removal: code=%q err=%v", pgCode(err), err)
		}
		if remaining != 0 {
			t.Fatalf("grants held by removed tenant = %d, want 0 (ON DELETE CASCADE on grantee_tenant FK)", remaining)
		}
	})
}
