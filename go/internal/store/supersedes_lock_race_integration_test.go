//go:build integration

// Issue #197, review thread 1: archiving a block (DeleteBlock) or moving it to
// another scope (UpdateBlock) reconciles the targets of its supersedes links.
// The targets must be read AFTER the block's lock is held. Here a second
// transaction holds the block (as a store declaring a supersedes does: the
// declaration locks its source first), writes the declaration's link and
// target state, and commits while the archive or the move waits for that
// lock. A target list taken from the waiting statement's snapshot misses the
// new link, and the target stays a snapshot pointing at the archived or moved
// block.
//
// The declaration is written by hand on the second connection: the store
// function runs its own transaction and cannot be paused between its lock and
// its commit, which is the interleaving under test.
//
//	go test -tags=integration ./internal/store/ -run TestSupersedesSourceLockReadsTargetsAfterTheLock -count=20 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSupersedesSourceLockReadsTargetsAfterTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, op := range []string{"archive", "scope_move"} {
		t.Run(op, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			ctx := context.Background()
			mk := func(title string) string {
				b, err := store.UpsertBlock(ctx, pool, "race", title, title+" content", nil, nil, "private", true,
					store.SensitivityWrite{}, "")
				if err != nil {
					t.Fatalf("seed %s: %v", title, err)
				}
				return b.ID
			}
			src, tgt := mk("race-src"), mk("race-tgt")

			decl, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = decl.Rollback(ctx) }()
			for _, st := range []struct {
				sql  string
				args []any
			}{
				{`SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, []any{src}},
				{`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, pinned)
				  VALUES ($1::uuid, $2::uuid, 'supersedes', 1, 1, 'private', true)`, []any{src, tgt}},
				{`UPDATE context_blocks SET lifecycle_state = 'snapshot', superseded_by = $1::uuid WHERE id = $2::uuid`, []any{src, tgt}},
			} {
				if _, err := decl.Exec(ctx, st.sql, st.args...); err != nil {
					t.Fatalf("declaration: %v", err)
				}
			}

			done := make(chan error, 1)
			go func() {
				var err error
				if op == "archive" {
					_, err = store.DeleteBlock(ctx, pool, src, []string{"private"})
				} else {
					dest := "race-dest"
					_, _, err = store.UpdateBlock(ctx, pool, src, store.UpdateBlockData{Scope: &dest}, []string{"private", dest})
				}
				done <- err
			}()

			// Wait until the archive/move statement waits for the lock.
			deadline := time.Now().Add(15 * time.Second)
			for {
				var waiting int
				if err := pool.QueryRow(ctx,
					`SELECT count(*)::int FROM pg_stat_activity
					  WHERE wait_event_type = 'Lock' AND query LIKE '%FOR NO KEY UPDATE OF b%'`).Scan(&waiting); err != nil {
					t.Fatalf("pg_stat_activity: %v", err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the %s never waited for the source lock", op)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := decl.Commit(ctx); err != nil {
				t.Fatalf("commit declaration: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("%s did not finish", op)
			}

			var lifecycle string
			var by *string
			if err := pool.QueryRow(ctx,
				`SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id = $1::uuid`, tgt).Scan(&lifecycle, &by); err != nil {
				t.Fatalf("read target: %v", err)
			}
			if lifecycle != "knowledge" || by != nil {
				ptr := "<nil>"
				if by != nil {
					ptr = *by
				}
				t.Errorf("target after the %s = %s superseded_by %s, want knowledge without pointer", op, lifecycle, ptr)
			}
		})
	}
}
