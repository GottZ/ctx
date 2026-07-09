//go:build integration

// Integration tests for Multi-Tenant wave T03/T15 (migration 060
// "ctx_auth_tenant") against a real PG18 testcontainer.
//
// T03/T15 extends ctx_auth from 6 to 8 return columns (+tenant_id UUID,
// +tenant_role TEXT — the 2 new columns AT THE END, additive) under tenant
// model C (E1), rebuilds the read_scopes body POSITIONALLY (design/02 §4.1
// amendment) and adds a tenant-status gate (design/01 §5.2). It also touches
// internal/auth/auth.go (AuthResult +TenantID/+TenantRole, scan +2 cols) — the
// Go-layer plumbing is exercised by ctx_auth_tenant_authenticate (added GREEN).
//
// Model C: the data tables get NO tenant_id; scope stays the discriminator.
// ctx_auth joins context_api_keys.tenant_id → context_tenants for the status
// gate and unions context_tenant_grants for cross-tenant read scopes.
//
// RED (today, WITH migrations 058/059/061 but WITHOUT 060): ctx_auth is still
// the 052 6-column version, so every probe that selects tenant_id / tenant_role
// fails with a DB-level Postgres error (42703 undefined_column), NOT a compile
// error. GREEN (after 060): the 8-col function exists and all probes pass.
//
// LATE-BINDING PROOF: migration 060 CREATEs ctx_auth with a body referencing
// context_tenant_grants, a table migration 061 (a LATER version) creates. The
// runner applies versions ASC (058,059,060,061), so at 060-create-time the
// grants table does not exist yet. plpgsql late-binds table names to CALL time,
// so CREATE FUNCTION succeeds. If it did NOT, testdb.SetupTestDB would fail at
// migration 060 and EVERY test here would error at setup — so a green run is
// the empirical proof. The grant-branch subtest additionally proves the body
// resolves the table at call time.
//
// pgCode + defaultTenantID are declared here (package auth_test); the store_test
// copies live in a different package and are not visible.
//
// Run with:
//
//	go test -tags=integration ./internal/auth/ -run TestCtxAuthTenant -count=1 -v
package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// defaultTenantID is the fixed identity 059 pins to slug 'default' (E9); the hex
// tail spells "d3fa0" ≈ "default". It is the join target for every legacy scope
// and key, and the tenant_id a freshly minted key inherits via the 059 DEFAULT.
const defaultTenantID = "00000000-0000-0000-0000-0000000d3fa0"

