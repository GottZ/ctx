//go:build integration

// Issue #555, review round: the gate a request's write holds (a shared
// advisory lock per key, store.holdRequestKey) and the one a server run's
// write holds (per tenant, store.holdRunTenant), against the tenant-delete
// that takes them exclusively — its key step for every key it deletes or
// takes a scope from, its first step for the tenant.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// gateAuth authenticates the plaintext key raw as a request's middleware does
// and returns the context its writes carry.
func gateAuth(t *testing.T, pool *pgxpool.Pool, raw string) (context.Context, *auth.AuthResult) {
	t.Helper()
	ar, err := auth.Authenticate(context.Background(), pool, raw)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %+v, %v", ar, err)
	}
	return requestCtx(context.Background(), ar), ar
}

// nextMultiXact is the next MultiXact id the last checkpoint recorded, after
// a checkpoint.
func nextMultiXact(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	var n string
	if err := pool.QueryRow(ctx, `SELECT next_multixact_id::text FROM pg_control_checkpoint()`).Scan(&n); err != nil {
		t.Fatalf("next multixact: %v", err)
	}
	var v int64
	if _, err := fmt.Sscan(n, &v); err != nil {
		t.Fatalf("next multixact %q: %v", n, err)
	}
	return v
}

// TestRequestKeyGateMakesNoMultiXact: a request's write holds its key's gate
// while the key authenticates 20 more times (each authentication writes the
// key row's last_used_at). The gate lives in the lock manager, so the key row
// gets no MultiXact; the key row's FOR KEY SHARE held over the write (the
// first form of #555) gave it one, for every authentication beside a holder.
func TestRequestKeyGateMakesNoMultiXact(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	_, scopes, raw := pruneWritersTenant(t, f, "gmx", 1)
	rctx, ar := gateAuth(t, f.pool, raw)
	held, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_blocks") })
	before := nextMultiXact(t, f.pool)
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlock(rctx, held, "learnings", "gmx held", "a held write", nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
		done <- err
	}()
	hold.Wait(t)
	for i := range 20 {
		var ok bool
		if err := f.pool.QueryRow(ctx, `SELECT is_valid FROM ctx_auth_by_id($1::uuid)`, ar.ApiKeyID).Scan(&ok); err != nil || !ok {
			t.Fatalf("authentication %d beside the held write: %v, %v", i, ok, err)
		}
	}
	hold.Release()
	if err := <-done; err != nil {
		t.Fatalf("held write: %v", err)
	}
	if after := nextMultiXact(t, f.pool); after != before {
		t.Fatalf("%d MultiXacts created by 20 authentications beside one held write — want 0", after-before)
	}
}

// TestKeyStepNotStarvedByOverlappingWriters: eight writers of the tenant's
// key, each a block batch of 20 items in a loop without a pause, so their
// transactions overlap all the time; a tenant-delete beside them. The key
// step waits only for the transactions that hold the gate when it asks for
// it — the lock manager queues every later shared request behind its
// exclusive one — so the first refusal comes within a second of the delete's
// start, and the delete ends.
func TestKeyStepNotStarvedByOverlappingWriters(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, raw := pruneWritersTenant(t, f, "gst", 2)
	pool := zzGatePool(t, f.dsn, 16)
	rctx, ar := gateAuth(t, pool, raw)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var written atomic.Int64
	var firstRefusal atomic.Int64
	var failed atomic.Value
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				items := make([]store.BatchUpsert, 20)
				for k := range items {
					items[k] = store.BatchUpsert{Category: "learnings", Title: fmt.Sprintf("gst %d %d %d", g, i, k), Content: "overlapping", Scope: scopes[k%2], ScopeExplicit: true}
				}
				_, err := store.UpsertBlocks(rctx, pool, items, ar.ApiKeyID, f.set, nil)
				switch {
				case err == nil:
					written.Add(1)
				case errors.Is(err, store.ErrRequestKeyGone):
					firstRefusal.CompareAndSwap(0, time.Now().UnixNano())
					return
				default:
					failed.CompareAndSwap(nil, err.Error())
					return
				}
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	w0 := written.Load()
	start := time.Now()
	perr := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{})
	took := time.Since(start)
	close(stop)
	wg.Wait()
	if msg := failed.Load(); msg != nil {
		t.Fatalf("a writer failed: %v", msg)
	}
	if perr != nil {
		t.Fatalf("tenant-delete: %v", perr)
	}
	if w0 == 0 {
		t.Fatal("the writers wrote nothing before the delete")
	}
	fr := firstRefusal.Load()
	if fr == 0 {
		t.Fatal("no writer was refused")
	}
	keyStep := time.Duration(fr - start.UnixNano())
	if keyStep > time.Second {
		t.Fatalf("the first refusal came %s after the delete began — the key step waited behind the writers' stream", keyStep)
	}
	if blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes); blocks+links+other+tenant != 0 {
		t.Fatalf("%d blocks, %d links, %d other rows, tenant %d left", blocks, links, other, tenant)
	}
	t.Logf("8 overlapping writers (%d batches before the delete): first refusal %s after the delete began, the delete took %s",
		w0, keyStep.Round(time.Millisecond), took.Round(time.Millisecond))
}

