//go:build integration

// Negative gate for Achse-02 Welle I-A: the Dream replace cycle
// (dream.WriteLinks, writelinks.go:38-78) must NEVER touch structural edges.
// Structural edges live in their OWN table (context_structural_links, 076);
// dream edges live in context_dream_links. This test runs the REAL dream replace
// and contrasts the two placements:
//
//   - FAKE (a dream_links-based structural-link implementation): the edge is
//     stored in context_dream_links. The replace sweep of a later cycle deletes
//     it (it is a stale link of the same source not in the new plan) ⇒ the edge
//     is GONE. This subtest asserting count==0 IS the literal RED proof that a
//     dream_links-based fake loses the structural edge.
//   - REAL (context_structural_links via store.PutStructuralLink): the same real
//     replace cycle leaves the edge intact ⇒ GREEN.
//
// insertBlock / icBuiltinSet are declared in writelinks_integration_test.go
// (same package dream_test).
//
// Run: go test -tags=integration ./internal/dream/ -run TestDreamReplace_Structural -count=1 -v.
package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	slSourceID = "019d5000-0000-7000-9000-000000000001"
	slDreamTgt = "019d5000-0000-7000-9000-000000000002"
	slStructT  = "019d5000-0000-7000-9000-000000000003"
	slScope    = "sl-dream"
)

func TestDreamReplace_StructuralEdgesUntouched_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tE := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tL := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	insertBlock(t, pool, slSourceID, slScope, "decisions", "src", tE, tE)
	insertBlock(t, pool, slDreamTgt, slScope, "decisions", "dream-tgt", tL, tL)
	insertBlock(t, pool, slStructT, slScope, "decisions", "struct-tgt", tL, tL)

	// The dream plan for BOTH scenarios: keep ONLY slDreamTgt. Any other
	// dream_link of slSourceID is stale and gets swept by the replace.
	runReplace := func(t *testing.T) {
		t.Helper()
		n, err := dream.WriteLinks(ctx, pool, icBuiltinSet, slSourceID, slScope, 1.0,
			[]dream.Link{{TargetID: slDreamTgt, Relationship: "topical", Confidence: 0.9}})
		if err != nil {
			t.Fatalf("WriteLinks: %v", err)
		}
		if n < 1 {
			t.Fatalf("WriteLinks wrote %d links, want ≥1 (replace only fires on a non-empty cycle)", n)
		}
	}
	countDream := func(t *testing.T, src, tgt string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_dream_links WHERE source_block_id=$1::uuid AND target_block_id=$2::uuid`,
			src, tgt).Scan(&n); err != nil {
			t.Fatalf("count dream: %v", err)
		}
		return n
	}
	countStruct := func(t *testing.T, src, tgt string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_structural_links WHERE source_block_id=$1::uuid AND target_block_id=$2::uuid`,
			src, tgt).Scan(&n); err != nil {
			t.Fatalf("count struct: %v", err)
		}
		return n
	}

	// RED PROOF — a dream_links-based structural edge is destroyed by replace.
	t.Run("fake_edge_in_dream_links_is_destroyed_by_replace", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_dream_links (source_block_id,target_block_id,relationship,scope)
			 VALUES ($1::uuid,$2::uuid,'topical',$3)
			 ON CONFLICT (source_block_id,target_block_id) DO NOTHING`,
			slSourceID, slStructT, slScope); err != nil {
			t.Fatalf("seed fake dream_link: %v", err)
		}
		if countDream(t, slSourceID, slStructT) != 1 {
			t.Fatalf("fake edge not seeded")
		}
		runReplace(t)
		if got := countDream(t, slSourceID, slStructT); got != 0 {
			t.Fatalf("RED expectation broken: fake dream_links edge survived replace (got %d, want 0)", got)
		}
		// The KEPT dream link is still present — proves replace ran, not a wipe.
		if got := countDream(t, slSourceID, slDreamTgt); got != 1 {
			t.Fatalf("kept dream link missing after replace (got %d, want 1)", got)
		}
	})

	// GREEN — the REAL structural edge survives the identical replace cycle.
	t.Run("real_edge_in_structural_links_survives_replace", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := store.PutStructuralLink(ctx, tx,
			store.StructuralLink{SourceID: slSourceID, TargetID: slStructT, LinkClass: "references", Origin: "manual"},
			[]string{slScope}); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("PutStructuralLink: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if countStruct(t, slSourceID, slStructT) != 1 {
			t.Fatalf("real edge not written")
		}
		runReplace(t)
		if got := countStruct(t, slSourceID, slStructT); got != 1 {
			t.Fatalf("structural edge destroyed by dream replace (got %d, want 1) — separation broken", got)
		}
	})
}
