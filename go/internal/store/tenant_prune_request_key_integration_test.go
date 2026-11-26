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
	"os"
	"strconv"
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
// write budget, as the batch surfaces call it), a block batch without one
// (as the confirm of a staged batch calls it), and a blob, each once per
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
	rctx := requestCtx(ctx, ar)
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
	loop("block batch without a budget", func(i int) error { // the confirm of a staged batch books nothing
		_, err := store.UpsertBlocks(rctx, f.pool, []store.BatchUpsert{
			{Category: "learnings", Title: fmt.Sprintf("rk unbudgeted %d", i), Content: "a request's block", Scope: scopes[(i+2)%len(scopes)], ScopeExplicit: true},
		}, ar.ApiKeyID, f.set, nil)
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
// millisecond over the whole delete (10 runs; 50 with CTX_P555_RUNS=50): every run answers success and
// leaves no row in the tenant's scopes, and every write the writers made
// either went through (and was deleted with the tenant) or answered
// ErrRequestKeyGone.
func TestTenantPruneBesideRequestsInFlight(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	// 10 runs in the suite; the evidence run of issue #555 took 50
	// (CTX_P555_RUNS=50).
	runs, seed := envInt("CTX_P555_RUNS", 10), envInt("CTX_P555_SEED", 300)
	var written, refused int64
	var took time.Duration
	for run := range runs {
		tenantID, scopes, key := pruneWritersTenant(t, f, fmt.Sprintf("rk%02d", run), 3)
		pruneWritersSeed(t, f, scopes, seed)
		stop := make(chan struct{})
		w := &requestKeyWriters{}
		w.start(t, f, key, scopes, stop)
		time.Sleep(20 * time.Millisecond)
		start := time.Now()
		perr := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{})
		took += time.Since(start)
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
	t.Logf("%d runs of %d seeded blocks over 3 scopes: the deletes took %s in all; %d writes went through (and went with their tenant), %d were refused with ErrRequestKeyGone",
		runs, seed, took.Round(time.Millisecond), written, refused)
}

// requestCtx is the context the authentication middleware gives a request
// of ar (handler.Auth): its key and the scopes the key granted.
func requestCtx(ctx context.Context, ar *auth.AuthResult) context.Context {
	return store.WithRequestKey(ctx, store.RequestKey{ID: ar.ApiKeyID, HomeScope: ar.HomeScope, AllowedScopes: ar.AllowedScopes, WriteScopes: ar.WriteScopes})
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// TestRequestKeyHoldLockPairs: the key share every request write now takes
// first, against the paths it meets (issue #555): 20 rounds per pair (200
// with CTX_P555_PAIR_ROUNDS=200), both sides started together, no lock cycle
// (40P01) and no other fault on either side.
//
//   - writers of two keys of a tenant (a block batch and a booked update of
//     one key; a single store and a supersedes declaration of the other, on
//     the same identities and scope) × the tenant-delete, whose key step
//     deletes both keys one after the other;
//   - a writer × AssignTenantScope of its tenant (the tenant row FOR UPDATE);
//   - a block batch × a scope move of one of its scopes' blocks (both
//     requests of the same key).
func TestRequestKeyHoldLockPairs(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	// 20 rounds per pair in the suite (the first pair takes about 5 s a
	// round); the evidence run of issue #555 took 200 (CTX_P555_PAIR_ROUNDS=200).
	rounds := envInt("CTX_P555_PAIR_ROUNDS", 20)
	deadlock := func(err error) bool { return err != nil && pgxdb.SerializationConflict(err) }
	authed := func(t *testing.T, key string) (context.Context, string) {
		ar, err := auth.Authenticate(ctx, f.pool, key)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate: %v", err)
		}
		return requestCtx(ctx, ar), ar.ApiKeyID
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
		var longest time.Duration
		for r := range rounds {
			tenantID, scopes, key1 := pruneWritersTenant(t, f, fmt.Sprintf("kp%03d", r), 2)
			_, key2, err := store.CreateApiKey(ctx, f.pool, "second key", scopes[0], scopes, tenantID)
			if err != nil {
				t.Fatal(err)
			}
			c1, id1 := authed(t, key1)
			c2, id2 := authed(t, key2)
			target := f.block(t, scopes[0], "kp seed a")
			f.block(t, scopes[1], "kp seed b")
			same0 := f.block(t, scopes[0], "kp same 0")
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
			// A booked update of the first key on a block the second key's
			// single store writes too: its booking takes the key's key share
			// after the row unless it holds the key first.
			loop("booked update", func(i int) error {
				content := fmt.Sprintf("updated %d", i)
				_, _, err := store.UpdateBlockBooked(c1, f.pool, same0, store.UpdateBlockData{Content: &content}, scopes, &store.WriteBudget{APIKeyID: id1})
				if errors.Is(err, store.ErrBlockNotFound) {
					return nil // the delete's block pass took it: an answer, not a fault
				}
				return err
			})
			// A declaration of the second key: it takes the scope lock the
			// batch of the first key takes (a writer that held that lock and
			// then waited for its key would meet it in a cycle).
			loop("declaration", func(i int) error {
				_, err := store.UpsertBlockSupersedes(c2, f.pool, "learnings", "kp declaring", fmt.Sprintf("declaration %d", i), nil, nil, scopes[0], true,
					store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: target, APIKeyID: id2, Types: f.set})
				if errors.Is(err, store.ErrSupersedesTarget) {
					return nil // the delete's block pass took the target: an answer, not a fault
				}
				return err
			})
			// A round is bounded: on the CI's two cores the delete of the
			// first form of #555 stood 10 min in its block pass beside these
			// writers (the pass ended only on a batch that found nothing, and
			// the single store wrote the rows again faster than a batch came
			// round; the key step, which would have stopped the writers, came
			// after the passes).
			pctx, cancel := context.WithTimeout(ctx, time.Minute)
			start := time.Now()
			perr := store.PruneTenant(pctx, f.pool, tenantID, store.AuditStamp{})
			took := time.Since(start)
			cancel()
			longest = max(longest, took)
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
		t.Logf("%d rounds, %d writes refused after the key step, the longest delete took %s", rounds, refusals, longest.Round(time.Millisecond))
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
