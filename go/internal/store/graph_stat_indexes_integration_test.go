//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// TestGraphStatIndexes_M106 is the W04-4 gate (plan graph-structural
// 2026-07-14, design/04 §3/§7): migration 106 ships the statistics/prune
// indexes in FINAL form while the tables are small (the single-tx runner
// cannot CONCURRENTLY, tenant.go N11 doctrine).
//
// Assertions:
//  1. BOTH indexes exist (pg_indexes) — covering INCLUDE on the structural
//     side is asserted via indexdef.
//  2. The _migrations footer row exists (version=106). NOTE (review-corrected
//     against migrations.go:100-106): the runner records every applied
//     migration ITSELF, so a missing in-file footer would not cause re-runs —
//     the footer is a file-level convention (076/103/104) and its ON CONFLICT
//     is what the double-run replay in (3) actually exercises.
//  3. Double-run idempotence: re-executing the migration file verbatim
//     errors nothing and changes nothing.
//
// Red probe (documented in the wave commit): this test predates the
// migration file — before 106 exists, assertions 1+2 fail against a fresh
// SetupTestDB (which applies every shipped migration).
func TestGraphStatIndexes_M106(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// (1) both indexes exist; structural side must be covering.
	var structDef string
	if err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'context_structural_links' AND indexname = 'idx_struct_links_scope_created'
	`).Scan(&structDef); err != nil {
		t.Fatalf("idx_struct_links_scope_created missing: %v", err)
	}
	for _, want := range []string{"(scope, created_at)", "INCLUDE (link_class, origin)"} {
		if !strings.Contains(structDef, want) {
			t.Fatalf("idx_struct_links_scope_created lacks %q — got %q", want, structDef)
		}
	}
	var dreamDef string
	if err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'context_dream_links' AND indexname = 'idx_dream_links_scope_created'
	`).Scan(&dreamDef); err != nil {
		t.Fatalf("idx_dream_links_scope_created missing: %v", err)
	}
	if !strings.Contains(dreamDef, "(scope, created_at)") {
		t.Fatalf("idx_dream_links_scope_created lacks (scope, created_at) — got %q", dreamDef)
	}
	// Deliberate asymmetry (design/04 §3): the dream side must NOT be covering
	// — context_dream_links is replace-swept on the hottest link-write path.
	if strings.Contains(dreamDef, "INCLUDE") {
		t.Fatalf("idx_dream_links_scope_created must not be covering — got %q", dreamDef)
	}

	// (2) _migrations footer row.
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1 FROM _migrations WHERE version = 106`).Scan(&one); err != nil {
		t.Fatalf("_migrations row for 106 missing: %v", err)
	}

	// Hardening (design/04 §3 "idx_struct_links_scope bleibt bestehen"): the
	// old scope-only index must survive — 106 adds, never replaces.
	if err := pool.QueryRow(ctx, `
		SELECT 1 FROM pg_indexes
		WHERE tablename = 'context_structural_links' AND indexname = 'idx_struct_links_scope'
	`).Scan(&one); err != nil {
		t.Fatalf("idx_struct_links_scope must remain: %v", err)
	}

	// (3) double-run idempotence: replay the shipped file verbatim.
	sql, err := migrations.Section("106_graph_stat_indexes.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("double-run not idempotent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit double-run: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 106`).Scan(&count); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if count != 1 {
		t.Fatalf("_migrations rows for 106 after double-run = %d, want 1", count)
	}
}
