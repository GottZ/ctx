//go:build integration

// Issue #198, part B, the duplicate guard: a title or content change keeps
// the previous vector until the backfill writes the new one (marked stale).
// Such a vector stands for text its block no longer holds, so the guard must
// not judge by it in either role:
//
//   - as the block under check: the change dropped guard_checked_at, so the
//     block is pending for the guard, and checking it now would compare the
//     new text by the old text's vector and write that verdict as final. It
//     waits until its new vector is written (guardPendingWhere).
//   - as a candidate: a new block close to the previous text would become a
//     near_duplicate of a block that no longer holds that text and be
//     archived (ctx_guard_check, migration 173).
//
// Before part B the change nulled the vector, which kept the block out of
// both roles; this test pins that the stale marker does the same.
//
//	go test -tags=integration ./internal/guard/ -run TestGuardStale -count=1 -v
package guard_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGuardStale_NeitherCheckedNorMatched(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"
	strptr := func(s string) *string { return &s }
	scopes := []string{scope}

	type guardState struct {
		archived bool
		checked  bool
		decision string
	}
	read := func(t *testing.T, id string) guardState {
		t.Helper()
		var s guardState
		if err := pool.QueryRow(ctx,
			`SELECT is_archived, metadata ? 'guard_checked_at', COALESCE(metadata->>'guard_status', '')
			   FROM context_blocks WHERE id = $1::uuid`, id).Scan(&s.archived, &s.checked, &s.decision); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return s
	}
	batch := func(t *testing.T, want int) {
		t.Helper()
		n, err := guard.RunGuardBatch(ctx, pool, gitSet(), 50, guard.SearchIndexForTest)
		if err != nil || n != want {
			t.Fatalf("guard batch: processed %d, err %v; want %d", n, err, want)
		}
	}

	// X holds text A with vector a and is checked.
	x, err := store.UpsertBlock(ctx, pool, "learnings", "gs198 x", "text A", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, pool, x.ID, "test-embed", unitVec1024(0, 1)); err != nil {
		t.Fatal(err)
	}
	batch(t, 1)
	if s := read(t, x.ID); !s.checked || s.archived {
		t.Fatalf("precondition: X = %+v, want checked and live", s)
	}

	// X is rewritten to text B: its vector a is now stale, and the change
	// dropped guard_checked_at.
	if _, changed, err := store.UpdateBlock(ctx, pool, x.ID, store.UpdateBlockData{Content: strptr("text B")}, scopes); err != nil || !changed {
		t.Fatalf("UpdateBlock: changed=%v err=%v", changed, err)
	}
	// Y is new, with text close to A: its vector is a.
	y, err := store.UpsertBlock(ctx, pool, "learnings", "gs198 y", "text A again", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, pool, y.ID, "test-embed", unitVec1024(0, 1)); err != nil {
		t.Fatal(err)
	}

	// The batch checks Y alone: X waits for its new vector, and X's stale
	// vector is no candidate, so Y is clean and stays live.
	batch(t, 1)
	if s := read(t, y.ID); !s.checked || s.archived {
		t.Errorf("Y = %+v, want checked, clean and live (the only vector close to it is X's stale one)", s)
	}
	if s := read(t, x.ID); s.checked || s.archived {
		t.Errorf("X = %+v, want unchecked and live: it waits for the vector of text B", s)
	}

	// The backfill writes X's vector of text B (far from a): X is checked
	// now, by that vector.
	if err := store.StoreEmbedding(ctx, pool, x.ID, "test-embed", unitVec1024(1, 1)); err != nil {
		t.Fatal(err)
	}
	batch(t, 1)
	if s := read(t, x.ID); !s.checked || s.archived {
		t.Errorf("X after its new vector = %+v, want checked, clean and live", s)
	}
}
