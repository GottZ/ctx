//go:build integration

// Issue #547, review round 1 (F6): the prune takes the supersedes locks of
// the tenant's scopes in the order every multi-scope holder takes them —
// Go's byte order (slices.Sort), not the database's: TenantScopes returns
// them ORDER BY scope, in the collation of the database, which on the
// production image is not byte order ("zoa" before "zoB"). A prune in that
// order and a block batch over the same two scopes cross: 40P01 in 10 of 10
// rounds in the review. The prune is held at its second scope lock until the
// batch waits; then it goes on. On a database whose collation is byte order
// (the CI image) no pair differs and the test has nothing to show.
//
//	CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18 go test -tags=integration -p 1 ./internal/store/ -run 'TestTenantPruneLocksScopesInByteOrder' -v
package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

func TestTenantPruneLocksScopesInByteOrder(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	var x, y string
	for _, p := range [][2]string{{"poB", "poa"}, {"po-z", "po0"}, {"po.b", "poa1"}} {
		var dbFirst string
		if err := f.pool.QueryRow(ctx, `SELECT s FROM unnest($1::text[]) s ORDER BY s LIMIT 1`, []string{p[0], p[1]}).Scan(&dbFirst); err != nil {
			t.Fatal(err)
		}
		if dbFirst != slices.Min(p[:]) {
			x, y = p[0], p[1]
			break
		}
	}
	if x == "" {
		t.Skip("the database's collation orders these scope names as bytes do: no pair to cross")
	}
	for r := range 3 {
		sx, sy := fmt.Sprintf("%s%d", x, r), fmt.Sprintf("%s%d", y, r)
		tenantID := f.tenant(t, fmt.Sprintf("po-%d", r), sx, sy)
		f.block(t, sx, fmt.Sprintf("po x %d", r))
		f.block(t, sy, fmt.Sprintf("po y %d", r))

		var locks atomic.Int64
		var once sync.Once
		batchDone := make(chan struct{})
		var batchErr error
		tr := &p547Tracer{}
		tr.before = func(pid uint32, sql string, _ []any) {
			if !strings.Contains(sql, "ctx:supersedes:") || locks.Add(1) != 2 {
				return
			}
			once.Do(func() {
				go func() {
					defer close(batchDone)
					_, batchErr = store.UpsertBlocks(ctx, f.pool, []store.BatchUpsert{
						{Category: "learnings", Title: "po w x", Content: "w", Scope: sx, ScopeExplicit: true},
						{Category: "learnings", Title: "po w y", Content: "w", Scope: sy, ScopeExplicit: true},
					}, "", f.set, nil)
				}()
				p547WaitOn(t, f.pool, pid, batchDone)
			})
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
		pruneErr := store.PruneTenant(ctx, prunePool, tenantID, store.AuditStamp{})
		<-batchDone
		prunePool.Close()
		if pruneErr != nil || batchErr != nil || tr.deadlocks.Load() != 0 {
			t.Fatalf("round %d over %q, %q: prune %v, batch %v, 40P01 in the prune %d — want both through",
				r, sx, sy, pruneErr, batchErr, tr.deadlocks.Load())
		}
	}
}
