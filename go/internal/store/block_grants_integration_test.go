//go:build integration

// Integration test for Multi-Tenant wave T39 (Achse 07-W1, migration 067
// "block_grants").
//
// T39 lays the THIRD level of the model-C 3-level architecture (user 2026-06-15):
// after tenant (059) and scope/department, the single-block read grant. It is one
// granularity FINER than context_tenant_grants (061, scope-level): a row shares
// ONE block_id with a grantee tenant rather than a whole scope. Exact schema
// vorlage: design/07-block-level-sharing.md §3.1.
//
// Mechanism = code (the OR-arm in the switch point, waves T40a/T40b), policy =
// data (THIS table). 067 is an ADDITIVE table with NO consumer yet — the
// VisibilityPredicate OR-arm (T40a) and the ctx_rrf sixfold OR (T40b/068) read it
// LATER. 067 only lays the table. Backfill-free (0 existing grants), idempotent,
// self-registering, one Tx, new-table-only (no context_blocks lock, no 1M index
// build — the decisive advantage of the join over the array variant, §3.2).
//
// Model C (E1): grantee_tenant is a real UUID FK on context_tenants (conflicts.md
// §0: NO tenant_id on data tables, but FKs on the owner register are allowed).
// block_id is a UUID FK on context_blocks with ON DELETE CASCADE (a deleted block
// cannot hold a grant — KONTRAST to context_dream_links, which has no ON DELETE
// and blocks the delete, design/07 §3.1). granted_by is ON DELETE SET NULL (key
// delete anonymizes the audit pointer, never deletes the grant). uq_block_grant
// (block_id, grantee_tenant) makes a double-grant idempotent and also covers the
// block_id-leading "who sees this block?" lookup; idx_block_grants_grantee
// (grantee_tenant, block_id) is the hot read "which blocks are granted to T?".
//
// There is DELIBERATELY no permission column in 067 (design/07 §3.1, Rev.2
// review-finding): a single-value 'read' CHECK enum gates nothing today (every row
// is necessarily 'read') — it is additive via Achse 05 when write/comment grants
// arrive (lock-trivial ADD COLUMN on this small table). The schema-artifact test
// asserts the column is ABSENT to guard against re-adding it.
//
// RED (today, WITH migrations up to 064 but WITHOUT 067): every sub-case fails
// with a DB-level Postgres error, NOT a compile error — the first statement
// against the table yields 42P01 undefined_table "context_block_grants".
// (context_tenants from 059 and context_blocks from 001 DO exist, so the failure
// pins specifically the missing grants table.)
// GREEN (after 067): all sub-cases pass.
//
// Note: pgCode, defaultTenantID and seedBlock are declared elsewhere in the
// store_test package (tenants_hybrid / scope_generalize integration tests) and
// reused here — they are NOT redeclared.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestBlockGrants -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// A syntactically valid UUID that is never registered as a block or a tenant —
// used to exercise the two FK guards in isolation.
const ghostBlockID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const ghostTenantID = "11111111-2222-3333-4444-555566667777"

// TestBlockGrantsMigration asserts the SCHEMA ARTIFACTS of migration 067 (the
// K-G1 primary gate: artifacts, not migration numbers). RED: the table does not
// exist (to_regclass NULL) → the first assertion fails.
func TestBlockGrantsMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// The table itself.
	var hasTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('context_block_grants') IS NOT NULL`).Scan(&hasTable); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if !hasTable {
		t.Fatal("migration 067 did not create context_block_grants")
	}

	// uq_block_grant (block_id, grantee_tenant): no double-grant + covers the
	// block_id-leading lookup.
	var hasUq bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='uq_block_grant')`).Scan(&hasUq); err != nil {
		t.Fatalf("check uq: %v", err)
	}
	if !hasUq {
		t.Error("uq_block_grant missing")
	}

	// idx_block_grants_grantee (grantee_tenant, block_id): the hot read path.
	var hasIdx bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname='idx_block_grants_grantee')`).Scan(&hasIdx); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if !hasIdx {
		t.Error("idx_block_grants_grantee missing")
	}

	// NO permission column (design/07 §3.1: single-value enum gates nothing;
	// additive via Achse 05). Guard against re-adding it.
	var hasPermission bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		   WHERE table_name='context_block_grants' AND column_name='permission')`).Scan(&hasPermission); err != nil {
		t.Fatalf("check permission column: %v", err)
	}
	if hasPermission {
		t.Error("context_block_grants has a permission column — design/07 §3.1 strips it (additive via Achse 05)")
	}

	// 2x-idempotent: a second migration run is a no-op (per-version skip), exactly
	// one v67 row.
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("second RunMigrations (idempotency): %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 67`).Scan(&count); err != nil {
		t.Fatalf("count migration 67: %v", err)
	}
	if count != 1 {
		t.Errorf("_migrations version 67 rows = %d, want exactly 1 (idempotent)", count)
	}
}

