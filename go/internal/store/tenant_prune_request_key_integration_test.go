//go:build integration

// Issue #555, point 1: a request's write holds the key it authenticated with
// (store.WithRequestKey, holdRequestKey), so nothing it writes outlives a
// tenant-delete that answered success. The writers here are requests in
// flight for the whole delete: they authenticate once, before the delete's
// first step, and then write with that identity in their context, one write
// per millisecond, as a long MCP session or a web chat turn does. The
// production router's version of the same writers (every surface that puts a
// row into a scope) is cmd/ctxd's TestTenantDeleteBesideAWriteInFlight.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// requestKeyWriters runs writers of the key key in flight, until stop is
// closed: a single store, a block batch over two of the scopes (with the
// write budget, as the batch surfaces call it), and a blob, each once per
// millisecond. A write that fails with anything but ErrRequestKeyGone is an
// error of the test.
type requestKeyWriters struct {
	written, refused atomic.Int64
	wg               sync.WaitGroup
	failed           atomic.Value
}

func (w *requestKeyWriters) start(t *testing.T, f *p547Fixture, key string, scopes []string, stop <-chan struct{}) {
	t.Helper()
	ctx := context.Background()
	ar, err := auth.Authenticate(ctx, f.pool, key)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate before the delete: %+v, %v", ar, err)
	}
	rctx := store.WithRequestKey(ctx, ar.ApiKeyID)
	budget := &store.WriteBudget{APIKeyID: ar.ApiKeyID, Limit: 0}
	loop := func(name string, write func(i int) error) {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				switch err := write(i); {
				case err == nil:
					w.written.Add(1)
				case errors.Is(err, store.ErrRequestKeyGone):
					w.refused.Add(1)
				default:
					w.failed.CompareAndSwap(nil, fmt.Sprintf("%s: %v", name, err))
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	loop("single store", func(i int) error {
		_, err := store.UpsertBlock(rctx, f.pool, "learnings", fmt.Sprintf("rk store %d", i), "a request's block", nil, nil,
			scopes[i%len(scopes)], true, store.SensitivityWrite{}, "")
		return err
	})
	loop("block batch", func(i int) error {
		_, err := store.UpsertBlocks(rctx, f.pool, []store.BatchUpsert{
			{Category: "learnings", Title: fmt.Sprintf("rk batch a %d", i), Content: "a request's block", Scope: scopes[i%len(scopes)], ScopeExplicit: true},
			{Category: "learnings", Title: fmt.Sprintf("rk batch b %d", i), Content: "a request's block", Scope: scopes[(i+1)%len(scopes)], ScopeExplicit: true},
		}, ar.ApiKeyID, f.set, budget)
		return err
	})
	loop("blob", func(i int) error {
		_, err := store.UpsertBlob(rctx, f.pool, "files", fmt.Sprintf("rk blob %d", i), "f.txt", "text/plain",
			scopes[i%len(scopes)], []byte("a request's file"), nil, nil, "")
		return err
	})
}

// TestTenantPruneBesideRequestsInFlight: a tenant-delete beside requests of
// the tenant's key that authenticated before it and write once per
// millisecond over the whole delete, 50 runs: every run answers success and
// leaves no row in the tenant's scopes, and every write the writers made
// either went through (and was deleted with the tenant) or answered
// ErrRequestKeyGone.
func TestTenantPruneBesideRequestsInFlight(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	const runs = 50
	var written, refused int64
	for run := range runs {
		tenantID, scopes, key := pruneWritersTenant(t, f, fmt.Sprintf("rk%02d", run), 3)
		pruneWritersSeed(t, f, scopes, 300)
		stop := make(chan struct{})
		w := &requestKeyWriters{}
		w.start(t, f, key, scopes, stop)
		time.Sleep(20 * time.Millisecond)
		perr := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{})
		time.Sleep(20 * time.Millisecond) // writes after the delete answered
		close(stop)
		w.wg.Wait()
		if msg := w.failed.Load(); msg != nil {
			t.Fatalf("run %d: a writer failed: %v", run, msg)
		}
		if perr != nil {
			t.Fatalf("run %d: tenant-delete: %v", run, perr)
		}
		blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
		if blocks+links+other+tenant != 0 {
			t.Fatalf("run %d: after a tenant-delete that answered success: %d blocks, %d links, %d other rows, tenant row %d — want none",
				run, blocks, links, other, tenant)
		}
		if w.refused.Load() == 0 {
			t.Fatalf("run %d: no write was refused — the writers did not reach the delete's key step", run)
		}
		written += w.written.Load()
		refused += w.refused.Load()
	}
	t.Logf("%d runs: %d writes went through (and went with their tenant), %d were refused with ErrRequestKeyGone", runs, written, refused)
}

