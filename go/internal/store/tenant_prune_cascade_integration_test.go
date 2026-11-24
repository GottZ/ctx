//go:build integration

// Review of #547: the prune's block batch of scope A deletes, through the
// parent_id cascade, a child that a scope move put into scope B of the same
// tenant. A dream batch of B that writes a link to that child while the
// prune runs must neither make the block delete fail (23503) nor meet it in a
// lock cycle (40P01): the batch holds the locks of all the tenant's scopes.
// The dream batch is started at the prune's first block batch, after its link
// delete and right before its block delete.
package store_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
)

func TestTenantPruneCascadesAChildOfAnotherScope(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	a, b := "p547c-a", "p547c-b"
	tenantID := f.tenant(t, "p547c", a, b)
	parent := f.block(t, a, "p547c parent")
	child := f.c547Moved(t, a, b, "p547c child", parent) // in B, its parent in A
	other := f.block(t, b, "p547c other")

	var blockStatements atomic.Int64
	dreamDone := make(chan struct{})
	var dreamErr error
	tr := &p547Tracer{}
	tr.before = func(pid uint32, sql string, _ []any) {
		if !strings.Contains(sql, "DELETE FROM context_blocks WHERE ctid") {
			return
		}
		if blockStatements.Add(1) != 1 { // the first block batch, after its link delete
			return
		}
		go func() {
			defer close(dreamDone)
			_, dreamErr = dream.WriteLinks(ctx, f.pool, f.set, child, b, 1.0,
				[]dream.Link{{TargetID: other, Relationship: "topical", Confidence: 0.9}})
		}()
		p547WaitOn(t, f.pool, pid, dreamDone)
	}
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tr
	prunePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer prunePool.Close()
	pruneErr := store.PruneTenant(ctx, prunePool, tenantID, store.AuditStamp{})
	<-dreamDone
	if pruneErr != nil || tr.deadlocks.Load() != 0 {
		t.Fatalf("prune beside a dream batch of the cascaded child's scope = %v (40P01 %d), want through", pruneErr, tr.deadlocks.Load())
	}
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1)`, []string{a, b}).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d blocks of the pruned tenant left (%v)", left, err)
	}
	t.Logf("the dream batch after the prune: %v", dreamErr)
}
