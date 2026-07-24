//go:build integration

// Integration test for the Evokoa-Clean-Room-Plan Achse 04 W04-8 (Upsert-
// Stale-Vektor-Fix, design/04-reembed-migration.md §7 W04-8 + Inventur §3.4):
// UpsertBlock's ON CONFLICT (category,title,scope) path did NOT invalidate a
// block's embedding when the conflicting write changed content — `ctx save`
// on an existing title with new content left the OLD vector sitting next to
// the NEW content (context_store.go:184-236 → blocks.go:218-223 pre-fix,
// Inventur §3.4 point 8). Only `manage update`/MCP `update` ran
// ClearEmbedding; the upsert conflict path never did.
//
// This matters beyond a stale-search-result bug: W04-4's re-embed migration
// convergence invariant (design §4.3 "Konvergenz-Invariante") has this as a
// HARD PRECONDITION — without it, an upsert during an active migration could
// leave `embedding_next` populated (already migrated) sitting next to fresh
// content the migration's pending predicate never re-picks, and Verify (which
// checks model/norm, never content) would count the block complete while
// serving a content-foreign vector permanently after cutover (§7 W04-8 row,
// §4.3 last paragraph).
//
// The fix reuses the ClearEmbeddingTx primitive (W04-3) inside UpsertBlock's
// own transaction — not bespoke SQL — so future extensions of the primitive
// (e.g. a W04-4 migration-scoped memo delete) are picked up here for free.
//
// RED premise (captured verbatim in the wave's build log before the fix):
// subtest "conflict_with_changed_content_bug" failed with:
//
//	upsert_stale_vector_integration_test.go:NN: post-upsert (PRE-FIX BEHAVIOR
//	PINNED, Inventur §3.4): embedding_is_null=false embed_model="from-model"
//	— old vector survived a content change (want embedding_is_null=true)
//
// i.e. embedding IS NOT NULL and embed_model unchanged after a content
// change — the Ist-Bug from the wave briefing, verbatim.
//
// Run: go test -tags=integration ./internal/store/ -run TestUpsertBlock_StaleVector -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestUpsertBlock_StaleVector_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	vec := func(fill float32) []float32 {
		v := make([]float32, 1024)
		for i := range v {
			v[i] = fill
		}
		return v
	}

	// Gate 1 (Rot→Grün): the Ist-Bug itself. A block with a live vector +
	// embed_model gets re-upserted under the SAME (category,title,scope) key
	// with DIFFERENT content. Pre-fix, embedding/embed_model survive
	// untouched (Ist-Bug). Post-fix, ALL FOUR embedding columns null out and
	// the backfill memo is deleted — the same contract ClearEmbedding gives
	// manage-update, now also on the upsert-conflict path.
	t.Run("conflict_with_changed_content_clears_embedding", func(t *testing.T) {
		b, err := store.UpsertBlock(ctx, pool, "learnings", "w04-8-changed", "content v1", nil, nil, "w04-8", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("initial upsert: %v", err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "from-model", vec(0.4)); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		// Seed a backfill memo too — ClearEmbedding's contract (W04-2/W04-3
		// coupling) deletes it; the upsert-conflict path must inherit that,
		// not hand-roll a partial clear that forgets the memo.
		if err := store.RecordEmbedFailure(ctx, pool, b.ID, store.EmbedFailureOversize, "oversize: too big", 0, 0); err != nil {
			t.Fatalf("seed backfill memo: %v", err)
		}

		var preMemo int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_failures WHERE block_id = $1::uuid AND migration_id IS NULL`, b.ID).
			Scan(&preMemo); err != nil {
			t.Fatalf("read pre-upsert memo count: %v", err)
		}
		if preMemo != 1 {
			t.Fatalf("precondition: expected exactly 1 backfill memo before re-upsert, got %d", preMemo)
		}

		if _, err := store.UpsertBlock(ctx, pool, "learnings", "w04-8-changed", "content v2 — DIFFERENT", nil, nil, "w04-8", false, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("conflicting upsert (changed content): %v", err)
		}

		var embNull, modelNull, embNextNull, modelNextNull bool
		var model, content string
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL, embed_model IS NULL, embedding_next IS NULL, embed_model_next IS NULL,
			        COALESCE(embed_model, ''), content
			 FROM context_blocks WHERE id = $1::uuid`, b.ID).
			Scan(&embNull, &modelNull, &embNextNull, &modelNextNull, &model, &content); err != nil {
			t.Fatalf("read post-upsert state: %v", err)
		}
		if content != "content v2 — DIFFERENT" {
			t.Fatalf("content = %q, want the new content (upsert itself must still apply)", content)
		}
		if !embNull || !modelNull {
			// This is the literal Ist-Bug wording the wave briefing asked for
			// (Gate 1 "Rot", captured verbatim in the file header once run
			// against the pre-fix source).
			t.Errorf("post-upsert (PRE-FIX BEHAVIOR PINNED, Inventur §3.4): embedding_is_null=%v embed_model=%q — old vector survived a content change (want embedding_is_null=true)", embNull, model)
		}
		if !embNextNull || !modelNextNull {
			t.Errorf("post-upsert: embedding_next_is_null=%v embed_model_next_is_null=%v, want both true (ClearEmbeddingTx contract, W04-3)", embNextNull, modelNextNull)
		}

		var postMemo int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_failures WHERE block_id = $1::uuid AND migration_id IS NULL`, b.ID).
			Scan(&postMemo); err != nil {
			t.Fatalf("read post-upsert memo count: %v", err)
		}
		if postMemo != 0 {
			t.Errorf("backfill memo survived a content-changing upsert (count=%d), want deleted (ClearEmbeddingTx contract)", postMemo)
		}
	})

	// Gate 2 (Idempotenz-Schutz, negative probe): re-upserting with the
	// IDENTICAL content must NOT clear the vector or the memo. A real
	// content-change detector, not "always clear on conflict" — the latter
	// would make the backfill re-embed on every idempotent `ctx save`
	// no-op at scale (Ziel-Scale 1M-10M, organic growth means repeat saves
	// are common).
	t.Run("conflict_with_identical_content_preserves_embedding", func(t *testing.T) {
		b, err := store.UpsertBlock(ctx, pool, "learnings", "w04-8-identical", "stable content", nil, nil, "w04-8", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("initial upsert: %v", err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "stable-model", vec(0.6)); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		if err := store.RecordEmbedFailure(ctx, pool, b.ID, store.EmbedFailureOversize, "oversize: too big", 0, 0); err != nil {
			t.Fatalf("seed backfill memo: %v", err)
		}

		if _, err := store.UpsertBlock(ctx, pool, "learnings", "w04-8-identical", "stable content", nil, nil, "w04-8", false, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("no-op re-upsert (identical content): %v", err)
		}

		var embNull, modelNull bool
		var model string
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL, embed_model IS NULL, COALESCE(embed_model, '')
			 FROM context_blocks WHERE id = $1::uuid`, b.ID).
			Scan(&embNull, &modelNull, &model); err != nil {
			t.Fatalf("read post-upsert state: %v", err)
		}
		if embNull || modelNull || model != "stable-model" {
			t.Errorf("identical-content upsert cleared the vector: embedding_is_null=%v embed_model_is_null=%v embed_model=%q, want unchanged (stable-model)", embNull, modelNull, model)
		}

		var memoCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_failures WHERE block_id = $1::uuid AND migration_id IS NULL`, b.ID).
			Scan(&memoCount); err != nil {
			t.Fatalf("read memo count: %v", err)
		}
		if memoCount != 1 {
			t.Errorf("identical-content upsert touched the backfill memo (count=%d), want untouched (1)", memoCount)
		}
	})

	// Gate 3 (Konvergenz, W04-4-Vorbedingung): simulate mid-migration state
	// (embedding_next already populated by a migration worker) plus a
	// content-changing upsert — BOTH vector pairs must null out. This is the
	// §4.3 Konvergenz-Invariante's hard precondition: without it, the
	// migration's pending predicate (embedding_next IS NULL) would never
	// re-pick this block, and Verify would count it complete while serving a
	// content-foreign vector forever after cutover.
	t.Run("conflict_during_simulated_migration_clears_both_pairs", func(t *testing.T) {
		b, err := store.UpsertBlock(ctx, pool, "learnings", "w04-8-migration", "pre-migration content", nil, nil, "w04-8", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("initial upsert: %v", err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "from-model", vec(0.2)); err != nil {
			t.Fatalf("StoreEmbedding (live pair): %v", err)
		}
		// Simulate a migration worker having already migrated this block's
		// vector to the new space (StoreEmbeddingNext is W04-4 — raw SQL
		// stands in, same pattern as the W04-3 negative probe).
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET embedding_next = $2, embed_model_next = $3 WHERE id = $1`,
			b.ID, pgvecLiteral(vec(0.8)), "to-model",
		); err != nil {
			t.Fatalf("seed embedding_next: %v", err)
		}

		var preEmbNext, preModelNext bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding_next IS NULL, embed_model_next IS NULL FROM context_blocks WHERE id = $1::uuid`, b.ID).
			Scan(&preEmbNext, &preModelNext); err != nil {
			t.Fatalf("read pre-upsert _next state: %v", err)
		}
		if preEmbNext || preModelNext {
			t.Fatalf("precondition: embedding_next/embed_model_next already NULL before the conflicting upsert — seed did not take")
		}

		if _, err := store.UpsertBlock(ctx, pool, "learnings", "w04-8-migration", "post-migration content — CHANGED", nil, nil, "w04-8", false, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("conflicting upsert during simulated migration: %v", err)
		}

		var embNull, modelNull, embNextNull, modelNextNull bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL, embed_model IS NULL, embedding_next IS NULL, embed_model_next IS NULL
			 FROM context_blocks WHERE id = $1::uuid`, b.ID).
			Scan(&embNull, &modelNull, &embNextNull, &modelNextNull); err != nil {
			t.Fatalf("read post-upsert state: %v", err)
		}
		if !embNull || !modelNull {
			t.Errorf("live pair not nulled: embedding_is_null=%v embed_model_is_null=%v, want both true", embNull, modelNull)
		}
		if !embNextNull || !modelNextNull {
			t.Errorf("_next pair not nulled: embedding_next_is_null=%v embed_model_next_is_null=%v, want both true — this is the §4.3 Konvergenz-Invariante's hard precondition (W04-4)", embNextNull, modelNextNull)
		}
	})
}
