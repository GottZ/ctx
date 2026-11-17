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

// TestStatusDBEmbedBacklogStaleNeverEmbedded (#291, review round 4): a block
// that held a vector when its type became never-embedded keeps its vector
// and no marker (the reconcile marks unembedded blocks only); a content change
// before migration 176's second change (issue #213) marked it stale and kept
// the vector — a row of that kind is left from before it (since then a
// content change drops the vector, and the reconcile's stale pass of a queued
// name does too). The pick's stale branches filter it by the type predicate,
// so the backfill never re-embeds it, and the /status count must not count it
// either.
//
// RED before the stale branch carries store.RetrievalExcludedTypePredicate:
// EmbedBacklog = 3 (the stale block of the embeddable type and the two of
// the never-embedded one). GREEN after: 1, the set the pick can return.
func TestStatusDBEmbedBacklogStaleNeverEmbedded(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const scope = "private"
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = float32(i%2) + 0.5
	}
	never, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
		Name: "bl-never", Scope: store.GlobalScope, DisplayName: "bl-never",
		Config: []byte(`{"v":1,"retrieval":{"policy":"full-pass"}}`),
	}, nil, "bl-test")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, typ := range []string{"knowledge", "bl-never", "bl-never"} {
		b, err := store.UpsertBlock(ctx, pool, "learnings", "bl stale "+string(rune('a'+i)), "text", nil, nil, scope, true, store.SensitivityWrite{}, typ)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "test-embed", vec); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	// The type becomes never-embedded while its blocks hold a vector.
	if _, err := store.UpdateBlockType(ctx, pool, never.ID, []string{store.GlobalScope},
		store.BlockTypeUpdate{Config: []byte(`{"v":1,"retrieval":{"policy":"excluded"}}`)}, nil, "bl-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// The state a content change left before issue #213: stale, vector kept.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding_stale = true WHERE id = ANY($1::uuid[])`, ids); err != nil {
		t.Fatal(err)
	}
	var stale, marked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE embedding_stale AND embedding IS NOT NULL), count(*) FILTER (WHERE embed_excluded)
		   FROM context_blocks WHERE id = ANY($1::uuid[])`, ids).Scan(&stale, &marked); err != nil {
		t.Fatal(err)
	}
	if stale != 3 || marked != 0 {
		t.Fatalf("fixture: %d stale with a vector (want 3), %d marked (want 0)", stale, marked)
	}

	// The pick returns the embeddable block only.
	var sens string
	if err := pool.QueryRow(ctx, `SELECT sensitivity FROM context_blocks WHERE id = $1::uuid`, ids[0]).Scan(&sens); err != nil {
		t.Fatal(err)
	}
	cl := store.NewPendingClasses()
	cl.PairScopes, cl.PairLevels = []string{scope}, []string{sens}
	picked := []string{}
	for range 3 {
		var id string
		if err := pool.QueryRow(ctx, store.PendingPickSQL("id", true, store.EmbedFailureExcludedPredicate),
			append(cl.Args(), picked)...).Scan(&id); err != nil {
			break
		}
		picked = append(picked, id)
	}
	if len(picked) != 1 || picked[0] != ids[0] {
		t.Fatalf("fixture: the pick returns %v, want only %s", picked, ids[0])
	}

	got := queryEmbedBacklog(ctx, pool)
	if got == nil {
		t.Fatal("EmbedBacklog = null")
	}
	if *got != 1 {
		t.Errorf("EmbedBacklog = %d, want 1 (the stale block of the embeddable type; the pick never returns the two of the never-embedded type)", *got)
	}
}