// pgCode extracts the 5-char SQLSTATE from a pgx error, or "" if the error is
// not a Postgres server error. RED probes assert 42703 (undefined_column).
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestCtxAuthTenant_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// (1) The 8-column signature: a default-tenant key resolves to the fixed
	// default UUID + tenant_role 'member' (059 DEFAULT/backfill, no auto-promote),
	// is_valid true. RED: 42703 undefined_column "tenant_id" (052 has 6 cols).
	t.Run("signature_default_key_carries_tenant", func(t *testing.T) {
		_, plaintext, err := store.CreateApiKey(ctx, pool, "t03-default", "private", nil, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		var (
			homeScope, tenantRole string
			tenantID              *string
			isValid               bool
		)
		err = pool.QueryRow(ctx,
			`SELECT home_scope, is_valid, tenant_id::text, tenant_role FROM ctx_auth($1)`,
			plaintext,
		).Scan(&homeScope, &isValid, &tenantID, &tenantRole)
		if err != nil {
			t.Fatalf("ctx_auth 8-col select failed (RED without 060: 42703 undefined_column tenant_id; GREEN with): code=%q err=%v", pgCode(err), err)
		}
		if !isValid {
			t.Fatal("default key is_valid=false, want true")
		}
		if tenantID == nil || *tenantID != defaultTenantID {
			t.Fatalf("tenant_id = %v, want %q", tenantID, defaultTenantID)
		}
		if tenantRole != "member" {
			t.Fatalf("tenant_role = %q, want 'member' (059 default, no auto-promote)", tenantRole)
		}
		if homeScope != "private" {
			t.Fatalf("home_scope = %q, want 'private'", homeScope)
		}
	})

	// (1b) tenant_role is carried faithfully from the column (not hardcoded
	// 'member'): a key promoted to 'admin' round-trips as 'admin'. Pins that 060
	// RETURNs tenant_role from context_api_keys, not a literal. The tenant stays
	// the (active) default, so the status gate passes.
	t.Run("admin_role_round_trips", func(t *testing.T) {
		key, plaintext, err := store.CreateApiKey(ctx, pool, "t03-admin-role", "private", nil, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET tenant_role = 'admin' WHERE id = $1::uuid`, key.ID); err != nil {
			t.Fatalf("promote to admin: %v", err)
		}
		var (
			tenantRole string
			isValid    bool
		)
		if err := pool.QueryRow(ctx,
			`SELECT is_valid, tenant_role FROM ctx_auth($1)`, plaintext).Scan(&isValid, &tenantRole); err != nil {
			t.Fatalf("ctx_auth: %v", err)
		}
		if !isValid {
			t.Fatal("admin key is_valid=false, want true (default tenant is active)")
		}
		if tenantRole != "admin" {
			t.Fatalf("tenant_role = %q, want 'admin' (carried from column, not hardcoded)", tenantRole)
		}
	})

	// (2) read_scopes is POSITIONAL: a 'work'-home key with allowed={shared}
	// resolves to EXACTLY {work, shared} in that order — read_scopes[0]==home.
	// This is the array_agg(DISTINCT) mutation pin: DISTINCT sorts alphabetically
	// → {shared, work} → read_scopes[0]='shared' ≠ home → red. 'work' (not
	// 'private') is the load-bearing probe — 'private' sorts first and would MASK
	// the regression. RED: 42703.
	t.Run("work_key_read_scopes_positional", func(t *testing.T) {
		_, plaintext, err := store.CreateApiKey(ctx, pool, "t03-work", "work", []string{"shared"}, "")
		if err != nil {
			t.Fatalf("create work key: %v", err)
		}
		var (
			readScopes []string
			tenantID   *string
		)
		err = pool.QueryRow(ctx,
			`SELECT read_scopes, tenant_id::text FROM ctx_auth($1)`, plaintext,
		).Scan(&readScopes, &tenantID)
		if err != nil {
			t.Fatalf("ctx_auth read_scopes select failed (RED without 060: 42703): code=%q err=%v", pgCode(err), err)
		}
		if len(readScopes) != 2 || readScopes[0] != "work" || readScopes[1] != "shared" {
			t.Fatalf("read_scopes = %v, want [work shared] (positional: home first, NO alphabetical sort)", readScopes)
		}
		if tenantID == nil || *tenantID != defaultTenantID {
			t.Fatalf("tenant_id = %v, want default", tenantID)
		}
	})

	// (3) Empty (non-nil) allowed_scopes → read_scopes is home-only {work}.
	// []string{} must NOT trip the DB DEFAULT '{shared}' (api_keys.go:67 COALESCE
	// keeps a non-nil empty array). RED: 42703.
	t.Run("empty_allowed_read_scopes_home_only", func(t *testing.T) {
		_, plaintext, err := store.CreateApiKey(ctx, pool, "t03-empty", "work", []string{}, "")
		if err != nil {
			t.Fatalf("create empty-allowed key: %v", err)
		}
		var (
			readScopes []string
			tenantID   *string
		)
		err = pool.QueryRow(ctx,
			`SELECT read_scopes, tenant_id::text FROM ctx_auth($1)`, plaintext,
		).Scan(&readScopes, &tenantID)
		if err != nil {
			t.Fatalf("ctx_auth select failed (RED without 060: 42703): code=%q err=%v", pgCode(err), err)
		}
		if len(readScopes) != 1 || readScopes[0] != "work" {
			t.Fatalf("read_scopes = %v, want [work] (empty allowed, no shared default)", readScopes)
		}
		if tenantID == nil {
			t.Fatal("tenant_id NULL for a valid key, want default")
		}
	})

	// (3b) Order-preserving DEDUP when home_scope is ALSO in allowed_scopes:
	// home=work, allowed={shared,work} → read_scopes==[work,shared] (the redundant
	// 'work' is dropped, NOT appended again; 'shared' is not pushed to the end).
	// Pins the `NOT (v_s = ANY(...))` dedup — subtest (2) uses non-overlapping
	// allowed so it catches the sort mutation but NOT a dedup-removal mutation
	// (which would yield [work,shared,work]).
	t.Run("home_in_allowed_deduped", func(t *testing.T) {
		_, plaintext, err := store.CreateApiKey(ctx, pool, "t03-dedup", "work", []string{"shared", "work"}, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		var readScopes []string
		if err := pool.QueryRow(ctx,
			`SELECT read_scopes FROM ctx_auth($1)`, plaintext).Scan(&readScopes); err != nil {
			t.Fatalf("ctx_auth: %v", err)
		}
		if len(readScopes) != 2 || readScopes[0] != "work" || readScopes[1] != "shared" {
			t.Fatalf("read_scopes = %v, want [work shared] (home deduped, order preserved)", readScopes)
		}
	})

	// (4) Grant branch + LATE-BINDING proof: a cross-tenant read grant on the
	// key's tenant adds that scope to read_scopes, AFTER home, deduped. Uses a
	// fully isolated pair of fresh tenants (NOT the default tenant) so it cannot
	// contaminate the default-tenant probes. The grant body references
	// context_tenant_grants (created by 061, AFTER 060) — a green result proves
	// plpgsql late-bound the table to call time. RED: 42703.
	t.Run("grant_branch_adds_scope_and_proves_late_binding", func(t *testing.T) {
		// Tenant A owns scope 't03a-scope'; tenant B owns 't03b-scope'.
		var tenantA, tenantB string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ('t03-grant-a','grant A') RETURNING id::text`).Scan(&tenantA); err != nil {
			t.Fatalf("insert tenant A: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ('t03-grant-b','grant B') RETURNING id::text`).Scan(&tenantB); err != nil {
			t.Fatalf("insert tenant B: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('t03a-scope',$1::uuid),('t03b-scope',$2::uuid)`, tenantA, tenantB); err != nil {
			t.Fatalf("map probe scopes: %v", err)
		}
		// A is granted read on B's scope (the friend-tenant case).
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope) VALUES ($1::uuid,'t03b-scope')`, tenantA); err != nil {
			t.Fatalf("insert grant A→t03b-scope: %v", err)
		}
		// A key in tenant A, home='t03a-scope', allowed={}: 't03b-scope' can ONLY
		// arrive via the grant.
		key, plaintext, err := store.CreateApiKey(ctx, pool, "t03-grant-key", "t03a-scope", []string{}, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET tenant_id = $1::uuid WHERE id = $2::uuid`, tenantA, key.ID); err != nil {
			t.Fatalf("pin key to tenant A: %v", err)
		}
		var (
			readScopes []string
			tenantID   *string
		)
		err = pool.QueryRow(ctx,
			`SELECT read_scopes, tenant_id::text FROM ctx_auth($1)`, plaintext,
		).Scan(&readScopes, &tenantID)
		if err != nil {
			t.Fatalf("ctx_auth select failed (RED without 060: 42703): code=%q err=%v", pgCode(err), err)
		}
		// home first, then the grant: {t03a-scope, t03b-scope} — exactly.
		if len(readScopes) != 2 || readScopes[0] != "t03a-scope" || readScopes[1] != "t03b-scope" {
			t.Fatalf("read_scopes = %v, want [t03a-scope t03b-scope] (home then grant)", readScopes)
		}
		if tenantID == nil || *tenantID != tenantA {
			t.Fatalf("tenant_id = %v, want tenant A %q", tenantID, tenantA)
		}
	})

	// (5) Status gate: a key whose tenant is 'suspended' authenticates to the
	// __UNAUTHORIZED__ sentinel (is_valid=false, tenant_id NULL) — design/01
	// §5.2. The fail-closed condition is `IS NULL OR <> 'active'`. RED: 42703.
	t.Run("suspended_tenant_unauthorized", func(t *testing.T) {
		var suspID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name, status)
			 VALUES ('t03-suspended','suspended probe','suspended') RETURNING id::text`).Scan(&suspID); err != nil {
			t.Fatalf("insert suspended tenant: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('t03-susp-scope', $1::uuid)`, suspID); err != nil {
			t.Fatalf("map suspended scope: %v", err)
		}
		key, plaintext, err := store.CreateApiKey(ctx, pool, "t03-susp-key", "t03-susp-scope", nil, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		// CreateApiKey defaults the key to the default tenant; re-pin it to the
		// suspended tenant so the status gate has something non-active to reject.
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET tenant_id = $1::uuid WHERE id = $2::uuid`, suspID, key.ID); err != nil {
			t.Fatalf("pin key to suspended tenant: %v", err)
		}
		var (
			homeScope string
			isValid   bool
			tenantID  *string
		)
		err = pool.QueryRow(ctx,
			`SELECT home_scope, is_valid, tenant_id::text FROM ctx_auth($1)`, plaintext,
		).Scan(&homeScope, &isValid, &tenantID)
		if err != nil {
			t.Fatalf("ctx_auth select failed (RED without 060: 42703): code=%q err=%v", pgCode(err), err)
		}
		if isValid {
			t.Fatal("suspended-tenant key is_valid=true, want false (status gate)")
		}
		if homeScope != "__UNAUTHORIZED__" {
			t.Fatalf("home_scope = %q, want '__UNAUTHORIZED__' (suspend sentinel)", homeScope)
		}
		if tenantID != nil {
			t.Fatalf("tenant_id = %v, want NULL in sentinel", *tenantID)
		}
	})

	// (5b) Offboarding tenant → __UNAUTHORIZED__, same as suspended. The gate
	// condition `<> 'active'` rejects both non-active statuses via the same branch;
	// this pins the 'offboarding' arm explicitly (subtest 5 only pins 'suspended').
	t.Run("offboarding_tenant_unauthorized", func(t *testing.T) {
		var offID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name, status)
			 VALUES ('t03-offboarding','offboarding probe','offboarding') RETURNING id::text`).Scan(&offID); err != nil {
			t.Fatalf("insert offboarding tenant: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('t03-off-scope', $1::uuid)`, offID); err != nil {
			t.Fatalf("map offboarding scope: %v", err)
		}
		key, plaintext, err := store.CreateApiKey(ctx, pool, "t03-off-key", "t03-off-scope", nil, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET tenant_id = $1::uuid WHERE id = $2::uuid`, offID, key.ID); err != nil {
			t.Fatalf("pin key to offboarding tenant: %v", err)
		}
		var (
			homeScope string
			isValid   bool
		)
		if err := pool.QueryRow(ctx,
			`SELECT home_scope, is_valid FROM ctx_auth($1)`, plaintext).Scan(&homeScope, &isValid); err != nil {
			t.Fatalf("ctx_auth: %v", err)
		}
		if isValid || homeScope != "__UNAUTHORIZED__" {
			t.Fatalf("offboarding key: is_valid=%v home_scope=%q, want false/__UNAUTHORIZED__", isValid, homeScope)
		}
	})

	// (6) '_'-prefixed scopes are filtered OUT of read_scopes (fail-closed:
	// system scopes never visible). A legacy key with allowed={_global} (inserted
	// directly — CreateApiKey blocks '_' at api_keys.go:47, the DB column has no
	// CHECK) must resolve to home-only, NEVER leaking _global. RED: 42703.
	t.Run("underscore_scope_filtered", func(t *testing.T) {
		const plaintext = "00000000000000000000000000000000000000000000000000000000000003ff"
		sum := sha256.Sum256([]byte(plaintext))
		keyHash := hex.EncodeToString(sum[:])
		if _, err := pool.Exec(ctx,
			`WITH p AS (
			     INSERT INTO context_principals (display_name) VALUES ('test-fixture') RETURNING id
			 )
			 INSERT INTO context_api_keys (key_hash, label, home_scope, allowed_scopes, tenant_id, principal_id)
			 SELECT $1, 't03-underscore', 'private', '{_global}', $2::uuid, p.id FROM p`, keyHash, defaultTenantID); err != nil {
			t.Fatalf("direct-insert underscore key: %v", err)
		}
		var (
			readScopes []string
			tenantID   *string
		)
		err := pool.QueryRow(ctx,
			`SELECT read_scopes, tenant_id::text FROM ctx_auth($1)`, plaintext,
		).Scan(&readScopes, &tenantID)
		if err != nil {
			t.Fatalf("ctx_auth select failed (RED without 060: 42703): code=%q err=%v", pgCode(err), err)
		}
		for _, s := range readScopes {
			if s == "_global" {
				t.Fatalf("read_scopes = %v leaks '_global' (the '_'-filter is gone)", readScopes)
			}
		}
		if len(readScopes) != 1 || readScopes[0] != "private" {
			t.Fatalf("read_scopes = %v, want [private] (home only, _global filtered)", readScopes)
		}
		if tenantID == nil {
			t.Fatal("tenant_id NULL for a valid key, want default")
		}
	})

	// (7) Invalid key keeps the sentinel shape with the NEW columns:
	// tenant_id NULL, tenant_role '' (design/01 §5.2). RED: 42703.
	t.Run("invalid_key_sentinel_tenant_null", func(t *testing.T) {
		var (
			isValid, isAdmin bool
			tenantID         *string
			tenantRole       string
		)
		err := pool.QueryRow(ctx,
			`SELECT is_valid, is_admin, tenant_id::text, tenant_role
			 FROM ctx_auth('deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef')`,
		).Scan(&isValid, &isAdmin, &tenantID, &tenantRole)
		if err != nil {
			t.Fatalf("ctx_auth select failed (RED without 060: 42703): code=%q err=%v", pgCode(err), err)
		}
		if isValid || isAdmin {
			t.Fatalf("invalid key: is_valid=%v is_admin=%v, want false/false", isValid, isAdmin)
		}
		if tenantID != nil {
			t.Fatalf("invalid key tenant_id = %v, want NULL", *tenantID)
		}
		if tenantRole != "" {
			t.Fatalf("invalid key tenant_role = %q, want '' (sentinel)", tenantRole)
		}
	})

	// (8) Go-layer plumbing: auth.Authenticate carries TenantID/TenantRole from
	// the 8-col ctx_auth into AuthResult. Valid default key → default UUID +
	// 'member'; invalid key → empty TenantID (nullable scan) + empty TenantRole.
	// (This subtest references the new AuthResult fields, so it only COMPILES
	// after auth.go is extended — it is added in the GREEN phase, not RED.)
	t.Run("authenticate_carries_tenant_fields", func(t *testing.T) {
		_, plaintext, err := store.CreateApiKey(ctx, pool, "t03-authn", "private", nil, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if !ar.IsValid {
			t.Fatal("valid key authenticated invalid")
		}
		if ar.TenantID != defaultTenantID {
			t.Fatalf("AuthResult.TenantID = %q, want %q", ar.TenantID, defaultTenantID)
		}
		if ar.TenantRole != "member" {
			t.Fatalf("AuthResult.TenantRole = %q, want 'member'", ar.TenantRole)
		}

		bad, err := auth.Authenticate(ctx, pool, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		if err != nil {
			t.Fatalf("authenticate invalid: %v", err)
		}
		if bad.IsValid {
			t.Fatal("invalid key authenticated valid")
		}
		if bad.TenantID != "" {
			t.Fatalf("invalid key AuthResult.TenantID = %q, want empty (NULL→empty)", bad.TenantID)
		}
		if bad.TenantRole != "" {
			t.Fatalf("invalid key AuthResult.TenantRole = %q, want empty", bad.TenantRole)
		}
	})

	// (8b) NULL tenant_id → __UNAUTHORIZED__. This pins the fail-closed `IS NULL`
	// arm of the status gate: a key whose tenant_id is NULL (the column is still
	// nullable pre-T06) makes `SELECT status WHERE id = NULL` return no row, so
	// v_status is NULL. A bare `v_status <> 'active'` would be NULL/falsy and let
	// the key through (fail-OPEN); `IS NULL OR <> 'active'` rejects it. Without
	// this probe the IS NULL arm is untested (no other subtest produces a NULL
	// tenant_id — CreateApiKey always inherits the 059 DEFAULT).
	t.Run("null_tenant_id_unauthorized", func(t *testing.T) {
		key, plaintext, err := store.CreateApiKey(ctx, pool, "t03-null-tenant", "private", nil, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET tenant_id = NULL WHERE id = $1::uuid`, key.ID); err != nil {
			t.Fatalf("null the tenant_id: %v", err)
		}
		var (
			homeScope string
			isValid   bool
		)
		if err := pool.QueryRow(ctx,
			`SELECT home_scope, is_valid FROM ctx_auth($1)`, plaintext).Scan(&homeScope, &isValid); err != nil {
			t.Fatalf("ctx_auth: %v", err)
		}
		if isValid {
			t.Fatal("NULL-tenant_id key is_valid=true, want false (fail-closed IS NULL arm)")
		}
		if homeScope != "__UNAUTHORIZED__" {
			t.Fatalf("home_scope = %q, want '__UNAUTHORIZED__'", homeScope)
		}
	})

	// (9) Idempotency: re-applying the 060 DDL against an already-migrated DB is
	// a no-op (DROP FUNCTION IF EXISTS + CREATE + INSERT ON CONFLICT). SetupTestDB
	// applied it once; we re-apply it in a transaction (mirroring migrations.go)
	// and assert no error + ctx_auth still resolves the 8-col shape. Proves the
	// migration is safely re-runnable (2×-idempotent gate).
	t.Run("migration_060_idempotent", func(t *testing.T) {
		ddl, err := migrations.FS.ReadFile("060_ctx_auth_tenant.sql")
		if err != nil {
			t.Fatalf("read embedded 060: %v", err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("re-apply 060 (idempotency): %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit re-applied 060: %v", err)
		}
		// The 8-col function is still callable after the DROP+CREATE re-run.
		var tenantID *string
		var isValid bool
		if err := pool.QueryRow(ctx,
			`SELECT is_valid, tenant_id::text
			 FROM ctx_auth('deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef')`).
			Scan(&isValid, &tenantID); err != nil {
			t.Fatalf("ctx_auth 8-col after re-apply: %v", err)
		}
		if isValid || tenantID != nil {
			t.Fatalf("post-reapply sentinel: is_valid=%v tenant_id=%v, want false/NULL", isValid, tenantID)
		}
	})
}
