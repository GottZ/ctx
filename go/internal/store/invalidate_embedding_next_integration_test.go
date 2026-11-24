//go:build integration

// Integration test for the Evokoa-Clean-Room-Plan Achse 04 W04-3 negative
// probe (design/04-reembed-migration.md §3.2b / §4.3 "Konvergenz-Invariante"):
// a Content-Update during an active re-embed migration MUST null the _next
// pair (embedding_next/embed_model_next), not only touch the live pair (which
// since issue #198 part B is kept, marked stale). Without the _next nulling, a stale embedding_next
// vector would sit next to fresh content, the migration's pending predicate
// (embedding_next IS NULL) would never re-pick the block, and Verify would
// count it complete while serving a content-foreign vector after cutover.
//
// This test is written test-first per the wave's Gate-Disziplin: it runs
// RED against the pre-W04-3 ClearEmbedding (the version that only nulls
// embedding/embed_model — captured in the wave's build log) and GREEN
// against the _next-extended version below. Migration 114 (which adds the
// embedding_next/embed_model_next columns) already exists at RED time —
// the columns exist, ClearEmbedding just doesn't touch them yet.
//
// The content-change path was ClearEmbedding then; it is InvalidateEmbeddingTx.
//
// Run: `go test -tags=integration ./internal/store/ -run TestInvalidateEmbedding_NextExtension -count=1 -v`.
package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestInvalidateEmbedding_NextExtension_Integration(t *testing.T) {
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

	t.Run("invalidation_nulls_the_next_pair_and_marks_the_live_one", func(t *testing.T) {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('issue', 'w04-3-clear-next', 'body', 'w04-3') RETURNING id::text`).Scan(&id); err != nil {
			t.Fatalf("seed block: %v", err)
		}

		// Live pair: the block is already embedded in the OLD space.
		if err := store.StoreEmbedding(ctx, pool, id, "from-model", vec(0.3)); err != nil {
			t.Fatalf("StoreEmbedding (live): %v", err)
		}
		// _next pair: simulates a migration worker having already migrated
		// this block's vector to the NEW space (StoreEmbeddingNext itself is
		// W04-4 — direct SQL stands in for it here, this test only pins
		// InvalidateEmbeddingTx's contract, not the worker's).
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET embedding_next = $2, embed_model_next = $3 WHERE id = $1`,
			id, pgvecLiteral(vec(0.7)), "to-model",
		); err != nil {
			t.Fatalf("seed embedding_next: %v", err)
		}

		var preEmbNext, preModelNext bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding_next IS NULL, embed_model_next IS NULL FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&preEmbNext, &preModelNext); err != nil {
			t.Fatalf("read pre-clear _next state: %v", err)
		}
		if preEmbNext || preModelNext {
			t.Fatalf("precondition: embedding_next/embed_model_next already NULL before InvalidateEmbeddingTx — seed did not take")
		}

		// This is the Content-Update-analogue: InvalidateEmbeddingTx IS the
		// path manage-update/upsert-with-content-change invoke to invalidate a
		// stale vector (design §3.2b/§4.3). Simulating "a content update
		// happened during an active migration" is exactly "InvalidateEmbeddingTx
		// ran while embedding_next was populated".
		if err := store.InvalidateEmbeddingTx(ctx, pool, id); err != nil {
			t.Fatalf("InvalidateEmbeddingTx: %v", err)
		}

		// Since issue #198 part B the live pair is kept and marked stale until
		// the backfill replaces it; the _next pair is nulled as before.
		var embNull, modelNull, stale, embNextNull, modelNextNull bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL, embed_model IS NULL, embedding_stale, embedding_next IS NULL, embed_model_next IS NULL
			 FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&embNull, &modelNull, &stale, &embNextNull, &modelNextNull); err != nil {
			t.Fatalf("read post-clear state: %v", err)
		}
		if embNull || modelNull || !stale {
			t.Errorf("live pair: embedding_null=%v embed_model_null=%v stale=%v, want it kept and marked stale", embNull, modelNull, stale)
		}
		if !embNextNull || !modelNextNull {
			t.Errorf("_next pair not nulled: embedding_next_null=%v embed_model_next_null=%v, want both true (§3.2b/§4.3 Konvergenz-Invariante)", embNextNull, modelNextNull)
		}
	})

	// Negative probe for the W04-2/W04-3 coupling-resolution decision
	// (Lead-Entscheid, wave briefing): InvalidateEmbeddingTx must ALSO
	// delete the block's backfill memo row (context_embed_failures,
	// migration_id IS NULL) — the oversize infinity-memo's semantics are
	// "parked until content changes" (design §4.4), and InvalidateEmbeddingTx
	// IS the content-change path. Without the delete, a shrunk block stays parked forever
	// even though it would now fit.
	t.Run("invalidation_deletes_backfill_memo", func(t *testing.T) {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('issue', 'w04-3-clear-memo', 'body', 'w04-3') RETURNING id::text`).Scan(&id); err != nil {
			t.Fatalf("seed block: %v", err)
		}
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureOversize, "oversize: too big", 0, 0); err != nil {
			t.Fatalf("seed oversize memo: %v", err)
		}
		var preCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_failures WHERE block_id = $1::uuid AND migration_id IS NULL`, id).
			Scan(&preCount); err != nil {
			t.Fatalf("read pre-clear memo count: %v", err)
		}
		if preCount != 1 {
			t.Fatalf("precondition: expected exactly 1 backfill memo, got %d", preCount)
		}

		if err := store.InvalidateEmbeddingTx(ctx, pool, id); err != nil {
			t.Fatalf("InvalidateEmbeddingTx: %v", err)
		}

		var postCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_failures WHERE block_id = $1::uuid AND migration_id IS NULL`, id).
			Scan(&postCount); err != nil {
			t.Fatalf("read post-clear memo count: %v", err)
		}
		if postCount != 0 {
			t.Errorf("backfill memo survived InvalidateEmbeddingTx (count=%d), want deleted — a shrunk block would stay parked forever", postCount)
		}
	})
}

// pgvecLiteral renders a float32 slice as the pgvector text literal
// ("[0.3,0.3,…]") this file's raw-SQL seed uses to populate embedding_next
// directly (bypassing StoreEmbeddingNext, which does not exist yet — W04-4).
func pgvecLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%v", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
