//go:build integration

// The two backfill pick indexes of migration 171 (issue #239) are optional
// anchors of the re-embed cutover (cutoverOptionalIndexes): migration 171
// builds them inline below 500 000 rows only, and a swap done before 171 left
// no _old anchors of them. A confirm without the canonical indexes still
// swaps, and their _next twins become canonical; rollback and cleanup of a
// swap without their _old anchors still run. The guard pick index of
// migration 182 (issue #360) is built the same way and is an optional
// anchor too.
//
//	go test -tags=integration ./internal/events/ -run TestEmbedCutoverPickIndexes -count=1 -v
package events

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/testdb"
)

var pickIndexes = []string{"idx_embedding_pending_scope_sens", "idx_embedding_pending_sens"}

// optionalAnchors are the canonical indexes of cutoverOptionalIndexes.
var optionalAnchors = append(append([]string(nil), pickIndexes...), "idx_guard_pending_type", "idx_embedding_pending_type_scope",
	"idx_blocks_stratify_covering")

func TestEmbedCutoverPickIndexes(t *testing.T) {
	ctx := context.Background()

	t.Run("confirm_without_the_canonical_pick_indexes", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID := seedConfirmReady(t, pool, 0)
		for _, ix := range optionalAnchors {
			if _, err := pool.Exec(ctx, `DROP INDEX `+ix); err != nil {
				t.Fatalf("drop %s: %v", ix, err)
			}
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if st := readMigrationRow(t, pool, migID).Status; st != string(embedmigration.StatusDone) {
			t.Fatalf("status %s, want done", st)
		}
		for _, ix := range optionalAnchors {
			if !indexExistsT(t, pool, ix) {
				t.Errorf("%s missing after the swap: its _next twin was not renamed", ix)
			}
			if indexExistsT(t, pool, ix+"_old") {
				t.Errorf("%s_old exists, but there was no canonical index to keep", ix)
			}
		}
	})

	t.Run("rollback_of_a_swap_without_the_old_pick_anchors", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID := seedConfirmReady(t, pool, 0)
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		for _, ix := range optionalAnchors {
			if _, err := pool.Exec(ctx, `DROP INDEX `+ix+`_old`); err != nil {
				t.Fatalf("drop %s_old: %v", ix, err)
			}
		}
		if _, err := RollbackEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID, "pick anchors absent"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if !columnExistsT(t, pool, "embedding") || columnExistsT(t, pool, "embedding_old") {
			t.Errorf("rollback did not restore the serving column")
		}
	})

	t.Run("cleanup_of_a_swap_without_the_old_pick_anchors", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID := seedConfirmReady(t, pool, 0)
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		for _, ix := range optionalAnchors {
			if _, err := pool.Exec(ctx, `DROP INDEX `+ix+`_old`); err != nil {
				t.Fatalf("drop %s_old: %v", ix, err)
			}
		}
		if err := CleanupEmbedMigration(ctx, pool, migID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if columnExistsT(t, pool, "embedding_old") {
			t.Errorf("embedding_old still there after cleanup")
		}
	})
}
