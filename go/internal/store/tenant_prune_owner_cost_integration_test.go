//go:build integration

// Issue #555, points 1 and 2 (a measurement): what the owner test of the
// prune's statements costs on a tenant of 100 000 blocks, and the plans of
// the block pick and a drain with it, under both random_page_cost values a
// deployment plans with. Opt-in:
//
//	CTX_P555_PRUNE_COST=1 go test -tags=integration -p 1 ./internal/store/ -run TestPruneOwnerTestCost -v -timeout 30m
package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestPruneOwnerTestCost(t *testing.T) {
	if os.Getenv("CTX_P555_PRUNE_COST") == "" {
		t.Skip("measurement: set CTX_P555_PRUNE_COST=1")
	}
	f := newP547Fixture(t)
	ctx := context.Background()
	const blocks = 100_000
	tenantID, scopes, key := pruneWritersTenant(t, f, "cost100k", 1)
	start := time.Now()
	for n := 0; n < blocks; n += 1000 {
		items := make([]store.BatchUpsert, 1000)
		for i := range items {
			items[i] = store.BatchUpsert{Category: "learnings", Title: fmt.Sprintf("cost %d", n+i), Content: fmt.Sprintf("block %d of the cost tenant", n+i),
				Scope: scopes[0], ScopeExplicit: true}
		}
		if _, err := store.UpsertBlocks(ctx, f.pool, items, "", f.set, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d blocks in %s", blocks, time.Since(start).Round(time.Second))

	const pick = `SELECT id::text FROM context_blocks WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $3::uuid) LIMIT $2 FOR UPDATE`
	const pickBase = `SELECT id::text FROM context_blocks WHERE scope = ANY($1::text[]) LIMIT $2 FOR UPDATE`
	for _, rpc := range testdb.RandomPageCosts {
		p := testdb.WithRandomPageCost(t, f.pool, rpc)
		for name, sql := range map[string]string{"pick with the owner test": pick, "pick of the base": pickBase} {
			args := []any{scopes, 2000}
			if strings.Contains(sql, "$3") {
				args = append(args, tenantID)
			}
			tx, err := p.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) `+sql, args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			for rows.Next() {
				var l string
				_ = rows.Scan(&l)
				plan = append(plan, l)
			}
			rows.Close()
			_ = tx.Rollback(ctx)
			t.Logf("random_page_cost %s, %s:\n%s", rpc, name, strings.Join(plan, "\n"))
		}
	}

	stop := make(chan struct{})
	w := &requestKeyWriters{}
	w.start(t, f, key, scopes, stop)
	start = time.Now()
	perr := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{})
	took := time.Since(start)
	close(stop)
	w.wg.Wait()
	if perr != nil {
		t.Fatalf("prune: %v", perr)
	}
	b, l, o, tn := pruneLeftovers(t, f.pool, tenantID, scopes)
	t.Logf("prune of %d blocks beside request writers of the tenant's key: %s; writes through %d, refused %d; left %d blocks, %d links, %d other, tenant row %d",
		blocks, took.Round(time.Millisecond), w.written.Load(), w.refused.Load(), b, l, o, tn)
	if b+l+o+tn != 0 {
		t.Fatal("rows left after the delete answered success")
	}
}
