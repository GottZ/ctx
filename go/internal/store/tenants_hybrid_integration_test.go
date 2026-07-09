//go:build integration

// Integration test for Multi-Tenant wave T02 (migration 059 "tenants_hybrid").
//
// T02 introduces the TENANT identity layer for tenant-model C (hybrid, user-
// decided E1): a context_tenants owner-register + a context_tenant_scopes
// partition-map, plus tenant_id + tenant_role columns on context_api_keys.
// scope STAYS the data discriminator — context_blocks/dream_links/blobs/sources
// are NOT given a tenant_id (that would be model A). 059 therefore creates:
//
//	context_tenants        — owner register, default tenant seeded with the
//	                         fixed UUID 00000000-0000-0000-0000-0000000d3fa0
//	context_tenant_scopes  — scope→tenant_id map; (private,work,shared) wired to
//	                         the default tenant, FK ON DELETE CASCADE
//	context_api_keys.tenant_id   — FK→context_tenants, backfilled to default
//	context_api_keys.tenant_role — owner|admin|member CHECK, DEFAULT 'member'
//	                               (RBAC L1 bootstrap, E2; permission checks
//	                               arrive later via auth.Can() / T20)
//
// RED (today, WITH migrations up to 058 but WITHOUT 059): every sub-case fails
// with a DISTINCT, DB-level Postgres error that pins exactly what 059 has not
// yet built — NOT a compile error:
//   - tenant register      → 42P01 undefined_table "context_tenants"
//   - scope partition map  → 42P01 undefined_table "context_tenant_scopes"
//   - tenant_id backfill    → 42703 undefined_column context_api_keys.tenant_id
//   - tenant_role column   → 42703 undefined_column context_api_keys.tenant_role
//     and (once GREEN) 23514 check_violation for an out-of-domain role.
// GREEN (after 059): all sub-cases pass.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTenantsHybrid -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// defaultTenantID is the fixed identity the 059 backfill pins to slug 'default'
// (E9). The hex tail spells "d3fa0" ≈ "default". The slug is cosmetic; this UUID
// carries the identity and is the join target for every legacy scope and key.
const defaultTenantID = "00000000-0000-0000-0000-0000000d3fa0"

// pgCode extracts the 5-char SQLSTATE from a pgx error, or "" if the error is
// not a Postgres server error. Used to assert the EXACT discriminating failure
// (table/column/CHECK) rather than matching on fragile message text.
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// seedAPIKey inserts one minimal, valid context_api_keys row and returns its id.
// 014 dropped the plaintext api_key column and promoted key_hash to the sole
// NOT NULL/UNIQUE identifier, so the post-014 minimal column set is exactly three
// NOT NULL / no-default columns besides the generated PK: key_hash (UNIQUE),
// label, home_scope. Inserting this row COMPILES and SUCCEEDS today — it is the
// "existing key" whose post-059 backfill (tenant_id → default, tenant_role →
// 'member') the GREEN assertions check. keyHash doubles as the lookup handle.
func seedAPIKey(t *testing.T, pool *pgxpool.Pool, keyHash string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	if err := pool.QueryRow(ctx,
		`WITH p AS (
		     INSERT INTO context_principals (display_name) VALUES ('test-fixture') RETURNING id
		 )
		 INSERT INTO context_api_keys (key_hash, label, home_scope, principal_id)
		 SELECT $1, 't02-key-label', 'private', p.id FROM p
		 RETURNING id`, keyHash).Scan(&id); err != nil {
		t.Fatalf("seed api_key key_hash=%q: %v", keyHash, err)
	}
	return id
}

