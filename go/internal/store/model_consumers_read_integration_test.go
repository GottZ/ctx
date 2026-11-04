//go:build integration

// Gates of the W01-5 store additions (design/01 §4.6, design/05 §3.2):
//
//   - TestUpdateConsumerCheckedSeesTheLockedRow: the field-tier check of
//     model-consumer-update runs under the consumer's row lock on the value
//     the write replaces — a concurrent server-admin write that commits while
//     the update waits is what the check sees, not the value read before.
//     Red against a check fed from an unlocked read before the transaction.
//
//   - TestModelConsumerReadsAreOneStatement: GetConsumer and a filtered,
//     paged ListConsumers each send exactly one statement, and the status
//     filter is SQL text (no row is fetched to be dropped).
//
//     go test -tags=integration -p 1 -count=1 -run 'TestUpdateConsumerCheckedSeesTheLockedRow|TestModelConsumerReadsAreOneStatement' ./internal/store/ -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func TestUpdateConsumerCheckedSeesTheLockedRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-checked", nil)
	c, _, _ := mkCreate(ctx, t, e, tenant, "checked")
	if _, err := e.pool.Exec(ctx, `UPDATE context_model_consumers SET fair_weight = 5 WHERE id = $1::uuid`, c.ID); err != nil {
		t.Fatal(err)
	}

	// A server-admin write holds the row and lowers fair_weight to 1.
	hold, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `UPDATE context_model_consumers SET fair_weight = 1 WHERE id = $1::uuid`, c.ID); err != nil {
		t.Fatal(err)
	}

	// A tenant-admin re-submits fair_weight = 5: "unchanged" against the value
	// it read before (5), "raised" against the one it replaces (1).
	five := int16(5)
	seen := make(chan int16, 1)
	errRaise := errors.New("raise refused")
	done := make(chan error, 1)
	go func() {
		_, err := store.UpdateConsumerChecked(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant},
			store.ModelConsumerPatch{FairWeight: &five}, func(cur store.ModelConsumer) error {
				seen <- cur.FairWeight
				if five > cur.FairWeight {
					return errRaise
				}
				return nil
			}, e.stamp)
		done <- err
	}()
	mkAwaitLockWaiters(ctx, t, e.pool, 1)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errRaise) {
		t.Fatalf("UpdateConsumerChecked: %v, want the check's refusal", err)
	}
	if got := <-seen; got != 1 {
		t.Fatalf("the check saw fair_weight %d, want 1 — the committed value it replaces", got)
	}
	var fw int16
	if err := e.pool.QueryRow(ctx, `SELECT fair_weight FROM context_model_consumers WHERE id = $1::uuid`, c.ID).Scan(&fw); err != nil || fw != 1 {
		t.Fatalf("fair_weight after the refused update: %d (%v), want 1 unchanged", fw, err)
	}

	// Positive control: a check that passes lets the patch through.
	lower := int16(1)
	got, err := store.UpdateConsumerChecked(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant},
		store.ModelConsumerPatch{FairWeight: &lower, Label: strPtr("checked-2")}, func(store.ModelConsumer) error { return nil }, e.stamp)
	if err != nil || got.Label != "checked-2" {
		t.Fatalf("passing check: %+v %v", got, err)
	}
}

func strPtr(s string) *string { return &s }

func TestModelConsumerReadsAreOneStatement(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-reads", nil)
	var ids []string
	for _, l := range []string{"r-a", "r-b", "r-c"} {
		c, _, _ := mkCreate(ctx, t, e, tenant, l)
		ids = append(ids, c.ID)
	}
	tr := &mkTracer{}
	tp := mkTracedPool(t, e.pool, tr)
	if err := tp.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	access := store.ModelAccess{TenantID: tenant}
	tr.reset()
	c, err := store.GetConsumer(ctx, tp, ids[0], access, time.Time{})
	if err != nil || c.ID != ids[0] || c.TenantSlug != "mk-reads" || len(c.Keys) != 1 || c.CreatedByLabel == nil {
		t.Fatalf("GetConsumer: %+v %v", c, err)
	}
	if sqls, _ := tr.snapshot(); len(sqls) != 1 {
		t.Errorf("GetConsumer sent %d statements, want 1", len(sqls))
	}
	tr.reset()
	page, err := store.ListConsumers(ctx, tp, access, store.ListConsumersOptions{
		Statuses: []string{store.ConsumerStatusActive}, LabelPrefix: "r-", Limit: 2, Now: time.Now()})
	if err != nil || len(page) != 2 || page[0].ID != ids[2] {
		t.Fatalf("ListConsumers page: %d %v", len(page), err)
	}
	sqls, _ := tr.snapshot()
	if len(sqls) != 1 || !strings.Contains(sqls[0], "kk.live_n = 1") || !strings.Contains(sqls[0], "LIMIT") {
		t.Errorf("ListConsumers sent %d statement(s), want 1 carrying the status predicate and the LIMIT: %q", len(sqls), sqls)
	}
	if _, err := store.GetConsumer(ctx, e.pool, ids[0], store.ModelAccess{TenantID: mkTenant(ctx, t, e.pool, "mk-reads-other", nil)}, time.Time{}); !errors.Is(err, store.ErrModelConsumerNotFound) {
		t.Errorf("foreign GetConsumer: %v, want ErrModelConsumerNotFound", err)
	}
	if _, err := store.ListConsumers(ctx, e.pool, access, store.ListConsumersOptions{Statuses: []string{"bogus"}}); err == nil {
		t.Error("unknown status: accepted, want an error")
	}
}
