//go:build integration

package handler

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestStatusDBEmbedBacklogCountsStale: since issue #198 part B a title or
// content change keeps the previous vector, marked stale, and the backfill
// re-embeds the block. The /status embed_backlog counts what the backfill
// has to do, so a stale block is pending although its embedding is not NULL;
// a stale block whose vector a cutover sweep nulled is counted once. Without
// the stale-pick indexes the stale set is not counted (no unguarded scan),
// and the count is the unembedded set: without them a content change nulls
// the vector, so only stale blocks left from before are missing from it.
func TestStatusDBEmbedBacklogCountsStale(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = float32(i%2) + 0.5
	}

	ids := make([]string, 6)
	for i := range ids {
		b, err := store.UpsertBlock(ctx, pool, "learnings", "backlog198 "+string(rune('a'+i)), "text", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = b.ID
	}
	// ids[0..1] stay unembedded; ids[2..5] get a vector; ids[2..4] change.
	for _, id := range ids[2:] {
		if err := store.StoreEmbedding(ctx, pool, id, "test-embed", vec); err != nil {
			t.Fatal(err)
		}
	}
	changed := "text v2"
	for _, id := range ids[2:5] {
		if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: &changed}, []string{scope}); err != nil {
			t.Fatal(err)
		}
	}
	// ids[4]: a cutover sweep nulled the vector, the marker stayed.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = NULL, embed_model = NULL WHERE id = $1::uuid`, ids[4]); err != nil {
		t.Fatal(err)
	}

	got := queryEmbedBacklog(ctx, pool)
	if got == nil || *got != 5 {
		t.Fatalf("EmbedBacklog = %v, want 5 (2 unembedded, 2 stale, 1 stale without a vector counted once)", got)
	}

	if _, err := pool.Exec(ctx, `DROP INDEX idx_embedding_stale_scope_sens, idx_embedding_stale_sens`); err != nil {
		t.Fatal(err)
	}
	if got := queryEmbedBacklog(ctx, pool); got == nil || *got != 3 {
		t.Errorf("without the stale-pick indexes EmbedBacklog = %v, want 3 (the unembedded set only)", got)
	}
}