// zzGatePool is a pool of n connections on dsn.
func zzGatePool(t *testing.T, dsn string, n int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = n
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestKeyStepBesideAWriterOfAnotherInstance: the request's write runs on one
// pool (an instance), the tenant-delete on another; the write is held at its
// COMMIT. The delete's key step waits on the gate (advisory locks are
// database-wide), the write commits, the delete's passes after the key step
// delete its row, and the next write of the same request is refused.
func TestKeyStepBesideAWriterOfAnotherInstance(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, raw := pruneWritersTenant(t, f, "gin", 1)
	rctx, _ := gateAuth(t, f.pool, raw)
	instance1, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_blocks") })
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlock(rctx, instance1, "learnings", "gin held", "held at its commit", nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
		done <- err
	}()
	pid := hold.Wait(t)
	pruned := make(chan error, 1)
	go func() { pruned <- store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}) }()
	testdb.WaitBlockedBy(t, f.pool, pid, nil, "the key step of the other instance", "advisory")
	hold.Release()
	if err := <-done; err != nil {
		t.Fatalf("held write: %v", err)
	}
	if err := <-pruned; err != nil {
		t.Fatalf("tenant-delete: %v", err)
	}
	if blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes); blocks+links+other+tenant != 0 {
		t.Fatalf("%d blocks, %d links, %d other rows, tenant %d left", blocks, links, other, tenant)
	}
	if _, err := store.UpsertBlock(rctx, instance1, "learnings", "gin after", "after the delete", nil, nil, scopes[0], true, store.SensitivityWrite{}, ""); !errors.Is(err, store.ErrRequestKeyGone) {
		t.Fatalf("a write of the same request after the delete: %v, want ErrRequestKeyGone", err)
	}
}

