//go:build integration

// Supersedes reconcile — the paths and predicates the PR #44 suite left
// unpinned (review F5, each case proven necessary by a mutant the PR suite
// let through):
//
//	(a) CleanupDanglingLinks reconciles: an archived superseder's link is
//	    swept and its target returns to knowledge;
//	(b) an archived second superseder does not keep a target a snapshot;
//	(c) a superseder in another scope does not keep a target a snapshot;
//	(d) an existing pointer is kept although a re-write moves its link to
//	    the end of the (created_at, source id) order.
//
//	go test -tags=integration -p 1 ./internal/dream/ -run 'TestSupersedesReconcile_' -count=1 -v
package dream_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// (a) The production archive path (store.DeleteBlock) leaves the supersedes
// link in place — CleanupDanglingLinks is the step that sweeps it. The target
// returns to knowledge already at the archive (issue #197), and the sweep's
// reconcile keeps it there.
func TestSupersedesReconcile_CleanupAfterArchivedSuperseder_RestoresTarget(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedSupersedesFixture(t, pool)

	writeSupersedes(t, pool, icSourceID, 1.0)
	assertSnapshotState(t, pool, icTargetID, icSourceID)

	if _, err := store.DeleteBlock(ctx, pool, icSourceID, []string{"private"}); err != nil {
		t.Fatalf("archive superseder: %v", err)
	}
	// Since issue #197 the archive reconciles the target in its own
	// transaction: with no other live superseder it is knowledge at once, not
	// at the next daily sweep. The link stays until the sweep removes it.
	assertKnowledgeState(t, pool, icTargetID)

	if removed, err := dream.CleanupDanglingLinks(ctx, pool); err != nil || removed != 1 {
		t.Fatalf("cleanup: removed=%d err=%v", removed, err)
	}
	assertKnowledgeState(t, pool, icTargetID)
}

// (a') Same sweep with a second, live superseder: the target stays a snapshot
// and the pointer moves to the survivor.
func TestSupersedesReconcile_CleanupAfterArchivedSuperseder_MovesToSurvivor(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedSupersedesFixture(t, pool)

	writeSupersedes(t, pool, icSourceID, 1.0)
	writeSupersedes(t, pool, icOtherID, 1.0)
	assertSnapshotState(t, pool, icTargetID, icSourceID)

	if _, err := store.DeleteBlock(ctx, pool, icSourceID, []string{"private"}); err != nil {
		t.Fatalf("archive superseder: %v", err)
	}
	if removed, err := dream.CleanupDanglingLinks(ctx, pool); err != nil || removed != 1 {
		t.Fatalf("cleanup: removed=%d err=%v", removed, err)
	}
	assertSnapshotState(t, pool, icTargetID, icOtherID)
}

// (b) The second superseder is archived but its link not yet swept. When the
// pointer's own link goes, the archived one must not inherit the snapshot.
func TestSupersedesReconcile_ArchivedSecondSuperseder_DoesNotKeepSnapshot(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedSupersedesFixture(t, pool)

	writeSupersedes(t, pool, icSourceID, 1.0)
	writeSupersedes(t, pool, icOtherID, 1.0)
	if _, err := store.DeleteBlock(ctx, pool, icOtherID, []string{"private"}); err != nil {
		t.Fatalf("archive second superseder: %v", err)
	}

	reclassifyTopical(t, pool, icSourceID)
	assertKnowledgeState(t, pool, icTargetID)
}

// (c) The second superseder lives in another scope. Not via store.UpdateBlock:
// its scope move deletes every link whose far endpoint stays behind in the
// same transaction (sweepScopeMoveLinks, GD5/K8), so the O→T link would be
// gone and the scope predicate never reached. A link whose source scope
// diverges from its target's is the legacy/raw-injected row shape the K8
// audit names — reproduced here by a direct scope UPDATE without the sweep.
func TestSupersedesReconcile_ForeignScopeSuperseder_DoesNotKeepSnapshot(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedSupersedesFixture(t, pool)

	writeSupersedes(t, pool, icSourceID, 1.0)
	writeSupersedes(t, pool, icOtherID, 1.0)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET scope = 'hth' WHERE id = $1::uuid`, icOtherID); err != nil {
		t.Fatalf("move second superseder out of scope: %v", err)
	}

	reclassifyTopical(t, pool, icSourceID)
	assertKnowledgeState(t, pool, icTargetID)
}

// (d) A re-write of the pointer's own link resets its created_at to now()
// (ON CONFLICT DO UPDATE … created_at = now()), so the pointer's source
// becomes the LAST candidate in (created_at, source id) order. The existing
// pointer must be kept anyway — a reconcile that re-picked "the first valid
// candidate" would move it to the other superseder on every re-write.
func TestSupersedesReconcile_RewriteKeepsExistingPointer(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	seedSupersedesFixture(t, pool)

	writeSupersedes(t, pool, icSourceID, 1.0)
	assertSnapshotState(t, pool, icTargetID, icSourceID)
	writeSupersedes(t, pool, icOtherID, 1.0)
	assertSnapshotState(t, pool, icTargetID, icSourceID)
	writeSupersedes(t, pool, icSourceID, 1.0)
	assertSnapshotState(t, pool, icTargetID, icSourceID)
}
