//go:build integration

// Issue #370, point 3, and the last hand-over of #365 in issue #366: the
// check-then-act window of the runtime index DDL. runtimeIndexDDLHook lets
// another session act at a defined point between a builder's row check and
// its DDL — a window of microseconds that no timing hits on purpose.
//
//	go test -tags=integration ./internal/events/ -run 'TestEmbedRuntimeIndexWindow' -count=1 -v
package events

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/testdb"
)

// setRuntimeIndexHook installs fn as runtimeIndexDDLHook for one subtest.
func setRuntimeIndexHook(t *testing.T, fn func(stage, name string)) {
	t.Helper()
	runtimeIndexDDLHook = fn
	t.Cleanup(func() { runtimeIndexDDLHook = nil })
}

func TestEmbedRuntimeIndexWindow_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// The repair checked the row and read the catalog without the lock; then
	// another session finished the index and the operator's confirm committed
	// (hook: before the lock). The repair builds no _next index on the fresh
	// pair the swap left.
	t.Run("repair_after_a_confirm_between_its_check_and_its_ddl", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		s, _ := probeScheduler(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		var cerr error
		fired := false
		setRuntimeIndexHook(t, func(stage, name string) {
			if stage != runtimeIndexStageUnlocked || name != "idx_embedding_pending_next" || fired {
				return
			}
			fired = true
			createPlainIndexT(t, pool, "idx_embedding_pending_next")
			_, cerr = ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID)
		})
		if err := s.runEmbedIndexRepair(ctx, mig); err != nil {
			t.Fatalf("repair: %v", err)
		}
		if !fired || cerr != nil {
			t.Fatalf("hook fired=%t confirm=%v, want a confirm that swapped", fired, cerr)
		}
		if st := readVerifyRow(t, pool, migID); st.Status != "done" {
			t.Fatalf("status %q, want done", st.Status)
		}
		if oid := indexOIDT(t, pool, "idx_embedding_pending_next"); oid != 0 {
			t.Fatalf("idx_embedding_pending_next stands after the swap (oid %d): an index on the fresh embedding_next pair", oid)
		}
	})

	// The repair holds the runtime index lock and is about to run its DDL
	// (hook: under the lock); another session finished the index meanwhile and
	// the operator confirms. The confirm is refused while the lock is held; the
	// repair's DDL then runs on the column the verdict covers, and the next
	// confirm swaps.
	t.Run("confirm_while_a_builder_holds_the_lock_before_its_ddl", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		s, _ := probeScheduler(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		var cerr error
		var during verifyRowState
		fired := false
		setRuntimeIndexHook(t, func(stage, name string) {
			if stage != runtimeIndexStageLocked || name != "idx_embedding_pending_next" || fired {
				return
			}
			fired = true
			createPlainIndexT(t, pool, "idx_embedding_pending_next")
			_, cerr = ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID)
			during = readVerifyRow(t, pool, migID)
		})
		if err := s.runEmbedIndexRepair(ctx, mig); err != nil {
			t.Fatalf("repair: %v", err)
		}
		if !fired || cerr == nil || during.Status != "verifying" || !during.VerifyStartedAt.Equal(wm) {
			t.Fatalf("hook fired=%t confirm=%v status=%q, want the confirm refused while the builder holds the lock",
				fired, cerr, during.Status)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm after the repair: %v", err)
		}
		if oid := indexOIDT(t, pool, "idx_embedding_pending_next"); oid != 0 {
			t.Fatalf("idx_embedding_pending_next stands after the swap (oid %d): an index on the fresh embedding_next pair", oid)
		}
	})

	// The migration cycle read the row in verifying; the pending index is
	// missing; the confirm commits after the lock-free read (hook: before the
	// lock). The cycle builds no pending index on the swapped columns, where
	// its predicate matches every live row.
	t.Run("cycle_after_a_confirm_builds_no_pending_index", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, migrationPendingIndexName)
		s, _ := probeScheduler(t, pool)
		var cerr error
		fired := false
		setRuntimeIndexHook(t, func(stage, name string) {
			if stage != runtimeIndexStageUnlocked || name != migrationPendingIndexName || fired {
				return
			}
			fired = true
			_, cerr = ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID)
		})
		if err := miCycle(t, s, verifyTestCfg()); err != nil {
			t.Fatalf("cycle: %v", err)
		}
		if !fired || cerr != nil {
			t.Fatalf("hook fired=%t confirm=%v, want a confirm that swapped", fired, cerr)
		}
		if oid := indexOIDT(t, pool, migrationPendingIndexName); oid != 0 {
			t.Fatalf("%s stands after the swap (oid %d): built on the swapped columns", migrationPendingIndexName, oid)
		}
	})

	// From #365, the mutant that survived: between the lock-free read and the
	// lock another session finished the index (hook: before the lock). The
	// re-read under the lock reuses it — it is neither dropped nor rebuilt.
	t.Run("index_finished_before_the_lock_is_reused", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		s := newIndexTestScheduler(pool)
		const name = "idx_dream_pending_next"
		var oid, node int64
		setRuntimeIndexHook(t, func(stage, n string) {
			if stage != runtimeIndexStageUnlocked || n != name || oid != 0 {
				return
			}
			createPlainIndexT(t, pool, name)
			oid, node = indexIdentityT(t, pool, name)
		})
		action, err := s.ensureConcurrentIndexValid(ctx, name, ddlOfManaged(name), nil)
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
		gotOID, gotNode := indexIdentityT(t, pool, name)
		if oid == 0 || action != indexActionReused || gotOID != oid || gotNode != node {
			t.Fatalf("action %q, identity %d/%d → %d/%d, want the finished index reused", action, oid, node, gotOID, gotNode)
		}
	})
}