// TestRequestKeyGateHashCollision: two keys of two tenants whose ids hash to
// one gate. A write of the first key is held at its COMMIT while the second
// tenant is deleted: the delete's key step waits for it (the shared lock),
// then both go on — the write committed and stays (its tenant lives), and the
// first key writes on. A collision costs waiting, not a wrong answer.
func TestRequestKeyGateHashCollision(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	_, scopesA, rawA := pruneWritersTenant(t, f, "gha", 1)
	tenantB, scopesB, rawB := pruneWritersTenant(t, f, "ghb", 1)
	// Two uuids with one hashtext: no production path picks a key's id, so
	// the fixture gives the two keys these ids by hand, before anything names
	// them.
	var idA, idB string
	if err := f.pool.QueryRow(ctx, `
		WITH u AS (SELECT gen_random_uuid() AS id FROM generate_series(1, 300000))
		SELECT min(id::text), max(id::text) FROM u GROUP BY hashtext(id::text) HAVING count(*) > 1 LIMIT 1`).Scan(&idA, &idB); err != nil {
		t.Fatalf("find a hash collision: %v", err)
	}
	for _, k := range []struct{ raw, id string }{{rawA, idA}, {rawB, idB}} {
		ar, err := auth.Authenticate(ctx, f.pool, k.raw)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate: %v", err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE context_api_keys SET id = $2::uuid WHERE id = $1::uuid`, ar.ApiKeyID, k.id); err != nil {
			t.Fatalf("give the key its id: %v", err)
		}
	}
	var same bool
	if err := f.pool.QueryRow(ctx, `SELECT hashtext($1::uuid::text) = hashtext($2::uuid::text)`, idA, idB).Scan(&same); err != nil || !same {
		t.Fatalf("the two ids do not collide: %v", err)
	}
	ctxA, arA := gateAuth(t, f.pool, rawA)
	if arA.ApiKeyID != idA {
		t.Fatalf("key A authenticated as %s, want %s", arA.ApiKeyID, idA)
	}
	held, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_blocks") })
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlock(ctxA, held, "learnings", "gha held", "held while the other tenant is deleted", nil, nil, scopesA[0], true, store.SensitivityWrite{}, "")
		done <- err
	}()
	pid := hold.Wait(t)
	pruned := make(chan error, 1)
	go func() { pruned <- store.PruneTenant(ctx, f.pool, tenantB, store.AuditStamp{}) }()
	testdb.WaitBlockedBy(t, f.pool, pid, nil, "the other tenant's key step", "advisory")
	hold.Release()
	if err := <-done; err != nil {
		t.Fatalf("held write of key A: %v", err)
	}
	if err := <-pruned; err != nil {
		t.Fatalf("tenant-delete of B: %v", err)
	}
	if _, err := store.UpsertBlock(ctxA, f.pool, "learnings", "gha after", "after the other delete", nil, nil, scopesA[0], true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("key A after B's delete: %v", err)
	}
	var a, b int
	_ = f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE scope = $1`, scopesA[0]).Scan(&a)
	_ = f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE scope = $1`, scopesB[0]).Scan(&b)
	if a != 2 || b != 0 {
		t.Fatalf("blocks: %d in A's scope (want 2), %d in B's (want 0)", a, b)
	}
}

// TestTenantPruneEndsBesideAWriterWithoutAGate: four writers that hold no
// gate (a writer of an older binary, a path without a key) write into the
// tenant's scopes as fast as they can for the whole delete. The delete
// cannot outrun them, but it ends: its passes end on a batch that drains
// what is there, and the closing rounds give up with an answer that says so.
// Before, the block pass ended only on a batch that found nothing — beside
// such writers it never did (the CI's pair test stood ten minutes in one).
func TestTenantPruneEndsBesideAWriterWithoutAGate(t *testing.T) {
	f := newP547Fixture(t)
	tenantID, scopes, _ := pruneWritersTenant(t, f, "gng", 2)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var written atomic.Int64
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := store.UpsertBlock(context.Background(), f.pool, "learnings", fmt.Sprintf("gng %d %d", g, i), "no gate", nil, nil, scopes[i%2], true, store.SensitivityWrite{}, ""); err == nil {
					written.Add(1)
				}
			}
		}()
	}
	// The delete starts beside writers that write: without one write through
	// it would run alone and prove nothing.
	for wait := time.Now(); written.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Since(wait) > 10*time.Second {
			close(stop)
			wg.Wait()
			t.Fatal("no writer without a gate got a write through within 10 s")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	perr := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{})
	took := time.Since(start)
	close(stop)
	wg.Wait()
	if errors.Is(perr, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("the delete did not end within 20 s beside writers without a gate (%d writes): %v", written.Load(), perr)
	}
	if perr != nil && !errors.Is(perr, store.ErrTenantPruneIncomplete) {
		t.Fatalf("tenant-delete: %v", perr)
	}
	t.Logf("the delete ended after %s beside %d writes without a gate: %v", took.Round(time.Millisecond), written.Load(), perr)
}

