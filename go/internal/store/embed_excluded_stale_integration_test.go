//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestEmbedExcludedContentChangeDropsVector: a block of a type that is never
// embedded but still holds a vector (embedded before its type became
// excluded, or before migration 176 for a tenant-own type) loses the vector
// on a content change and carries the marker, instead of being marked stale:
// a stale block of such a type sat in the stale-pick indexes of migration 173
// for good, and every pick of its class filtered it row by row. A block of an
// embeddable type keeps its vector, marked stale, as before.
func TestEmbedExcludedContentChangeDropsVector(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "st-a", "st-a")
	excl := storeTyped(t, pool, "st-a", "st own", "st-x")
	plain := storeTyped(t, pool, "st-a", "st plain", "")
	cp := storeTyped(t, pool, "st-a", "st checkpoint", "checkpoint")
	for _, id := range []string{excl.ID, plain.ID, cp.ID} {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	createType(t, pool, "st-x", "st-a", "excluded")
	reconcile(t, pool)

	for _, c := range []struct {
		b           *store.Block
		typ         string
		wantVector  bool
		wantStale   bool
		wantMarker  bool
		description string
	}{
		{excl, "st-x", false, false, true, "tenant-own excluded type"},
		{cp, "checkpoint", false, false, true, "_global excluded type"},
		{plain, "", true, true, false, "embeddable type"},
	} {
		if _, err := store.UpsertBlock(ctx, pool, "ee-cat", c.b.Title, "changed body of "+c.b.Title, nil, nil, "st-a", true, store.SensitivityWrite{}, c.typ); err != nil {
			t.Fatalf("content change of %s: %v", c.description, err)
		}
		var hasVec, stale, marked bool
		if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL, embedding_stale, embed_excluded FROM context_blocks WHERE id = $1`, c.b.ID).
			Scan(&hasVec, &stale, &marked); err != nil {
			t.Fatal(err)
		}
		if hasVec != c.wantVector || stale != c.wantStale || marked != c.wantMarker {
			t.Errorf("%s after a content change: vector %v, stale %v, marker %v; want %v, %v, %v",
				c.description, hasVec, stale, marked, c.wantVector, c.wantStale, c.wantMarker)
		}
	}
	if got := eePick(t, pool, "st-a", excl.Sensitivity); got != plain.ID {
		t.Errorf("pick after the content changes returned %q, want the embeddable block %s", got, plain.ID)
	}
}
