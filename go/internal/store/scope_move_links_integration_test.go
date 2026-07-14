//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// GD5 gate (K8, plan graph-structural 2026-07-14): a manage-update scope move
// must not break the link tables' row-scope invariant post-hoc. Design chosen
// at build time (documented in the wave commit): LINK SWEEP in the same
// transaction — links whose far endpoint already lives in the target scope
// follow (row.scope update, covers raw-injected legacy rows), links whose far
// endpoint stays behind are DELETED (dream links regenerate; a structural
// link with scope-divergent endpoints could never be re-created by the write
// invariant, so the row would be dead, wrongly-scoped ballast). An update
// gate was rejected: it would block practically every move.
//
// Red probe against HEAD: before the sweep the moved block keeps its old
// link rows — both asserts below (rows deleted / rows moved) fail.
func TestUpdateBlock_ScopeMoveSweepsLinks(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		blkA = "019f60d5-0001-7000-9000-00000000000a" // moves X → Y
		blkB = "019f60d5-0001-7000-9000-00000000000b" // stays in X
		blkC = "019f60d5-0001-7000-9000-00000000000c" // stays in X
		blkD = "019f60d5-0001-7000-9000-00000000000d" // already in Y (raw-seeded link)
	)
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope) VALUES
		  ($1, 'learnings', 'gd5 A', 'x', 'gd5-x'),
		  ($2, 'learnings', 'gd5 B', 'x', 'gd5-x'),
		  ($3, 'learnings', 'gd5 C', 'x', 'gd5-x'),
		  ($4, 'learnings', 'gd5 D', 'x', 'gd5-y')`,
		blkA, blkB, blkC, blkD); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}
	// Consistent pre-move state: dream A→B, structural A→C, structural B→C
	// (untouched bystander). Plus ONE deliberately invariant-broken row
	// (raw-injected, the audit case K8 defends against): structural A→D with
	// row.scope 'gd5-x' while D lives in gd5-y — after moving A to gd5-y this
	// row becomes consistent again and must FOLLOW (row.scope → gd5-y).
	// Second dream row covers the REVERSE direction (moved block as target)
	// and the dream FOLLOW path in one: D→A with D already in gd5-y — after
	// the move it is consistent again and must follow, exercising the CASE
	// ELSE branch (review hardening).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, scope) VALUES
		  ($1, $2, 'topical', 0.9, 'gd5-x'),
		  ($3, $1, 'topical', 0.9, 'gd5-x')`, blkA, blkB, blkD); err != nil {
		t.Fatalf("seed dream links: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin) VALUES
		  ($1, $2, 'references', 'gd5-x', 'system'),
		  ($3, $2, 'references', 'gd5-x', 'system'),
		  ($1, $4, 'references', 'gd5-x', 'system')`, blkA, blkC, blkB, blkD); err != nil {
		t.Fatalf("seed structural links: %v", err)
	}

	newScope := "gd5-y"
	blk, _, err := store.UpdateBlock(ctx, pool, blkA,
		store.UpdateBlockData{Scope: &newScope}, []string{"gd5-x", "gd5-y"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if blk == nil || blk.Scope != "gd5-y" {
		t.Fatalf("block did not move: %+v", blk)
	}

	// Broken-invariant links (far endpoint stayed in gd5-x) are gone…
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM context_dream_links
		WHERE source_block_id = $1 AND target_block_id = $2`, blkA, blkB).Scan(&n); err != nil {
		t.Fatalf("count dream: %v", err)
	}
	if n != 0 {
		t.Fatalf("dream link to left-behind endpoint survived the move (rows=%d)", n)
	}

	// …the reverse-direction dream link D→A FOLLOWED (dream follow path +
	// CASE ELSE branch)…
	var dreamScope string
	if err := pool.QueryRow(ctx, `
		SELECT scope FROM context_dream_links
		WHERE source_block_id = $1 AND target_block_id = $2`, blkD, blkA).Scan(&dreamScope); err != nil {
		t.Fatalf("D→A dream row missing (must follow, not be swept): %v", err)
	}
	if dreamScope != "gd5-y" {
		t.Fatalf("D→A dream row.scope = %q, want gd5-y (reverse follow path)", dreamScope)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM context_structural_links
		WHERE (source_block_id = $1 AND target_block_id = $2)`, blkA, blkC).Scan(&n); err != nil {
		t.Fatalf("count structural A→C: %v", err)
	}
	if n != 0 {
		t.Fatalf("structural link to left-behind endpoint survived the move (rows=%d)", n)
	}

	// …the now-consistent link FOLLOWED (row.scope updated)…
	var rowScope string
	if err := pool.QueryRow(ctx, `
		SELECT scope FROM context_structural_links
		WHERE source_block_id = $1 AND target_block_id = $2`, blkA, blkD).Scan(&rowScope); err != nil {
		t.Fatalf("A→D row missing (must follow, not be swept): %v", err)
	}
	if rowScope != "gd5-y" {
		t.Fatalf("A→D row.scope = %q, want gd5-y (follow path)", rowScope)
	}

	// …and the bystander link B→C is untouched.
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM context_structural_links
		WHERE source_block_id = $1 AND target_block_id = $2 AND scope = 'gd5-x'`, blkB, blkC).Scan(&n); err != nil {
		t.Fatalf("count bystander: %v", err)
	}
	if n != 1 {
		t.Fatalf("bystander link B→C was touched (rows=%d)", n)
	}
}