// TestTenantDeleteBesideARunWrite: a server run's write for the tenant
// (store.WithRunTenant) held at its COMMIT makes the delete's first step wait
// for it, and the passes after that step delete its row; a run's write after
// that step writes nothing (ErrRunTenantGone) — also one that comes after the
// whole delete, when the scope belongs to no tenant — and the next tenant
// with the slug reads nothing of it.
func TestTenantDeleteBesideARunWrite(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "grun", 1)
	run := store.WithRunTenant(ctx, tenantID)
	held, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_blocks") })
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlock(run, held, "index", "grun digest", "a run's map", nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
		done <- err
	}()
	pid := hold.Wait(t)
	pruned := make(chan error, 1)
	go func() { pruned <- store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}) }()
	testdb.WaitBlockedBy(t, f.pool, pid, nil, "the delete's first step", "advisory")
	hold.Release()
	if err := <-done; err != nil {
		t.Fatalf("held run write: %v", err)
	}
	if err := <-pruned; err != nil {
		t.Fatalf("tenant-delete: %v", err)
	}
	if _, err := store.UpsertBlock(run, f.pool, "index", "grun late", "a run that read the tenant before its delete", nil, nil, scopes[0], true, store.SensitivityWrite{}, ""); !errors.Is(err, store.ErrRunTenantGone) {
		t.Fatalf("a run's write after the delete: %v, want ErrRunTenantGone", err)
	}
	next, err := store.CreateTenant(ctx, f.pool, "grun", "grun")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssignTenantScope(ctx, f.pool, next.ID, scopes[0], nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE scope = $1`, scopes[0]).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the next owner of %s finds %d blocks (%v), want 0", scopes[0], n, err)
	}
}

// TestTenantDeleteTakesItsScopesFromOtherKeys (B2): a server-admin mints two
// keys of the default tenant that name the tenant's scopes — one with its
// home in the first, one with the second in its allowed and write scopes —
// and both write there in requests that authenticated before the delete. The
// delete deletes the first key (its home dies) and takes the scope from the
// second (it lives on): neither request writes into the tenant's scopes after
// the key step, a write of the second key that was held at its COMMIT across
// the key step is deleted with the tenant, the second key still writes its
// own home, and a new authentication of it names none of the scopes the next
// owner of the slug gets.
func TestTenantDeleteTakesItsScopesFromOtherKeys(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "gfk", 2)
	_, rawHome, err := store.CreateApiKey(ctx, f.pool, "bridge home", scopes[0], nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	_, rawList, err := store.MintKeyWithQuota(ctx, f.pool, "bridge list", "private", []string{"private", scopes[1]}, []string{scopes[1]}, store.DefaultTenantID, "member", nil, store.AuditStamp{})
	if err != nil {
		t.Fatal(err)
	}
	homeCtx, arHome := gateAuth(t, f.pool, rawHome)
	listCtx, arList := gateAuth(t, f.pool, rawList)
	held, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_blocks") })
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlock(listCtx, held, "learnings", "gfk held", "held across the key step", nil, nil, scopes[1], true, store.SensitivityWrite{}, "")
		done <- err
	}()
	pid := hold.Wait(t)
	pruned := make(chan error, 1)
	go func() { pruned <- store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}) }()
	testdb.WaitBlockedBy(t, f.pool, pid, nil, "the key step", "advisory")
	hold.Release()
	if err := <-done; err != nil {
		t.Fatalf("held write of the listing key: %v", err)
	}
	if err := <-pruned; err != nil {
		t.Fatalf("tenant-delete: %v", err)
	}
	if blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes); blocks+links+other+tenant != 0 {
		t.Fatalf("%d blocks, %d links, %d other rows, tenant %d left", blocks, links, other, tenant)
	}
	if _, err := store.UpsertBlock(homeCtx, f.pool, "learnings", "gfk home late", "x", nil, nil, scopes[0], true, store.SensitivityWrite{}, ""); !errors.Is(err, store.ErrRequestKeyGone) {
		t.Fatalf("the home key's request after the delete: %v, want ErrRequestKeyGone", err)
	}
	if _, err := store.UpsertBlock(listCtx, f.pool, "learnings", "gfk list late", "x", nil, nil, scopes[1], true, store.SensitivityWrite{}, ""); !errors.Is(err, store.ErrRequestKeyGone) {
		t.Fatalf("the listing key's request (authenticated before) after the delete: %v, want ErrRequestKeyGone", err)
	}
	var homeLeft int
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_api_keys WHERE id = $1::uuid`, arHome.ApiKeyID).Scan(&homeLeft); err != nil || homeLeft != 0 {
		t.Fatalf("the key whose home was the tenant's scope: %d rows (%v), want 0", homeLeft, err)
	}
	listAgain, ar2 := gateAuth(t, f.pool, rawList)
	for _, s := range append(append(append([]string{ar2.HomeScope}, ar2.AllowedScopes...), ar2.WriteScopes...), ar2.ReadScopes...) {
		if s == scopes[0] || s == scopes[1] {
			t.Fatalf("the listing key still names %s after the delete: %+v", s, ar2)
		}
	}
	if ar2.ApiKeyID != arList.ApiKeyID {
		t.Fatalf("the listing key changed its id")
	}
	if _, err := store.UpsertBlock(listAgain, f.pool, "learnings", "gfk own", "its own home", nil, nil, "private", true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("the listing key in its own home after the delete: %v", err)
	}
}

