//go:build integration

// Wave B3 (Gap-C0-c), store side: DeleteBlob never checked its scope argument
// for emptiness.
//
// Ist-Stand: `DELETE FROM context_blobs WHERE id = $1 AND scope = $2` with an
// EMPTY $2 matches nothing, pgx.ErrNoRows collapses to (nil, nil), and the
// handler renders that as the ordinary "Blob not found" — a caller whose scope
// resolution produced nothing at all is answered as if the blob were simply
// absent. That is the exact silent-empty-scope shape RequireScopes/ErrNoScopes
// exist to forbid on every other scope-gated store path (design/01 §5.4); the
// blob delete was the one that never got the guard.
//
// B3 gives it the same fail-closed sentinel. The SCOPE BINDING itself stays
// untouched — delete remains pinned to home_scope; widening it to
// writableBlockScopes is deliberately NOT this wave (§8-E3).
//
//	d  EmptyScopeFailsClosed — DeleteBlob(..., "") ⇒ ErrNoScopes, row survives
//	   Golden                — home_scope deletes, a foreign scope is not-found
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestDeleteBlobScope -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestDeleteBlobScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seed := func(t *testing.T, title, scope string) string {
		t.Helper()
		bm, err := store.UpsertBlob(ctx, pool, "reference", title, title+".bin",
			"application/octet-stream", scope, []byte("b3-delete"), nil, nil)
		if err != nil {
			t.Fatalf("seed blob %q in %q: %v", title, scope, err)
		}
		return bm.ID
	}
	exists := func(t *testing.T, pool *pgxpool.Pool, id string) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blobs WHERE id = $1::uuid`, id).Scan(&n); err != nil {
			t.Fatalf("count blob %s: %v", id, err)
		}
		return n > 0
	}

	t.Run("d_EmptyScopeFailsClosed", func(t *testing.T) {
		id := seed(t, "b3-del-empty", "b3del")

		// RED pre-B3: the empty scope is handed straight to the DELETE, which
		// matches zero rows ⇒ (nil, nil) — a silent not-found where an empty
		// scope set must be an ERROR.
		bm, err := store.DeleteBlob(ctx, pool, id, "")
		if !errors.Is(err, store.ErrNoScopes) {
			t.Fatalf("DeleteBlob(id, \"\") err = %v, want store.ErrNoScopes (fail-closed, design/01 §5.4)", err)
		}
		if bm != nil {
			t.Errorf("DeleteBlob(id, \"\") returned %v, want nil alongside the error", bm)
		}
		if !exists(t, pool, id) {
			t.Fatalf("the blob was deleted under an EMPTY scope — the guard must reject, not widen")
		}
	})

	t.Run("golden_ScopeBindingUnchanged", func(t *testing.T) {
		// A foreign, NON-empty scope keeps its old contract: not-found, no
		// error, row intact. This is what separates the new guard from a fix
		// that turned every scope miss into an error.
		id := seed(t, "b3-del-foreign", "b3del")
		bm, err := store.DeleteBlob(ctx, pool, id, "b3other")
		if err != nil {
			t.Fatalf("DeleteBlob(id, foreign scope) err = %v, want nil (unchanged not-found contract)", err)
		}
		if bm != nil {
			t.Errorf("DeleteBlob(id, foreign scope) returned %v, want nil", bm)
		}
		if !exists(t, pool, id) {
			t.Fatalf("a foreign scope deleted the blob — the home_scope binding is gone")
		}

		// The owning scope still deletes and still returns the row.
		bm, err = store.DeleteBlob(ctx, pool, id, "b3del")
		if err != nil {
			t.Fatalf("DeleteBlob(id, home scope) err = %v, want nil", err)
		}
		if bm == nil {
			t.Fatalf("DeleteBlob(id, home scope) returned nil, want the deleted row")
		}
		if bm.ID != id || bm.Scope != "b3del" {
			t.Errorf("deleted row = (%s,%s), want (%s,b3del)", bm.ID, bm.Scope, id)
		}
		if exists(t, pool, id) {
			t.Fatalf("the blob survived a delete in its own scope")
		}
	})
}