// TestBlockGrants_Constraints asserts the BEHAVIORAL contract: the two FK guards,
// the uq idempotency, and the three ON DELETE policies (block CASCADE, grantee
// CASCADE, granted_by SET NULL). Each is the load-bearing integrity check that a
// mutation of 067 (drop a constraint) turns red.
func TestBlockGrants_Constraints(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// The happy path: a real block granted to the registered default tenant
	// inserts and returns a uuidv7 PK. RED: 42P01 undefined_table.
	t.Run("valid_grant_inserts", func(t *testing.T) {
		blockID := seedBlock(t, pool, "t39-valid-grant")
		var id string
		err := pool.QueryRow(ctx,
			`INSERT INTO context_block_grants (block_id, grantee_tenant)
			 VALUES ($1::uuid, $2::uuid)
			 RETURNING id::text`, blockID, defaultTenantID).Scan(&id)
		if err != nil {
			t.Fatalf("INSERT valid grant failed (expected RED without 067: 42P01 undefined_table \"context_block_grants\", GREEN with): code=%q err=%v",
				pgCode(err), err)
		}
		if id == "" {
			t.Fatalf("valid grant returned empty id, want a uuidv7 PK")
		}
	})

	// block_id FK → context_blocks: an unregistered block UUID is rejected with
	// 23503 foreign_key_violation. grantee_tenant is VALID here so the failure
	// pins specifically to the block_id FK. RED: 42P01.
	t.Run("block_id_fk_rejects_unregistered", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO context_block_grants (block_id, grantee_tenant)
			 VALUES ($1::uuid, $2::uuid)`, ghostBlockID, defaultTenantID)
		if err == nil {
			t.Fatal("INSERT grant with unregistered block_id succeeded, want 23503 foreign_key_violation")
		}
		if code := pgCode(err); code != "23503" {
			t.Fatalf("block_id FK on unregistered block: got code=%q (%v), want 23503 foreign_key_violation (RED without 067: 42P01 undefined_table)", code, err)
		}
	})

	// grantee_tenant FK → context_tenants: an unregistered tenant UUID is rejected
	// with 23503. block_id is VALID here so the failure pins to the grantee FK.
	// RED: 42P01.
	t.Run("grantee_tenant_fk_rejects_unregistered", func(t *testing.T) {
		blockID := seedBlock(t, pool, "t39-grantee-fk")
		_, err := pool.Exec(ctx,
			`INSERT INTO context_block_grants (block_id, grantee_tenant)
			 VALUES ($1::uuid, $2::uuid)`, blockID, ghostTenantID)
		if err == nil {
			t.Fatal("INSERT grant with unregistered grantee_tenant succeeded, want 23503 foreign_key_violation")
		}
		if code := pgCode(err); code != "23503" {
			t.Fatalf("grantee_tenant FK on unregistered tenant: got code=%q (%v), want 23503 foreign_key_violation (RED without 067: 42P01)", code, err)
		}
	})

	// uq_block_grant (block_id, grantee_tenant): re-granting the same (block,
	// tenant) pair is rejected with 23505 unique_violation — idempotent at the
	// schema level. Rolled-back sub-tx so it does not pollute later sub-cases.
	// RED: 42P01 on the first INSERT.
	t.Run("duplicate_grant_rejected_by_uq", func(t *testing.T) {
		blockID := seedBlock(t, pool, "t39-dup-grant")
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if _, err := tx.Exec(ctx,
			`INSERT INTO context_block_grants (block_id, grantee_tenant)
			 VALUES ($1::uuid, $2::uuid)`, blockID, defaultTenantID); err != nil {
			t.Fatalf("first grant failed (RED without 067: 42P01): code=%q err=%v", pgCode(err), err)
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO context_block_grants (block_id, grantee_tenant)
			 VALUES ($1::uuid, $2::uuid)`, blockID, defaultTenantID)
		if err == nil {
			t.Fatal("duplicate grant (same block_id + grantee_tenant) succeeded, want 23505 unique_violation")
		}
		if code := pgCode(err); code != "23505" {
			t.Fatalf("duplicate grant: got code=%q (%v), want 23505 unique_violation (uq_block_grant)", code, err)
		}
	})

	// ON DELETE CASCADE on block_id: deleting the granted block removes the grant
	// (KONTRAST to dream_links, which would block the delete). THE distinguishing
	// T39 probe. Rolled-back sub-tx; seed the block INSIDE the tx so rollback
	// cleans it. RED: 42P01 on the grant INSERT.
	t.Run("grant_cascades_on_block_removal", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		var blockID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('learnings','t39-cascade-block','c','private') RETURNING id::text`).Scan(&blockID); err != nil {
			t.Fatalf("insert probe block: code=%q err=%v", pgCode(err), err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_block_grants (block_id, grantee_tenant)
			 VALUES ($1::uuid, $2::uuid)`, blockID, defaultTenantID); err != nil {
			t.Fatalf("insert grant on probe block (RED without 067: 42P01): code=%q err=%v", pgCode(err), err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1::uuid`, blockID); err != nil {
			t.Fatalf("delete granted block: code=%q err=%v", pgCode(err), err)
		}
		var remaining int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM context_block_grants WHERE block_id = $1::uuid`, blockID).Scan(&remaining); err != nil {
			t.Fatalf("count grants after block removal: code=%q err=%v", pgCode(err), err)
		}
		if remaining != 0 {
			t.Fatalf("grants on removed block = %d, want 0 (ON DELETE CASCADE on block_id FK)", remaining)
		}
	})

	// ON DELETE CASCADE on grantee_tenant: deleting the tenant that HOLDS a grant
	// removes the grant. Key-less probe tenant (the default tenant is
	// RESTRICT-blocked by its api_keys, 059) so the tenant DELETE succeeds.
	// Rolled-back sub-tx. RED: 42P01 on the grant INSERT.
	t.Run("grant_cascades_on_grantee_tenant_removal", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		var blockID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('learnings','t39-grantee-cascade','c','private') RETURNING id::text`).Scan(&blockID); err != nil {
			t.Fatalf("insert probe block: code=%q err=%v", pgCode(err), err)
		}
		var granteeID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name)
			 VALUES ('t39-grantee-probe','probe') RETURNING id::text`).Scan(&granteeID); err != nil {
			t.Fatalf("insert key-less grantee probe tenant: code=%q err=%v", pgCode(err), err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_block_grants (block_id, grantee_tenant)
			 VALUES ($1::uuid, $2::uuid)`, blockID, granteeID); err != nil {
			t.Fatalf("insert grant for probe grantee (RED without 067: 42P01): code=%q err=%v", pgCode(err), err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, granteeID); err != nil {
			t.Fatalf("delete key-less grantee tenant: code=%q err=%v", pgCode(err), err)
		}
		var remaining int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM context_block_grants WHERE grantee_tenant = $1::uuid`, granteeID).Scan(&remaining); err != nil {
			t.Fatalf("count grants after grantee removal: code=%q err=%v", pgCode(err), err)
		}
		if remaining != 0 {
			t.Fatalf("grants held by removed tenant = %d, want 0 (ON DELETE CASCADE on grantee_tenant FK)", remaining)
		}
	})

	// ON DELETE SET NULL on granted_by: deleting the api-key that minted the grant
	// ANONYMIZES the audit pointer (granted_by → NULL) but NEVER deletes the grant
	// (the share survives the key that minted it). Fresh key inside the sub-tx so
	// no other rows reference it. RED: 42P01 on the grant INSERT.
	t.Run("granted_by_set_null_on_key_delete", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin sub-tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		var blockID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('learnings','t39-setnull-block','c','private') RETURNING id::text`).Scan(&blockID); err != nil {
			t.Fatalf("insert probe block: code=%q err=%v", pgCode(err), err)
		}
		var keyID string
		if err := tx.QueryRow(ctx,
			`WITH p AS (
			     INSERT INTO context_principals (display_name) VALUES ('test-fixture') RETURNING id
			 )
			 INSERT INTO context_api_keys (key_hash, label, home_scope, principal_id)
			 SELECT 't39-granter-key','t39-granter','private', p.id FROM p RETURNING id::text`).Scan(&keyID); err != nil {
			t.Fatalf("insert granter key: code=%q err=%v", pgCode(err), err)
		}
		var grantID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO context_block_grants (block_id, grantee_tenant, granted_by)
			 VALUES ($1::uuid, $2::uuid, $3::uuid) RETURNING id::text`, blockID, defaultTenantID, keyID).Scan(&grantID); err != nil {
			t.Fatalf("insert grant with granted_by (RED without 067: 42P01): code=%q err=%v", pgCode(err), err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM context_api_keys WHERE id = $1::uuid`, keyID); err != nil {
			t.Fatalf("delete granter key: code=%q err=%v", pgCode(err), err)
		}
		var stillThere bool
		var grantedByNull bool
		if err := tx.QueryRow(ctx,
			`SELECT true, granted_by IS NULL FROM context_block_grants WHERE id = $1::uuid`, grantID).Scan(&stillThere, &grantedByNull); err != nil {
			t.Fatalf("re-read grant after key delete: code=%q err=%v", pgCode(err), err)
		}
		if !stillThere {
			t.Fatal("grant disappeared after granter-key delete, want it to survive (ON DELETE SET NULL, not CASCADE)")
		}
		if !grantedByNull {
			t.Fatal("granted_by not NULL after granter-key delete, want NULL (ON DELETE SET NULL anonymizes the audit pointer)")
		}
	})
}