// TestRequestKeyGateQueueProbe (opt-in, CTX_P555_QUEUE_PROBE=1): the two
// locks side by side, outside the store — eight sessions that hold a shared
// lock in overlapping transactions of 2–6 ms without a pause, and one session
// that asks for the exclusive one 50 times (rolled back each time): the key
// row's FOR KEY SHARE against its DELETE, and the advisory gate against its
// exclusive form. It prints the waits of the exclusive side.
func TestRequestKeyGateQueueProbe(t *testing.T) {
	if os.Getenv("CTX_P555_QUEUE_PROBE") != "1" {
		t.Skip("opt-in: CTX_P555_QUEUE_PROBE=1")
	}
	f := newP547Fixture(t)
	ctx := context.Background()
	_, _, raw := pruneWritersTenant(t, f, "gqp", 1)
	_, ar := gateAuth(t, f.pool, raw)
	pool := zzGatePool(t, f.dsn, 16)
	forms := []struct{ name, share, exclusive string }{
		{"key row FOR KEY SHARE / DELETE", `SELECT 1 FROM context_api_keys WHERE id = $1::uuid FOR KEY SHARE`, `DELETE FROM context_api_keys WHERE id = $1::uuid`},
		{"advisory gate shared / exclusive", `SELECT pg_advisory_xact_lock_shared(hashtext('ctx:request-key'), hashtext($1::uuid::text))`, `SELECT pg_advisory_xact_lock(hashtext('ctx:request-key'), hashtext($1::uuid::text))`},
	}
	for _, form := range forms {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(g) * time.Millisecond)
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					tx, err := pool.Begin(ctx)
					if err != nil {
						return
					}
					_, _ = tx.Exec(ctx, form.share, ar.ApiKeyID)
					time.Sleep(time.Duration(2+(g+i)%5) * time.Millisecond)
					_ = tx.Commit(ctx)
				}
			}()
		}
		time.Sleep(300 * time.Millisecond)
		var waits []time.Duration
		for range 50 {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			if _, err := tx.Exec(ctx, form.exclusive, ar.ApiKeyID); err != nil {
				t.Fatalf("%s: %v", form.name, err)
			}
			waits = append(waits, time.Since(began))
			_ = tx.Rollback(ctx)
			time.Sleep(10 * time.Millisecond)
		}
		close(stop)
		wg.Wait()
		t.Logf("%-34s exclusive side, 50 asks beside 8 overlapping holders: p50 %s, p90 %s, max %s",
			form.name, pct(waits, 0.5), pct(waits, 0.9), pct(waits, 1))
	}
}

// pct is the q-quantile of ds.
func pct(ds []time.Duration, q float64) time.Duration {
	s := append([]time.Duration(nil), ds...)
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	i := int(q*float64(len(s))+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i].Round(10 * time.Microsecond)
}