// TestRequestKeyHoldLockPairs: the key share every request write now takes
// first, against the paths it meets (issue #555): 200 rounds per pair, both
// sides started together, no lock cycle (40P01) and no other fault on either
// side.
//
//   - writers of two keys of a tenant (a block batch of one key, a single
//     store of the other on the same identities) × the tenant-delete, whose
//     key step deletes both keys one after the other;
//   - a writer × AssignTenantScope of its tenant (the tenant row FOR UPDATE);
//   - a block batch × a scope move of one of its scopes' blocks (both
//     requests of the same key).
func TestRequestKeyHoldLockPairs(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	const rounds = 200
	deadlock := func(err error) bool { return err != nil && pgxdb.SerializationConflict(err) }
	authed := func(t *testing.T, key string) (context.Context, string) {
		ar, err := auth.Authenticate(ctx, f.pool, key)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate: %v", err)
		}
		return store.WithRequestKey(ctx, ar.ApiKeyID), ar.ApiKeyID
	}
	both := func(a, b func() error) (error, error) {
		var wg sync.WaitGroup
		var ea, eb error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; ea = a() }()
		go func() { defer wg.Done(); <-start; eb = b() }()
		close(start)
		wg.Wait()
		return ea, eb
	}

	t.Run("writers of two keys × tenant-delete", func(t *testing.T) {
		var refusals int64
		for r := range rounds {
			tenantID, scopes, key1 := pruneWritersTenant(t, f, fmt.Sprintf("kp%03d", r), 2)
			_, key2, err := store.CreateApiKey(ctx, f.pool, "second key", scopes[0], scopes, tenantID)
			if err != nil {
				t.Fatal(err)
			}
			c1, id1 := authed(t, key1)
			c2, _ := authed(t, key2)
			f.block(t, scopes[0], "kp seed a")
			f.block(t, scopes[1], "kp seed b")
			// Both writers loop over the whole delete, on the same identities:
			// a batch of the first key (it takes the scope locks), a single
			// store of the second (no scope lock), so the delete's key step
			// meets both keys held.
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var failed atomic.Value
			var refused atomic.Int64
			loop := func(name string, write func(i int) error) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						if err := write(i); errors.Is(err, store.ErrRequestKeyGone) {
							refused.Add(1)
						} else if err != nil {
							failed.CompareAndSwap(nil, fmt.Sprintf("%s: %v (deadlock: %v)", name, err, deadlock(err)))
							return
						}
						time.Sleep(time.Millisecond) // one write per millisecond, as a request in flight writes
					}
				}()
			}
			loop("batch", func(i int) error {
				_, err := store.UpsertBlocks(c1, f.pool, []store.BatchUpsert{
					{Category: "learnings", Title: fmt.Sprintf("kp same %d", i%4), Content: fmt.Sprintf("batch %d", i), Scope: scopes[0], ScopeExplicit: true},
					{Category: "learnings", Title: fmt.Sprintf("kp other %d", i%4), Content: fmt.Sprintf("batch %d", i), Scope: scopes[1], ScopeExplicit: true},
				}, id1, f.set, &store.WriteBudget{APIKeyID: id1})
				return err
			})
			loop("single store", func(i int) error {
				_, err := store.UpsertBlock(c2, f.pool, "learnings", fmt.Sprintf("kp same %d", i%4), fmt.Sprintf("single %d", i), nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
				return err
			})
			perr := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{})
			close(stop)
			wg.Wait()
			if msg := failed.Load(); msg != nil {
				t.Fatalf("round %d: %v", r, msg)
			}
			if perr != nil {
				t.Fatalf("round %d: prune: %v (deadlock: %v)", r, perr, deadlock(perr))
			}
			if blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes); blocks+links+other+tenant != 0 {
				t.Fatalf("round %d: %d blocks, %d links, %d other rows, tenant %d left", r, blocks, links, other, tenant)
			}
			refusals += refused.Load()
		}
		t.Logf("%d rounds, %d writes refused after the key step", rounds, refusals)
	})

	t.Run("writer × AssignTenantScope", func(t *testing.T) {
		tenantID, scopes, key := pruneWritersTenant(t, f, "ka", 1)
		c, id := authed(t, key)
		for r := range rounds {
			eb, ea := both(func() error {
				_, err := store.UpsertBlocks(c, f.pool, []store.BatchUpsert{
					{Category: "learnings", Title: fmt.Sprintf("ka %d", r), Content: "batch", Scope: scopes[0], ScopeExplicit: true},
				}, id, f.set, &store.WriteBudget{APIKeyID: id})
				return err
			}, func() error {
				_, err := store.AssignTenantScope(ctx, f.pool, tenantID, fmt.Sprintf("ka-new-%d", r), nil)
				return err
			})
			if eb != nil || ea != nil {
				t.Fatalf("round %d: batch %v, assign %v", r, eb, ea)
			}
		}
	})

	t.Run("block batch × scope move", func(t *testing.T) {
		_, scopes, key := pruneWritersTenant(t, f, "km", 2)
		c, id := authed(t, key)
		mover := f.block(t, scopes[0], "km moved")
		for r := range rounds {
			to := scopes[(r+1)%2]
			eb, em := both(func() error {
				_, err := store.UpsertBlocks(c, f.pool, []store.BatchUpsert{
					{Category: "learnings", Title: fmt.Sprintf("km a %d", r), Content: "batch", Scope: scopes[0], ScopeExplicit: true},
					{Category: "learnings", Title: fmt.Sprintf("km b %d", r), Content: "batch", Scope: scopes[1], ScopeExplicit: true},
				}, id, f.set, &store.WriteBudget{APIKeyID: id})
				return err
			}, func() error {
				_, _, err := store.UpdateBlock(c, f.pool, mover, store.UpdateBlockData{Scope: &to}, scopes)
				return err
			})
			if eb != nil || em != nil {
				t.Fatalf("round %d: batch %v (deadlock %v), move %v (deadlock %v)", r, eb, deadlock(eb), em, deadlock(em))
			}
		}
	})
}
