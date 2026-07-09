//go:build integration

// Integration test for Multi-Tenant wave T05b (tenant-delete = full-prune): the
// FK-ordered, batched mass-DELETE that store.PruneTenant performs. T05b is the
// destructive slice of masterplan wave T05 (Achse 01-T5) carved out along the
// design's own seam (design/01 §4.3.1 + §6.3 N11). It is "NICHT metadata-only":
// a tenant's scope-carried data (blocks/links/blobs/sources/sessions) is tenant-
// LESS (scope-discriminated, Modell C) with NO implicit CASCADE onto
// context_blocks, so it must be deleted explicitly and in FK order.
//
// Verified FK facts (live context_store schema + migrations, W3/W9):
//   - context_dream_links.{source,target}_block_id → context_blocks: NO ACTION
//     (016:18-19) ⇒ the ONE hard ordering — links BEFORE blocks (else 23503).
//   - context_api_keys.tenant_id → context_tenants: ON DELETE RESTRICT (059:111)
//     ⇒ keys BEFORE tenant. An explicit RESTRICT raises SQLSTATE 23001
//     (restrict_violation), NOT 23503 (that is NO ACTION / a deferred FK) — the
//     design/01 §4.3.1 prose said "23503", the live testcontainer says 23001
//     (W3/W9: verified against the primary source, not the doc). DeleteApiKey is
//     a SOFT delete (active=false, api_keys.go:102) and does NOT clear the FK, so
//     prune HARD-deletes the keys.
//   - context_tenant_scopes.tenant_id → context_tenants: ON DELETE CASCADE
//     (059:77) ⇒ the scope mapping clears itself when the tenant goes; the DATA
//     in those scopes does NOT (that is the whole point of the explicit prune).
//   - context_temporal / graph_cluster_member → blocks: CASCADE (auto-clear);
//     context_chat_messages → chat_sessions: CASCADE (auto-clear).
//
// RED: store.PruneTenant is undefined → the package fails to COMPILE (the honest
// red for a new-symbol wave; the intermediate No-Op-stub additionally shows the
// full_prune assertions failing semantically before the real body lands).
//
// pgCode, defaultTenantID are declared in tenants_hybrid_integration_test.go
// (same store_test package) and reused here.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTenantPrune -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantPrune_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// countWhere runs a single COUNT(*) and fails the test on a query error.
	countWhere := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", query, err)
		}
		return n
	}

	// seedBlock inserts a minimal valid block in a scope and returns its id.
	seedBlock := func(t *testing.T, scope, title string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('learnings', $1, 'body', $2) RETURNING id::text`, title, scope).Scan(&id); err != nil {
			t.Fatalf("seed block %q in %q: %v", title, scope, err)
		}
		return id
	}

	// (1) FK proof — a naked tenant DELETE is BLOCKED while a key points at it
	// (api_keys.tenant_id ON DELETE RESTRICT, 059:111). This is the live DB fact
	// that FORCES the ordered prune (keys before tenant) and the HARD key delete.
	// An explicit RESTRICT raises 23001 (restrict_violation), not 23503. Green
	// today (the constraint exists); stands as the ordering anchor.
	t.Run("fk_proof_naked_tenant_delete_blocked", func(t *testing.T) {
		tn, err := store.CreateTenant(ctx, pool, "tp-fk1", "fk1")
		if err != nil {
			t.Fatalf("create tenant: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`WITH p AS (
			     INSERT INTO context_principals (display_name) VALUES ('test-fixture') RETURNING id
			 )
			 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
			 SELECT 'tp-fk1-key', 'k', 'private', $1::uuid, p.id FROM p`, tn.ID); err != nil {
			t.Fatalf("seed key: %v", err)
		}
		_, err = pool.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, tn.ID)
		if pgCode(err) != "23001" {
			t.Fatalf("naked tenant delete with a bound key: pgCode=%q want 23001 (api_keys ON DELETE RESTRICT)", pgCode(err))
		}
	})

	// (2) FK proof — a naked block DELETE is BLOCKED while a dream_link points at
	// it (context_dream_links.{source,target}_block_id NO ACTION, 016:18-19). This
	// is the live DB fact that FORCES links-before-blocks. Green today.
	t.Run("fk_proof_naked_block_delete_blocked", func(t *testing.T) {
		b1 := seedBlock(t, "tp-fk2-scope", "fk2-a")
		b2 := seedBlock(t, "tp-fk2-scope", "fk2-b")
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, scope)
			 VALUES ($1::uuid, $2::uuid, 'topical', 'tp-fk2-scope')`, b1, b2); err != nil {
			t.Fatalf("seed dream_link: %v", err)
		}
		_, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE scope = 'tp-fk2-scope'`)
		if pgCode(err) != "23503" {
			t.Fatalf("naked block delete with dangling dream_links: pgCode=%q want 23503 (dream_links NO ACTION)", pgCode(err))
		}
	})

	// (3) THE wave: full_prune clears EVERY scope-carried table for the tenant,
	// the key, the scope mapping (CASCADE) and the tenant row — AND leaves a
	// foreign tenant's data untouched (cross-tenant isolation, the security
	// property). RED against a No-Op stub: the victim data survives and these
	// assertions fail; GREEN once PruneTenant runs the ordered batched delete.
	t.Run("full_prune_clears_all_and_isolates", func(t *testing.T) {
		victim, err := store.CreateTenant(ctx, pool, "tp-victim", "Victim")
		if err != nil {
			t.Fatalf("create victim: %v", err)
		}
		const vs = "tp-victim-scope"
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, vs, victim.ID); err != nil {
			t.Fatalf("map scope: %v", err)
		}
		// Two blocks + a dream_link between them (source<>target CHECK), a blob, a
		// source, a chat session, and a key bound to the victim tenant.
		b1 := seedBlock(t, vs, "victim-a")
		b2 := seedBlock(t, vs, "victim-b")
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, scope)
			 VALUES ($1::uuid, $2::uuid, 'topical', $3)`, b1, b2, vs); err != nil {
			t.Fatalf("seed dream_link: %v", err)
		}
		// context_blobs_check: storage_type='db' requires data NOT NULL / file_path
		// NULL (the alternative is 'fs' + file_path). Use the db-storage branch.
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blobs (category, title, filename, mime_type, file_size, storage_type, data, scope)
			 VALUES ('learnings', 'b', 'f.txt', 'text/plain', 1, 'db', '\x00'::bytea, $1)`, vs); err != nil {
			t.Fatalf("seed blob: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/x', 'hashx', $1)`, vs); err != nil {
			t.Fatalf("seed source: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_chat_sessions (scope, read_scopes) VALUES ($1, ARRAY[$2])`, vs, vs); err != nil {
			t.Fatalf("seed chat session: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`WITH p AS (
			     INSERT INTO context_principals (display_name) VALUES ('test-fixture') RETURNING id
			 )
			 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
			 SELECT 'tp-victim-key', 'k', $1, $2::uuid, p.id FROM p`, vs, victim.ID); err != nil {
			t.Fatalf("seed key: %v", err)
		}
		// Bystander: a block in the default tenant's 'private' scope MUST survive.
		bystander := seedBlock(t, "private", "survivor")

		if err := store.PruneTenant(ctx, pool, victim.ID); err != nil {
			t.Fatalf("PruneTenant(victim): %v", err)
		}

		// Every scope-carried victim table is empty.
		for _, tbl := range []string{
			"context_blocks", "context_dream_links", "context_blobs",
			"context_sources", "context_chat_sessions",
		} {
			if n := countWhere(t, `SELECT count(*) FROM `+tbl+` WHERE scope = $1`, vs); n != 0 {
				t.Errorf("%s after prune: %d rows in %q, want 0 (orphan = NOT full-prune)", tbl, n, vs)
			}
		}
		// The scope mapping is gone (CASCADE), the key is HARD-deleted, the tenant
		// row is gone.
		if n := countWhere(t, `SELECT count(*) FROM context_tenant_scopes WHERE tenant_id = $1::uuid`, victim.ID); n != 0 {
			t.Errorf("context_tenant_scopes after prune: %d, want 0 (CASCADE)", n)
		}
		if n := countWhere(t, `SELECT count(*) FROM context_api_keys WHERE tenant_id = $1::uuid`, victim.ID); n != 0 {
			t.Errorf("context_api_keys after prune: %d, want 0 (HARD delete — soft would leave the RESTRICT FK)", n)
		}
		if _, err := store.GetTenant(ctx, pool, victim.ID); !errors.Is(err, store.ErrTenantNotFound) {
			t.Errorf("GetTenant(victim) after prune: %v, want ErrTenantNotFound", err)
		}
		// Cross-tenant isolation: the default tenant's block is UNTOUCHED.
		if n := countWhere(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, bystander); n != 1 {
			t.Errorf("bystander block (default tenant 'private') after prune: %d, want 1 (cross-tenant leak!)", n)
		}
	})

	// (4) An unknown or malformed tenant id is ErrTenantNotFound (404, no oracle),
	// NOT a silent no-op success — the prune must not pretend to delete a tenant
	// that never existed.
	t.Run("prune_unknown_not_found", func(t *testing.T) {
		if err := store.PruneTenant(ctx, pool, "11111111-2222-3333-4444-555566667777"); !errors.Is(err, store.ErrTenantNotFound) {
			t.Errorf("PruneTenant(unknown) = %v, want ErrTenantNotFound", err)
		}
		if err := store.PruneTenant(ctx, pool, "not-a-uuid"); !errors.Is(err, store.ErrTenantNotFound) {
			t.Errorf("PruneTenant(malformed) = %v, want ErrTenantNotFound (no 22P02 oracle)", err)
		}
	})
}
