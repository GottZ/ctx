//go:build integration

// Issue #281: an upsert whose pre-read finds no row for its identity, and whose
// INSERT … ON CONFLICT then takes the conflict branch because another writer's
// first create of that identity committed in between, must treat the write as
// what it is — an update of the other writer's row. When it replaces that
// row's content, the row's vector stands for text the row no longer holds, and
// the same invalidation as on every other content change has to run
// (InvalidateEmbeddingTx). Before the fix the decision came from the pre-read
// (`hadRow && oldContent != content`), so this write skipped it and left B's
// content next to A's vector, unmarked and out of the backfill's reach.
//
// The interleaving is forced, not hoped for: writer A's first create stays
// uncommitted while writer B runs. B's pre-read cannot see A's row (READ
// COMMITTED snapshot, and FOR UPDATE locks only rows it sees), so it finds
// none; B's INSERT then waits for A on the unique index. A writes its vector
// and commits, and B's statement takes the conflict branch on a row that
// carries a vector of A's content — the state of the issue's steps 2–4.
//
//	go test -tags=integration ./internal/store/ -run TestUpsertBlockFirstCreateRace -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestUpsertBlockFirstCreateRace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = 0.3
	}

	// race runs writer A's held-open first create and writer B's UpsertBlock
	// of the same identity with contentB, and returns the row afterwards:
	// whether it holds a serving vector, whether that vector is marked stale,
	// and its content.
	race := func(t *testing.T, title, contentA, contentB string) (hasVec, stale bool, content string) {
		t.Helper()
		const category, scope = "learnings", "i281"
		txA, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = txA.Rollback(ctx) }()
		a, err := store.UpsertBlockTxForTest(ctx, txA, category, title, contentA, scope)
		if err != nil {
			t.Fatalf("writer A first create: %v", err)
		}
		var xidA string
		if err := txA.QueryRow(ctx, `SELECT pg_current_xact_id()::text`).Scan(&xidA); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := store.UpsertBlock(ctx, pool, category, title, contentB, nil, nil, scope, false, store.SensitivityWrite{}, "")
			done <- err
		}()
		ended := func() bool { return len(done) > 0 }
		if !r5Waits(t, pool, xidA, ended) {
			t.Fatalf("writer B did not wait for writer A's uncommitted first create (ended=%v): the interleaving of #281 was not reached", ended())
		}

		// A's block is embedded before B's statement resumes.
		if err := store.StoreEmbedding(ctx, txA, a.ID, "model-a", vec); err != nil {
			t.Fatalf("writer A vector: %v", err)
		}
		if err := txA.Commit(ctx); err != nil {
			t.Fatalf("writer A commit: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("writer B upsert: %v", err)
		}

		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NOT NULL, embedding_stale, content FROM context_blocks WHERE id = $1::uuid`, a.ID,
		).Scan(&hasVec, &stale, &content); err != nil {
			t.Fatalf("read row: %v", err)
		}
		return hasVec, stale, content
	}

	t.Run("changed content marks the other writer's vector", func(t *testing.T) {
		hasVec, stale, content := race(t, "i281-changed", "content of writer A", "content of writer B")
		if content != "content of writer B" {
			t.Fatalf("content = %q, want writer B's content (B took the conflict branch)", content)
		}
		if hasVec && !stale {
			t.Errorf("the row holds writer B's content next to the vector of writer A's content, unmarked (embedding_stale=false): the upsert decided the invalidation from its pre-read, which saw no row")
		}
	})

	t.Run("identical content keeps the vector", func(t *testing.T) {
		// The same race with the same text on both sides: nothing the vector
		// stands for changed, so it must stay current — invalidating it would
		// send the block back to the backfill for nothing.
		hasVec, stale, content := race(t, "i281-identical", "one shared text", "one shared text")
		if content != "one shared text" {
			t.Fatalf("content = %q", content)
		}
		if !hasVec || stale {
			t.Errorf("identical content: has_vector=%v embedding_stale=%v, want the vector kept and current", hasVec, stale)
		}
	})
}
