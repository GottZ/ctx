//go:build integration

// Issue #547, review round 1 (F2): the prune of tenant A deletes nothing
// outside A's scopes and does not fail on rows of another tenant.
//
// A child whose parent is in A's scope and which lies in a scope of tenant B
// (a scope move keeps parent_id) went with its parent through the parent_id
// cascade, its own children in B with it (B: 3 → 1 blocks); a dream link of
// B onto that child made every prune run end with 23503, the tenant half
// pruned. The prune now detaches such a child (parent_id = NULL) under the
// supersedes lock of its scope before it deletes the parent — as a scope
// purge never removes a row of another scope (it refuses such a closure) and
// an archive cascades nothing. The chain reaches B through two blocks of the
// tenant's OTHER scope, so the batch that deletes the parent finds B's child
// only below the cascade's second level.
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
)

func TestTenantPruneLeavesAnotherTenantsChild(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "unlinked"
		if linked {
			name = "with a dream link of B onto the child"
		}
		t.Run(name, func(t *testing.T) {
			f := newP547Fixture(t)
			ctx := context.Background()
			a1, a2, b := "fc-a1", "fc-a2", "fc-b"
			pruned := f.tenant(t, "fc-a", a1, a2)
			f.tenant(t, "fc-b", b, "")
			parent := f.block(t, a1, "fc parent")
			// A chain inside A that crosses its scopes, then into B: parent (a1)
			// → mid (a2) → low (a2) → child (b) → grandchild (b).
			mid := f.c547Moved(t, a1, a2, "fc mid", parent)
			if err := f.c547Link(f.pool, a2, "fc low", mid); err != nil {
				t.Fatalf("low: %v", err)
			}
			var low string
			if err := f.pool.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE title = 'fc low'`).Scan(&low); err != nil {
				t.Fatal(err)
			}
			child := f.c547Moved(t, a2, b, "fc child", low)
			if err := f.c547Link(f.pool, b, "fc grandchild", child); err != nil {
				t.Fatalf("grandchild: %v", err)
			}
			own := f.block(t, b, "fc own")
			if linked {
				if n, err := dream.WriteLinks(ctx, f.pool, f.set, own, b, 1.0,
					[]dream.Link{{TargetID: child, Relationship: "topical", Confidence: 0.9}}); err != nil || n != 1 {
					t.Fatalf("dream link of B onto the child: %d, %v", n, err)
				}
			}
			before := f.c547Count(t, b)
			if err := store.PruneTenant(ctx, f.pool, pruned, store.AuditStamp{}); err != nil {
				t.Fatalf("prune of A: %v", err)
			}
			if after := f.c547Count(t, b); after != before {
				t.Errorf("tenant B's scope held %d blocks before the prune of A and %d after", before, after)
			}
			var parentOfChild *string
			if err := f.pool.QueryRow(ctx, `SELECT parent_id::text FROM context_blocks WHERE id = $1`, child).Scan(&parentOfChild); err != nil {
				t.Fatalf("the child in B: %v", err)
			}
			if parentOfChild != nil {
				t.Errorf("the child in B still names the pruned parent %s", *parentOfChild)
			}
			var leftA, links int
			if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_blocks WHERE scope = ANY($1::text[]))::int,
				(SELECT count(*) FROM context_dream_links WHERE scope = $2)::int`, []string{a1, a2}, b).Scan(&leftA, &links); err != nil {
				t.Fatal(err)
			}
			if leftA != 0 {
				t.Errorf("%d blocks of A left", leftA)
			}
			if linked && links != 1 {
				t.Errorf("B's dream link: %d, want it kept", links)
			}
		})
	}
}

// c547Count counts the live and archived blocks of a scope.
func (f *p547Fixture) c547Count(t *testing.T, scope string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM context_blocks WHERE scope = $1`, scope).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