// TestKeyStepWaitsForOneWriteOfALongRequest (the production question of the
// CI hang): one long request of the tenant's key — an ingest of 200 chunks,
// each its own write transaction, one after the other — is under way when the
// tenant is deleted. The key step waits for the chunk transaction that holds
// the gate when it asks, not for the request's next ones: the request's next
// chunk queues behind the key step and finds its key gone. So a long request
// holds the delete up for one of its write transactions, whatever its length.
func TestKeyStepWaitsForOneWriteOfALongRequest(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, raw := pruneWritersTenant(t, f, "glr", 1)
	rctx, _ := gateAuth(t, f.pool, raw)
	var source string
	if err := f.pool.QueryRow(ctx, `INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/glr', 'h', $1) RETURNING id::text`, scopes[0]).Scan(&source); err != nil {
		t.Fatal(err)
	}
	var started atomic.Int64 // the delete's start, unix nanos
	var afterStart, written atomic.Int64
	refusedAt := make(chan time.Time, 1)
	go func() {
		for i := range 200 {
			_, err := store.InsertChunk(rctx, f.pool, source, i, "learnings", fmt.Sprintf("glr chunk %d", i), "a chunk of a long ingest", nil, nil, scopes[0])
			if errors.Is(err, store.ErrRequestKeyGone) {
				refusedAt <- time.Now()
				return
			}
			if err != nil {
				t.Errorf("chunk %d: %v", i, err)
				refusedAt <- time.Time{}
				return
			}
			written.Add(1)
			if s := started.Load(); s != 0 {
				afterStart.Add(1)
			}
		}
		refusedAt <- time.Time{}
	}()
	for deadline := time.Now().Add(10 * time.Second); written.Load() < 20; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the long request wrote %d chunks in 10 s", written.Load())
		}
	}
	// The delete's pool marks when the key step asks for the first gate.
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = gateAskTracer{asked: &started}
	dpool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dpool.Close)
	start := time.Now()
	if err := store.PruneTenant(ctx, dpool, tenantID, store.AuditStamp{}); err != nil {
		t.Fatalf("tenant-delete: %v", err)
	}
	at := <-refusedAt
	if at.IsZero() {
		t.Fatalf("the long request was never refused (%d chunks written)", written.Load())
	}
	// At most the chunk in flight when the key step asked for the gate —
	// and one more: the tracer notes the ask before pgx sends it, and on two
	// cores a chunk committed and the next took its gate in between (6 of
	// 130 runs in the review). The bound still tells a key step that waits
	// for the request's 200 chunks from one that waits for a write.
	if n := afterStart.Load(); n > 2 {
		t.Fatalf("%d chunks of the long request committed after the key step asked for the gate — it waited for more than the write in flight", n)
	}
	if blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes); blocks+links+other+tenant != 0 {
		t.Fatalf("%d blocks, %d links, %d other rows, tenant %d left", blocks, links, other, tenant)
	}
	t.Logf("a 200-chunk ingest in flight: %d chunks before the key step asked for the gate, %d after, refused %s after the delete began", written.Load()-afterStart.Load(), afterStart.Load(), at.Sub(start).Round(time.Millisecond))
}

// gateAskTracer stores the time the key step first asks for a request key's
// gate (pruneKeyGateSQL) on its pool.
type gateAskTracer struct{ asked *atomic.Int64 }

func (g gateAskTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "pg_advisory_xact_lock(hashtext('ctx:request-key')") {
		g.asked.CompareAndSwap(0, time.Now().UnixNano())
	}
	return ctx
}

func (gateAskTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// passTracer counts, on the delete's pool, the block picks of its passes
// (prunePickSQL) before its last pass statement (the projects) and marks when
// its first data pass statement (the dream links of the first batch) begins.
type passTracer struct {
	mu        sync.Mutex
	picks     int
	drains    int
	closing   bool
	firstPass atomic.Int64
}

func (p *passTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.Contains(d.SQL, "DELETE FROM context_projects WHERE tenant_id"):
		p.closing = true
	case p.closing:
	case strings.Contains(d.SQL, "DELETE FROM context_dream_links WHERE ctid IN"):
		p.firstPass.CompareAndSwap(0, time.Now().UnixNano())
	case strings.HasPrefix(d.SQL, "SELECT id::text FROM context_blocks WHERE scope = ANY"):
		p.picks++
	case strings.HasPrefix(d.SQL, "DELETE FROM context_blobs WHERE ctid IN"):
		p.drains++
	}
	return ctx
}

