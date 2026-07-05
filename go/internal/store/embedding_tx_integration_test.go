//go:build integration

// Integration test for Welle M-2 (W49 altlast): StoreEmbedding accepts an
// execQuerier (satisfied by both *pgxpool.Pool and pgx.Tx), so the scheduler
// backfill can run the embedding write INSIDE its FOR-UPDATE-SKIP-LOCKED tx
// instead of duplicating the raw UPDATE inline. The invariant that matters is
// atomicity: if the surrounding tx rolls back, the embedding write must roll
// back with it. This test proves that end-to-end against a migrated PG18.
//
// Run: go test -tags=integration ./internal/store/ -run TestStoreEmbeddingTx -count=1 -v
// (set CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18 to use the prod image).
package store_test

import (
	"context"
	"testing"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStoreEmbeddingTx_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seed := func(t *testing.T, title string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('issue', $1, 'body', 'm2-embtx') RETURNING id::text`, title).Scan(&id); err != nil {
			t.Fatalf("seed block %q: %v", title, err)
		}
		return id
	}
	embeddingIsNull := func(t *testing.T, id string) bool {
		t.Helper()
		var isNull bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL FROM context_blocks WHERE id = $1::uuid`, id).Scan(&isNull); err != nil {
			t.Fatalf("read embedding null-state %s: %v", id, err)
		}
		return isNull
	}

	// A distinctive vector: all 1024 dims = 0.25 so a positive-control read can
	// confirm the exact value round-tripped, not merely non-NULL.
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = 0.25
	}

	// (1) Rollback of the surrounding tx MUST roll back the embedding write.
	// This is the whole point of routing StoreEmbedding through the tx.
	t.Run("rollback_reverts_embedding_write", func(t *testing.T) {
		id := seed(t, "m2-rollback")
		if !embeddingIsNull(t, id) {
			t.Fatalf("precondition: fresh block should have NULL embedding")
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := store.StoreEmbedding(ctx, tx, id, vec); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("StoreEmbedding within tx: %v", err)
		}
		// Visible inside the tx before we decide to roll back.
		var nullInTx bool
		if err := tx.QueryRow(ctx,
			`SELECT embedding IS NULL FROM context_blocks WHERE id = $1::uuid`, id).Scan(&nullInTx); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("read within tx: %v", err)
		}
		if nullInTx {
			t.Fatalf("in-tx: embedding should be set after StoreEmbedding, got NULL")
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		// After rollback the embedding write is gone: still NULL.
		if !embeddingIsNull(t, id) {
			t.Fatalf("rollback did NOT revert the embedding write — embedding is non-NULL post-rollback (atomicity broken)")
		}
	})

	// (2) Positive control: committing the tx persists the exact vector. Guards
	// against a StoreEmbedding that silently no-ops (which would also "pass" the
	// rollback assertion above for the wrong reason).
	t.Run("commit_persists_embedding", func(t *testing.T) {
		id := seed(t, "m2-commit")

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := store.StoreEmbedding(ctx, tx, id, vec); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("StoreEmbedding within tx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		if embeddingIsNull(t, id) {
			t.Fatalf("commit did not persist embedding — still NULL")
		}
		var got pgvec.Vector
		if err := pool.QueryRow(ctx,
			`SELECT embedding FROM context_blocks WHERE id = $1::uuid`, id).Scan(&got); err != nil {
			t.Fatalf("read committed embedding: %v", err)
		}
		slice := got.Slice()
		if len(slice) != 1024 {
			t.Fatalf("committed embedding dim: got %d, want 1024", len(slice))
		}
		if slice[0] != 0.25 || slice[1023] != 0.25 {
			t.Fatalf("committed embedding value mismatch: got [%v ... %v], want 0.25", slice[0], slice[1023])
		}
	})
}