func TestTenantsHybrid_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// context_tenants must exist and the default tenant must be seeded with the
	// fixed UUID, slug 'default', status 'active'. RED: 42P01 undefined_table.
	t.Run("default_tenant_present", func(t *testing.T) {
		var (
			id, slug, status string
		)
		err := pool.QueryRow(ctx,
			`SELECT id::text, slug, status
			   FROM context_tenants
			  WHERE slug = 'default'`).Scan(&id, &slug, &status)
		if err != nil {
			t.Fatalf("SELECT default tenant failed (expected RED without 059 with 42P01 undefined_table, GREEN with): code=%q err=%v",
				pgCode(err), err)
		}
		if id != defaultTenantID {
			t.Fatalf("default tenant id = %q, want fixed %q", id, defaultTenantID)
		}
		if status != "active" {
			t.Fatalf("default tenant status = %q, want 'active'", status)
		}
	})

	// context_tenant_scopes must wire EXACTLY the three legacy scopes
	// (private,work,shared) to the default tenant. _global is deliberately NOT
	// mapped (system scope: a missing row means "no tenant"). RED: 42P01.
	t.Run("legacy_scopes_mapped_to_default", func(t *testing.T) {
		// All three rows point at the default UUID, and there are exactly three.
		var mappedToDefault int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_scopes
			  WHERE tenant_id = $1::uuid
			    AND scope IN ('private','work','shared')`, defaultTenantID,
		).Scan(&mappedToDefault); err != nil {
			t.Fatalf("count default scopes failed (expected RED without 059 with 42P01 undefined_table, GREEN with): code=%q err=%v",
				pgCode(err), err)
		}
		if mappedToDefault != 3 {
			t.Fatalf("scopes mapped to default tenant = %d, want 3 (private,work,shared)", mappedToDefault)
		}

		// No other scopes (notably no '_global') are mapped — the partition map
		// holds exactly the three legacy scopes and nothing else.
		var total int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_scopes`).Scan(&total); err != nil {
			t.Fatalf("count all tenant_scopes failed: code=%q err=%v", pgCode(err), err)
		}
		if total != 3 {
			t.Fatalf("total context_tenant_scopes rows = %d, want 3 (no _global, no strays)", total)
		}

		// '_global' must NOT have a row (fail-closed: absence == no tenant).
		var globalRows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_scopes WHERE scope = '_global'`).Scan(&globalRows); err != nil {
			t.Fatalf("count _global mapping failed: code=%q err=%v", pgCode(err), err)
		}
		if globalRows != 0 {
			t.Fatalf("_global is mapped (%d rows), want 0 — system scope must stay unowned", globalRows)
		}
	})

	// Backfill: an api_key that existed before 059 must end up with a non-NULL
	// tenant_id (= default) and NO key may be left NULL. We seed the "existing"
	// key BEFORE reading the column. RED: 42703 undefined_column tenant_id.
	t.Run("api_keys_tenant_id_backfilled", func(t *testing.T) {
		seedAPIKey(t, pool, "t02-backfill-key")

		// System-wide invariant: zero NULL tenant_id rows after 059's UPDATE.
		var nullCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_api_keys WHERE tenant_id IS NULL`).Scan(&nullCount); err != nil {
			t.Fatalf("count NULL tenant_id failed (expected RED without 059 with 42703 undefined_column, GREEN with): code=%q err=%v",
				pgCode(err), err)
		}
		if nullCount != 0 {
			t.Fatalf("context_api_keys with NULL tenant_id = %d, want 0 (all backfilled to default)", nullCount)
		}

		// And the backfilled value is specifically the default tenant UUID.
		var tid string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_id::text FROM context_api_keys WHERE key_hash = 't02-backfill-key'`).Scan(&tid); err != nil {
			t.Fatalf("read backfilled tenant_id failed: code=%q err=%v", pgCode(err), err)
		}
		if tid != defaultTenantID {
			t.Fatalf("backfilled tenant_id = %q, want default %q", tid, defaultTenantID)
		}
	})

	// tenant_role: column exists, DEFAULTs to 'member' for existing keys (no
	// auto-promote — fail-closed, unlike the is_admin bootstrap), and the CHECK
	// rejects an out-of-domain role. RED: 42703 undefined_column tenant_role.
	t.Run("tenant_role_default_member", func(t *testing.T) {
		seedAPIKey(t, pool, "t02-role-default-key")

		// Every existing key carries the DEFAULT 'member' (fail-closed bootstrap).
		var nonMember int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_api_keys WHERE tenant_role <> 'member'`).Scan(&nonMember); err != nil {
			t.Fatalf("count non-member roles failed (expected RED without 059 with 42703 undefined_column, GREEN with): code=%q err=%v",
				pgCode(err), err)
		}
		if nonMember != 0 {
			t.Fatalf("api_keys with tenant_role != 'member' = %d, want 0 (no auto-promote)", nonMember)
		}

		// The freshly seeded key reads back exactly 'member' via the DEFAULT.
		var role string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_role FROM context_api_keys WHERE key_hash = 't02-role-default-key'`).Scan(&role); err != nil {
			t.Fatalf("read tenant_role failed: code=%q err=%v", pgCode(err), err)
		}
		if role != "member" {
			t.Fatalf("seeded key tenant_role = %q, want DEFAULT 'member'", role)
		}
	})

	// The owner|admin|member CHECK must reject anything else. This sub-case only
	// becomes reachable GREEN (RED dies earlier on the missing column), but it is
	// the load-bearing RBAC-domain guard, so it is asserted explicitly.
	t.Run("tenant_role_check_rejects_out_of_domain", func(t *testing.T) {
		// INSERT a brand-new key with an illegal role → 23514 check_violation.
		_, err := pool.Exec(ctx,
			`WITH p AS (
			     INSERT INTO context_principals (display_name) VALUES ('test-fixture') RETURNING id
			 )
			 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_role, principal_id)
			 SELECT 't02-bad-role-key', 't02-bad', 'private', 'superuser', p.id FROM p`)
		if err == nil {
			t.Fatal("INSERT tenant_role='superuser' succeeded, want 23514 check_violation")
		}
		if code := pgCode(err); code != "23514" {
			t.Fatalf("INSERT illegal tenant_role: got code=%q (%v), want 23514 check_violation", code, err)
		}

		// UPDATE an existing valid key to an illegal role → 23514 as well.
		seedAPIKey(t, pool, "t02-update-bad-role-key")
		_, err = pool.Exec(ctx,
			`UPDATE context_api_keys SET tenant_role = 'root'
			  WHERE key_hash = 't02-update-bad-role-key'`)
		if err == nil {
			t.Fatal("UPDATE tenant_role='root' succeeded, want 23514 check_violation")
		}
		if code := pgCode(err); code != "23514" {
			t.Fatalf("UPDATE illegal tenant_role: got code=%q (%v), want 23514 check_violation", code, err)
		}

		// Sanity: each legal role is accepted (the CHECK is not over-broad).
		for _, ok := range []string{"owner", "admin", "member"} {
			if _, err := pool.Exec(ctx,
				`WITH p AS (
				     INSERT INTO context_principals (display_name) VALUES ('test-fixture') RETURNING id
				 )
				 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_role, principal_id)
				 SELECT 't02-ok-role-'||$1, 't02-ok', 'private', $1, p.id FROM p`, ok); err != nil {
				t.Fatalf("INSERT legal tenant_role=%q rejected: code=%q err=%v", ok, pgCode(err), err)
			}
		}
	})

	// Tenant lifecycle (User 2026-06-15): a naked tenant DELETE must be BLOCKED
	// while keys point at it — full-prune is an explicit, ordered super-admin
	// action, never an implicit cascade/orphan. ON DELETE RESTRICT on
	// context_api_keys.tenant_id. Sub-tx ROLLED BACK (no destructive escape).
	// RED without 059: 42P01 (no context_tenants table).
	t.Run("tenant_delete_restricted_by_keys", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// Seed a key pointing at the default tenant (a fresh testcontainer has no
		// Bestands-keys — 059's backfill only touches pre-existing rows; the
		// column DEFAULT would also set it, but be explicit).
		if _, err := tx.Exec(ctx,
			`WITH p AS (
			     INSERT INTO context_principals (display_name) VALUES ('test-fixture') RETURNING id
			 )
			 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
			 SELECT 't02-restrict-probe', 't02-restrict', 'private', $1::uuid, p.id FROM p`, defaultTenantID); err != nil {
			t.Fatalf("seed key pointing at default tenant: code=%q err=%v", pgCode(err), err)
		}

		// A naked DELETE of the now key-bearing tenant must fail with
		// foreign_key_violation (23503) — RESTRICT enforces the ordered
		// super-admin prune path (handle keys/data first, then tenant).
		_, err = tx.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, defaultTenantID)
		if got := pgCode(err); got != "23001" {
			t.Fatalf("naked DELETE of key-bearing tenant: code=%q err=%v, want 23001 restrict_violation (ON DELETE RESTRICT fires immediately; NO ACTION would be 23503)", got, err)
		}
	})

	// context_tenant_scopes.tenant_id KEEPS ON DELETE CASCADE: a key-less tenant
	// can be deleted and its scope mappings cascade away (the partition map is a
	// narrow table, no data). A fresh probe tenant (no keys) avoids the api_keys
	// RESTRICT above. RED without 059: 42P01 (no context_tenants table).
	t.Run("scope_cascade_on_keyless_tenant_delete", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		var probeID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ('t02-cascade-probe','probe') RETURNING id::text`).Scan(&probeID); err != nil {
			t.Fatalf("insert key-less probe tenant (RED 42P01 without 059): code=%q err=%v", pgCode(err), err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('t02-cascade-scope', $1::uuid)`, probeID); err != nil {
			t.Fatalf("map scope to probe tenant: code=%q err=%v", pgCode(err), err)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, probeID); err != nil {
			t.Fatalf("DELETE key-less probe tenant failed (want CASCADE, not RESTRICT — no keys point here): code=%q err=%v", pgCode(err), err)
		}

		var after int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_scopes WHERE scope = 't02-cascade-scope'`).Scan(&after); err != nil {
			t.Fatalf("count scope after cascade: %v", err)
		}
		if after != 0 {
			t.Fatalf("scope mapping after key-less tenant DELETE = %d, want 0 (ON DELETE CASCADE)", after)
		}
	})
}
