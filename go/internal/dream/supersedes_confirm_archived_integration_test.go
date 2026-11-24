//go:build integration

// Issue #547 point 7b: the confirm of a supersedes guess runs the
// declaration's already-superseded gate when the link would settle its
// target (issue #284). An archived target has no lifecycle to settle — the
// reconcile passes it by — so a guess on it settles nothing either, and its
// confirm is not refused for another settler of the archived block: the
// confirm pins the link, as before #544.
package dream_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestSupersedesConfirmOfAnArchivedTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, set := raceSetup(t)
	ctx := context.Background()
	tgt := raceBlock(t, pool, "p7b note")
	s1 := raceDeclare(t, pool, set, "p7b note v2", tgt)
	s2 := raceBlock(t, pool, "p7b note v3")
	raceGuess(t, pool, set, s2, tgt, 0.9)
	if _, err := store.DeleteBlock(ctx, pool, tgt, []string{"private"}); err != nil {
		t.Fatalf("archive the target: %v", err)
	}
	res, err := store.DreamLinkResolve(ctx, pool, s2, tgt, "supersedes", "confirm", "", []string{"private"})
	if err != nil || res == nil || !res.Pinned {
		t.Errorf("confirm of a guess on an archived target settled by %s = (%+v, %v), want pinned", s1, res, err)
	}
}
