//go:build integration

// Issue #555, point 2: a second tenant-delete of the same tenant beside the
// first. Both read the tenant's scope names at their start; once the first
// has deleted the tenant row, a tenant created with the same slug can be
// assigned one of those names and write there — and the second delete's
// statements, keyed by the names it read, deleted what that tenant wrote
// and answered success. Every scope-keyed statement of the prune now names
// only the scopes the pruned tenant still owns (context_tenant_scopes), so
// the second delete takes nothing of the next owner and answers that the
// tenant is gone.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func TestSecondTenantDeleteLeavesTheNextOwnersRows(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "twice", 2)
	for _, s := range scopes {
		f.block(t, s, "twice seed "+s)
	}
	if _, err := store.UpsertBlob(ctx, f.pool, "files", "twice blob", "f.txt", "text/plain", scopes[0], []byte("x"), nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	// The second delete stands right before its first scope lock: it has
	// read the tenant's scope names and holds nothing.
	h := newPruneHold(func(sql string) bool {
		return strings.Contains(sql, "pg_advisory_xact_lock(hashtext('ctx:supersedes:'")
	})
	second := make(chan error, 1)
	go func() { second <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, second)

	if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
		close(h.release)
		<-second
		t.Fatalf("first delete: %v", err)
	}
	// The next tenant with the slug takes a scope name of the deleted one
	// and writes there: a block, a blob, a secret.
	next, err := store.CreateTenant(ctx, f.pool, "twice", "twice")
	if err != nil {
		t.Fatalf("tenant with the deleted one's slug: %v", err)
	}
	if _, err := store.AssignTenantScope(ctx, f.pool, next.ID, scopes[0], nil); err != nil {
		t.Fatalf("assign %s to the next tenant: %v", scopes[0], err)
	}
	f.block(t, scopes[0], "the next owner's block")
	if _, err := store.UpsertBlob(ctx, f.pool, "files", "next blob", "n.txt", "text/plain", scopes[0], []byte("y"), nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	close(h.release)
	var serr error
	select {
	case serr = <-second:
	case <-time.After(30 * time.Second):
		t.Fatal("the second delete did not end")
	}
	blocks, _, other, _ := pruneLeftovers(t, f.pool, next.ID, scopes[:1])
	t.Logf("second delete: %v; the next owner's rows in %s after it: %d blocks, %d other rows", serr, scopes[0], blocks, other)
	if blocks != 1 || other != 1 {
		t.Errorf("the second delete of a deleted tenant took the next owner's rows: %d blocks and %d other rows left of 1 and 1", blocks, other)
	}
	if !errors.Is(serr, store.ErrTenantNotFound) {
		t.Errorf("the second delete, after the first had deleted the tenant: %v, want ErrTenantNotFound", serr)
	}
}

// TestTenantPruneBacklogDrainedInTheLastRound (point 3): what was written after
// its scope's pass is a backlog of the closing rounds, which delete up to
// pruneBatchSize (2 000) rows per table and round. A backlog of exactly ten
// rounds of blocks drains in the tenth: before, that round's rows made the
// delete fail with "rows kept arriving … (2000 in the last)" although none
// were left; now the delete counts what is left first and finishes.
func TestTenantPruneBacklogDrainedInTheLastRound(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "backlog", 1)
	f.block(t, scopes[0], "backlog seed")
	h := newPruneHold(func(sql string) bool { return strings.Contains(sql, "DELETE FROM context_api_keys") })
	done := make(chan error, 1)
	go func() { done <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, done)
	const backlog = 10 * 2000 // ten closing rounds of blocks, pruneBatchSize each
	for n := 0; n < backlog; n += 1000 {
		items := make([]store.BatchUpsert, 1000)
		for i := range items {
			items[i] = store.BatchUpsert{Category: "learnings", Title: fmt.Sprintf("backlog %d", n+i), Content: "written after the scope's pass",
				Scope: scopes[0], ScopeExplicit: true}
		}
		if _, err := store.UpsertBlocks(ctx, f.pool, items, "", f.set, nil); err != nil {
			t.Fatalf("backlog: %v", err)
		}
	}
	close(h.release)
	err := <-done
	blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
	t.Logf("delete after a backlog of %d blocks: %v; left %d blocks, %d links, %d other, tenant row %d", backlog, err, blocks, links, other, tenant)
	if err != nil {
		t.Fatalf("a backlog the closing rounds drained: %v", err)
	}
	if blocks+links+other+tenant != 0 {
		t.Fatalf("left after success: %d blocks, %d links, %d other, tenant row %d", blocks, links, other, tenant)
	}
}
