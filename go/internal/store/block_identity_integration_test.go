//go:build integration

// Issue #227, store side: store.UpdateBlock turns the 23505 of
// uq_context_category_title_scope into *BlockIdentityTakenError and names the
// holder only within the caller's write scopes. The handler test
// (handler/update_identity_conflict_integration_test.go) covers the surfaces;
// this one pins the scope bound of the holder read, which no surface can reach
// because every surface refuses a move into a scope it may not write before the
// store runs.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestUpdateBlockIdentityTaken -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestUpdateBlockIdentityTaken(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	upsert := func(t *testing.T, category, title, scope string) *store.Block {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, category, title, "body of "+title, nil, nil, scope, true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("upsert %s/%s@%s: %v", category, title, scope, err)
		}
		return b
	}
	strptr := func(s string) *string { return &s }

	t.Run("holder in a write scope is named", func(t *testing.T) {
		holder := upsert(t, "i227-c", "same", "private")
		mover := upsert(t, "i227-b", "same", "private")
		_, _, err := store.UpdateBlock(ctx, pool, mover.ID, store.UpdateBlockData{Category: strptr("i227-c")}, []string{"private"})
		var taken *store.BlockIdentityTakenError
		if !errors.As(err, &taken) {
			t.Fatalf("err = %v, want *BlockIdentityTakenError", err)
		}
		if taken.ConflictID != holder.ID {
			t.Errorf("ConflictID = %q, want the holder %s", taken.ConflictID, holder.ID)
		}
	})

	t.Run("holder outside the write scopes is not named", func(t *testing.T) {
		// The store does not gate the target scope of a move (the surfaces
		// do); the holder read must still not name a block of a scope the
		// caller cannot write.
		upsert(t, "i227-x", "foreign", "elsewhere")
		mover := upsert(t, "i227-x", "foreign", "private")
		_, _, err := store.UpdateBlock(ctx, pool, mover.ID, store.UpdateBlockData{Scope: strptr("elsewhere")}, []string{"private"})
		var taken *store.BlockIdentityTakenError
		if !errors.As(err, &taken) {
			t.Fatalf("err = %v, want *BlockIdentityTakenError", err)
		}
		if taken.ConflictID != "" {
			t.Errorf("ConflictID = %q, want empty: the holder lies outside the write scopes", taken.ConflictID)
		}
	})
}