func (*passTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestTenantPruneOrderAndPassEnds: the key step comes before the data passes,
// so no request write of the tenant commits once the first pass statement has
// begun (eight writers of the tenant's key write in a loop across the whole
// delete); and a pass ends on the batch that drains what is there — one block
// pick per scope and one blob drain for a tenant with fewer rows than a batch,
// where a pass that ended only on a batch that found nothing took two.
func TestTenantPruneOrderAndPassEnds(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, raw := pruneWritersTenant(t, f, "gord", 3)
	for i, s := range scopes {
		f.block(t, s, fmt.Sprintf("gord seed %d", i))
		if _, err := store.UpsertBlob(ctx, f.pool, "files", fmt.Sprintf("gord blob %d", i), "f.txt", "text/plain", s, []byte("x"), nil, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	rctx, ar := gateAuth(t, f.pool, raw)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var lastCommit atomic.Int64
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_, err := store.UpsertBlocks(rctx, f.pool, []store.BatchUpsert{{Category: "learnings", Title: fmt.Sprintf("gord %d %d", g, i), Content: "x", Scope: scopes[i%3], ScopeExplicit: true}}, ar.ApiKeyID, f.set, nil)
				if err == nil {
					now := time.Now().UnixNano()
					for {
						old := lastCommit.Load()
						if now <= old || lastCommit.CompareAndSwap(old, now) {
							break
						}
					}
				} else if errors.Is(err, store.ErrRequestKeyGone) {
					return
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	pt := &passTracer{}
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = pt
	dpool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dpool.Close)
	perr := store.PruneTenant(ctx, dpool, tenantID, store.AuditStamp{})
	close(stop)
	wg.Wait()
	if perr != nil {
		t.Fatalf("tenant-delete: %v", perr)
	}
	if fp, lc := pt.firstPass.Load(), lastCommit.Load(); fp == 0 || lc >= fp {
		t.Fatalf("a request write of the tenant committed %s after the first pass statement began — the key step did not come first",
			time.Duration(lc-fp))
	}
	if pt.picks != len(scopes) || pt.drains != 1 {
		t.Fatalf("the passes ran %d block picks over %d scopes and %d blob drains — want one each: a pass ends on the batch that drains what is there", pt.picks, len(scopes), pt.drains)
	}
	if blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes); blocks+links+other+tenant != 0 {
		t.Fatalf("%d blocks, %d links, %d other rows, tenant %d left", blocks, links, other, tenant)
	}
}

// TestKeyStepHoldsNoGateOfAKeyItKeeps (review of the round): the key step
// takes the tenant's scopes from a key of another tenant in a short
// transaction of its own, and deletes the keys that go in a second one —
// whose key delete sets the access log rows of those keys to NULL, millions
// at the target scale. While that second transaction is held at its COMMIT,
// the other tenant's key writes into its own home at once: its gate is not
// held. Before, both ran in one transaction holding every gate, and the other
// tenant's writes waited for the whole cascade.
func TestKeyStepHoldsNoGateOfAKeyItKeeps(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "gkk", 1)
	_, raw, err := store.MintKeyWithQuota(ctx, f.pool, "keeps", "private", []string{"private", scopes[0]}, []string{scopes[0]}, store.DefaultTenantID, "member", nil, store.AuditStamp{})
	if err != nil {
		t.Fatal(err)
	}
	held, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.HasPrefix(sql, "DELETE FROM context_api_keys WHERE tenant_id") })
	pruned := make(chan error, 1)
	go func() { pruned <- store.PruneTenant(ctx, held, tenantID, store.AuditStamp{}) }()
	hold.Wait(t)
	// A request of the kept key that authenticates now, while the key delete
	// stands at its COMMIT.
	kctx, _ := gateAuth(t, f.pool, raw)
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlock(kctx, f.pool, "learnings", "gkk own", "its own home", nil, nil, "private", true, store.SensitivityWrite{}, "")
		done <- err
	}()
	waited := false
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the kept key's write into its home beside the key delete: %v", err)
		}
	case <-time.After(5 * time.Second):
		waited = true
		t.Errorf("the kept key's write into its home waited 5 s for the key delete — its gate is held by it")
	}
	hold.Release()
	if err := <-pruned; err != nil {
		t.Fatalf("tenant-delete: %v", err)
	}
	if waited {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the kept key's write did not end after the delete")
		}
	}
}
