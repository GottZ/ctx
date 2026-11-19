//go:build integration

// Issue #211: the lock order of store.UpsertBlocks against the transactions it
// can meet. A batch holds several block rows and waits for the next one, so
// every other transaction that holds rows and waits for more could close a
// cycle with it; the batch takes the supersedes lock of each scope it writes,
// exclusively and in sorted order, before any row lock (upsert_batch.go).
//
// Each test pauses the first batch on a lock a third connection holds, lets the
// second party queue behind it, then releases: the interleaving in which a
// wrong order deadlocks. The batches run on single-connection pools named by
// application_name, so pg_stat_activity tells when each one waits.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestUpsertBlocks' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ubActor is a one-connection pool whose backend carries name.
func ubActor(t *testing.T, dsn, name string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	cfg.ConnConfig.Tracer = tracer
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("actor pool %s: %v", name, err)
	}
	t.Cleanup(p.Close)
	return p
}

// ubWaitForLockWait returns once the backend named name waits for a lock.
func ubWaitForLockWait(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; {
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*)::int FROM pg_stat_activity
			  WHERE application_name = $1 AND datname = current_database() AND wait_event_type = 'Lock'`,
			name).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never waited for a lock", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ubIsDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func ubItem(scope, title string) store.BatchUpsert {
	return store.BatchUpsert{Category: "batch", Title: title, Content: title + " rewritten", Scope: scope,
		Sens: store.SensitivityWrite{}}
}

func ubSeed(t *testing.T, pool *pgxpool.Pool, scope, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "batch", title, title+" body", nil, nil, scope, true,
		store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s/%s: %v", scope, title, err)
	}
	return b.ID
}

// ubRun starts UpsertBlocks on its own actor and returns its result channel.
func ubRun(t *testing.T, dsn, name string, items []store.BatchUpsert, types *blocktype.Set) <-chan error {
	t.Helper()
	actor := ubActor(t, dsn, name, nil)
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlocks(context.Background(), actor, items, "ub", types, nil)
		done <- err
	}()
	return done
}

func ubAwait(t *testing.T, name string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish", name)
		return nil
	}
}

// TestUpsertBlocksTwoBatchesAcrossScopes: two batches over the same two scopes,
// in both list orders. The first waits for the first scope lock, which a third
// transaction holds; the second queues behind it. Released, both write and
// neither ends in 40P01 — the scope locks are taken in sorted order, so the
// second never holds the lock the first needs next.
func TestUpsertBlocksTwoBatchesAcrossScopes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		first, second []store.BatchUpsert
	}{
		{"first in sorted order", []store.BatchUpsert{ubItem("ub-s1", "a"), ubItem("ub-s2", "b")},
			[]store.BatchUpsert{ubItem("ub-s2", "b"), ubItem("ub-s1", "a")}},
		{"first in reverse order", []store.BatchUpsert{ubItem("ub-s2", "b"), ubItem("ub-s1", "a")},
			[]store.BatchUpsert{ubItem("ub-s1", "a"), ubItem("ub-s2", "b")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ubSeed(t, pool, "ub-s1", "a")
			ubSeed(t, pool, "ub-s2", "b")
			gate, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gate.Rollback(ctx) }()
			if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ctx:supersedes:' || 'ub-s1'))`); err != nil {
				t.Fatal(err)
			}
			first := ubRun(t, dsn, "ub-first", tc.first, nil)
			ubWaitForLockWait(t, pool, "ub-first")
			second := ubRun(t, dsn, "ub-second", tc.second, nil)
			ubWaitForLockWait(t, pool, "ub-second")
			if err := gate.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			for name, done := range map[string]<-chan error{"ub-first": first, "ub-second": second} {
				if err := ubAwait(t, name, done); err != nil {
					t.Errorf("%s: %v (deadlock: %v)", name, err, ubIsDeadlock(err))
				}
			}
		})
	}
}

// TestUpsertBlocksTwoBatchesOneScope: two batches over the same two blocks of
// one scope, in both list orders, while a third transaction holds the first
// block. Neither ends in 40P01: the second waits for the scope lock and holds
// no row while it waits.
func TestUpsertBlocksTwoBatchesOneScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	for i, order := range [][2][]store.BatchUpsert{
		{{ubItem("ub-one", "a"), ubItem("ub-one", "b")}, {ubItem("ub-one", "b"), ubItem("ub-one", "a")}},
		{{ubItem("ub-one", "b"), ubItem("ub-one", "a")}, {ubItem("ub-one", "a"), ubItem("ub-one", "b")}},
	} {
		t.Run(fmt.Sprintf("order %d", i), func(t *testing.T) {
			a := ubSeed(t, pool, "ub-one", "a")
			ubSeed(t, pool, "ub-one", "b")
			gate, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gate.Rollback(ctx) }()
			if _, err := gate.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, a); err != nil {
				t.Fatal(err)
			}
			first := ubRun(t, dsn, "ub-first", order[0], nil)
			ubWaitForLockWait(t, pool, "ub-first")
			second := ubRun(t, dsn, "ub-second", order[1], nil)
			ubWaitForLockWait(t, pool, "ub-second")
			if err := gate.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			for name, done := range map[string]<-chan error{"ub-first": first, "ub-second": second} {
				if err := ubAwait(t, name, done); err != nil {
					t.Errorf("%s: %v (deadlock: %v)", name, err, ubIsDeadlock(err))
				}
			}
		})
	}
}

// TestUpsertBlocksAgainstADreamBatch: a dream link batch takes the scope's
// supersedes lock SHARED first and then, with each link it inserts, the
// key-share lock of both endpoints (the foreign-key check). Modelled here by
// its locks: the shared scope lock, the key-share lock of b (a link already
// inserted), then the key-share lock of a (the next link). The batch writes a
// and b while a third transaction holds a. Without the batch's exclusive scope
// lock the batch would get a, wait for b (key-shared by the link batch) while
// the link batch waits for a: 40P01. With it, the link batch waits for the
// scope lock holding nothing.
func TestUpsertBlocksAgainstADreamBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	a := ubSeed(t, pool, "ub-dream", "a")
	b := ubSeed(t, pool, "ub-dream", "b")

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, a); err != nil {
		t.Fatal(err)
	}
	batch := ubRun(t, dsn, "ub-batch", []store.BatchUpsert{ubItem("ub-dream", "a"), ubItem("ub-dream", "b")}, nil)
	ubWaitForLockWait(t, pool, "ub-batch")

	links := ubActor(t, dsn, "ub-links", nil)
	linksDone := make(chan error, 1)
	go func() {
		linksDone <- pgxWriteSteps(ctx, links,
			`SELECT pg_advisory_xact_lock_shared(hashtext('ctx:supersedes:' || 'ub-dream'))`,
			fmt.Sprintf(`SELECT 1 FROM context_blocks WHERE id = '%s' FOR KEY SHARE`, b),
			fmt.Sprintf(`SELECT 1 FROM context_blocks WHERE id = '%s' FOR KEY SHARE`, a))
	}()
	ubWaitForLockWait(t, pool, "ub-links")
	if err := gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ubAwait(t, "ub-batch", batch); err != nil {
		t.Errorf("batch: %v (deadlock: %v)", err, ubIsDeadlock(err))
	}
	if err := ubAwait(t, "ub-links", linksDone); err != nil {
		t.Errorf("link batch: %v (deadlock: %v)", err, ubIsDeadlock(err))
	}
}

// TestUpsertBlocksBudgetAcrossConcurrentBatches: two batches of one key in two
// scopes, 4 writes each against a limit of 6. The first counts and then waits
// for a row a third transaction holds; the second runs in the meantime. It
// must count after the first committed its booking — under the key's budget
// lock — and be refused: a count taken beside the first one would see 0 used
// and let both through (8 writes against 6).
func TestUpsertBlocksBudgetAcrossConcurrentBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	key, _, err := store.CreateApiKey(ctx, pool, "ub-budget", "ub-b1", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	a := ubSeed(t, pool, "ub-b1", "a")
	budget := &store.WriteBudget{APIKeyID: key.ID, Limit: 6}
	four := func(scope, first string) []store.BatchUpsert {
		return []store.BatchUpsert{ubItem(scope, first), ubItem(scope, "n1"), ubItem(scope, "n2"), ubItem(scope, "n3")}
	}

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, a); err != nil {
		t.Fatal(err)
	}
	run := func(name string, items []store.BatchUpsert) <-chan error {
		actor := ubActor(t, dsn, name, nil)
		done := make(chan error, 1)
		go func() {
			_, err := store.UpsertBlocks(ctx, actor, items, key.ID, nil, budget)
			done <- err
		}()
		return done
	}
	first := run("ub-budget-first", four("ub-b1", "a"))
	ubWaitForLockWait(t, pool, "ub-budget-first")
	second := run("ub-budget-second", four("ub-b2", "b"))
	// The second either waits (for the budget lock) or finishes before the
	// first can commit; both are observed before the gate opens.
	var secondErr error
	secondDone := false
	for deadline := time.Now().Add(20 * time.Second); ; {
		select {
		case secondErr = <-second:
			secondDone = true
		default:
		}
		if secondDone {
			break
		}
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM pg_stat_activity
			  WHERE application_name = 'ub-budget-second' AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second batch neither waited nor finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ubAwait(t, "ub-budget-first", first); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if !secondDone {
		secondErr = ubAwait(t, "ub-budget-second", second)
	}
	var over *store.WriteBudgetError
	if !errors.As(secondErr, &over) || over.Used != 4 || over.Writes != 4 || over.Limit != 6 {
		t.Fatalf("second batch: %v; want the budget refusal with 4 of 6 used", secondErr)
	}
	var booked, written int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM context_access_log WHERE api_key_id = $1 AND action = 'write')::int,
		(SELECT count(*) FROM context_blocks WHERE scope = 'ub-b2')::int`, key.ID).Scan(&booked, &written); err != nil {
		t.Fatal(err)
	}
	if booked != 4 || written != 0 {
		t.Fatalf("booked %d writes and wrote %d blocks of the refused batch; want 4 and 0", booked, written)
	}
}

// pgxWriteSteps runs statements in one transaction and commits.
func pgxWriteSteps(ctx context.Context, pool *pgxpool.Pool, stmts ...string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ubCounter counts the statements a pool sends.
type ubCounter struct{ n atomic.Int64 }

func (c *ubCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *ubCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestUpsertBlocksCostAgainstSingleWrites measures a batch at the bound
// against the same writes as single UpsertBlock calls: wall time (the batch's
// is its transaction time — the time it holds its locks), statements sent and
// shared-buffer accesses of the backend that ran them. It logs and asserts
// only what the code fixes: the statement count of each form.
func TestUpsertBlocksCostAgainstSingleWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	for si, shape := range []struct {
		name  string
		n     int
		bytes int
	}{
		{"100 items of 10 KB (the default cap, the 1 MB body cap)", 100, 10 * 1024},
		{"20 items of 50 KB (the content cap, the 1 MB body cap)", 20, 50 * 1024},
		{"100 items of 200 bytes", 100, 200},
	} {
		for _, mode := range []string{"insert", "update"} {
			t.Run(shape.name+" "+mode, func(t *testing.T) {
				prefix := fmt.Sprintf("shape%d-%s", si, mode)
				items := make([]store.BatchUpsert, shape.n)
				for i := range items {
					items[i] = store.BatchUpsert{Category: "cost", Title: fmt.Sprintf("%s-%03d", prefix, i),
						Content: strings.Repeat(string(rune('a'+i%26)), shape.bytes), Scope: "ub-cost"}
				}
				singles := make([]store.BatchUpsert, shape.n)
				for i := range singles {
					singles[i] = items[i]
					singles[i].Title += "-single"
				}
				if mode == "update" {
					// The same identities exist already, with another content.
					for _, it := range append(append([]store.BatchUpsert{}, items...), singles...) {
						ubSeed(t, pool, "ub-cost", it.Title)
					}
					for i := range items {
						items[i].Category, singles[i].Category = "batch", "batch"
					}
				}

				// A fresh insert reads the catalog once a minute per process and
				// database (derivedIdentityIndexReady, migration 187). One
				// insert before the measurement takes that reading, so neither
				// form is charged the statement the other one then saves.
				if _, err := store.UpsertBlock(ctx, pool, "cost", prefix+"-warm-up", "x", nil, nil, "ub-cost", false,
					store.SensitivityWrite{}, ""); err != nil {
					t.Fatalf("warm-up: %v", err)
				}

				var batchCount, singleCount ubCounter
				batchPool := ubActor(t, dsn, "ub-cost-batch", &batchCount)
				singlePool := ubActor(t, dsn, "ub-cost-single", &singleCount)
				hitsBefore := ubBufferAccesses(t, batchPool)
				start := time.Now()
				if _, err := store.UpsertBlocks(ctx, batchPool, items, "ub", nil, nil); err != nil {
					t.Fatalf("batch: %v", err)
				}
				batchTime := time.Since(start)
				batchHits := ubBufferAccesses(t, batchPool) - hitsBefore
				batchStmts := batchCount.n.Load() - 4 // the two calls of ubBufferAccesses, two statements each

				hitsBefore = ubBufferAccesses(t, singlePool)
				start = time.Now()
				for _, it := range singles {
					if _, err := store.UpsertBlock(ctx, singlePool, it.Category, it.Title, it.Content, nil, nil, it.Scope, false,
						store.SensitivityWrite{}, ""); err != nil {
						t.Fatalf("single: %v", err)
					}
				}
				singleTime := time.Since(start)
				singleHits := ubBufferAccesses(t, singlePool) - hitsBefore
				singleStmts := singleCount.n.Load() - 4

				t.Logf("batch: %v (%v per item), %d statements, %d buffer accesses | singles: %v (%v per item), %d statements, %d buffer accesses",
					batchTime, batchTime/time.Duration(shape.n), batchStmts, batchHits,
					singleTime, singleTime/time.Duration(shape.n), singleStmts, singleHits)
				// What the code fixes, whatever one item's own statements are
				// (the pre-read, the upsert, the provenance probe of an insert,
				// the stale-vector clear of a replaced content): a single write
				// sends them inside its own begin and commit, the batch sends
				// them inside one begin, one scope lock and one commit.
				if saved := singleStmts - batchStmts; saved != 2*int64(shape.n)-3 {
					t.Errorf("single statements %d - batch statements %d = %d, want 2n-3 = %d",
						singleStmts, batchStmts, saved, 2*int64(shape.n)-3)
				}
			})
		}
	}
}

// TestUpsertBlocksNoopCost measures what an item that changes nothing costs
// since the batch decides it on its locked row (review of #389): before, one
// IdenticalContentMatch per item on the pool, outside any transaction and
// without a lock; now one lock-and-compare statement per item inside the
// batch's transaction, the row held to the commit. Logged: statements,
// shared-buffer accesses, WAL bytes (a row lock marks the tuple, which writes
// WAL) and wall time. Asserted: the statement count of each form.
func TestUpsertBlocksNoopCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	for _, n := range []int{50, 100} {
		t.Run(fmt.Sprintf("%d unchanged items", n), func(t *testing.T) {
			items := make([]store.BatchUpsert, n)
			for i := range items {
				title := fmt.Sprintf("noop-%d-%03d", n, i)
				ubSeed(t, pool, "ub-noop", title)
				items[i] = store.BatchUpsert{Category: "batch", Title: title, Content: title + " body", Scope: "ub-noop"}
			}
			var before, after ubCounter
			prePool := ubActor(t, dsn, "ub-noop-before", &before)
			batchPool := ubActor(t, dsn, "ub-noop-after", &after)

			hits, wal := ubBufferAccesses(t, prePool), ubWALBytes(t, prePool)
			start := time.Now()
			for _, it := range items {
				m, err := store.IdenticalContentMatch(ctx, prePool, it.Content, it.Scope, it.Category, it.Title,
					it.Tags, it.Metadata, it.Sens, it.TypeName)
				if err != nil || !m.Unchanged() {
					t.Fatalf("pre-check %s: %+v %v", it.Title, m, err)
				}
			}
			preTime := time.Since(start)
			preHits, preWAL := ubBufferAccesses(t, prePool)-hits, ubWALBytes(t, prePool)-wal
			preStmts := before.n.Load() - 8 // ubBufferAccesses and ubWALBytes before and after, two statements each

			hits, wal = ubBufferAccesses(t, batchPool), ubWALBytes(t, batchPool)
			start = time.Now()
			results, err := store.UpsertBlocks(ctx, batchPool, items, "ub", nil, nil)
			if err != nil {
				t.Fatalf("batch: %v", err)
			}
			batchTime := time.Since(start)
			batchHits, batchWAL := ubBufferAccesses(t, batchPool)-hits, ubWALBytes(t, batchPool)-wal
			batchStmts := after.n.Load() - 8
			for i, r := range results {
				if r.Block != nil || !r.Match.Unchanged() {
					t.Fatalf("items[%d] written or not unchanged: %+v", i, r)
				}
			}
			t.Logf("before (unlocked pre-check): %v (%v per item), %d statements, %d buffer accesses, %d WAL bytes | "+
				"after (lock and compare in the batch): %v (%v per item), %d statements, %d buffer accesses, %d WAL bytes",
				preTime, preTime/time.Duration(n), preStmts, preHits, preWAL,
				batchTime, batchTime/time.Duration(n), batchStmts, batchHits, batchWAL)
			// Before: one statement per item. After: begin, the scope lock,
			// one statement per item, commit.
			if preStmts != int64(n) || batchStmts != int64(n)+3 {
				t.Errorf("statements before %d, after %d; want n = %d and n+3 = %d", preStmts, batchStmts, n, n+3)
			}
		})
	}
}

// ubWALBytes is the WAL the pool's one backend has written (PostgreSQL 18's
// per-backend WAL statistics), flushed to the statistics first.
func ubWALBytes(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush stats: %v", err)
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(wal_bytes, 0)::bigint FROM pg_stat_get_backend_wal(pg_backend_pid())`).Scan(&n); err != nil {
		t.Fatalf("pg_stat_get_backend_wal: %v", err)
	}
	return n
}

// ubBufferAccesses is the shared-buffer accesses (hits and reads) the pool's
// one backend has made (PostgreSQL 18's per-backend I/O statistics), flushed
// to the statistics first.
func ubBufferAccesses(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush stats: %v", err)
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(sum(hits + reads), 0)::bigint FROM pg_stat_get_backend_io(pg_backend_pid())`).Scan(&n); err != nil {
		t.Fatalf("pg_stat_get_backend_io: %v", err)
	}
	return n
}

// TestWriteBookingCarriesTheBookingTime: a booking row carries the time it was
// written, not the start of its transaction. A batch waits for the key's
// budget lock and for scope locks before it books; with the column default
// now() a wait past the 60 s window put its rows outside every later count of
// the key (review of #389). The wait is a pg_sleep inside the transaction.
func TestWriteBookingCarriesTheBookingTime(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key, _, err := store.CreateApiKey(ctx, pool, "ub-booktime", "ub-bt", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var started time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	const wait = 1500 * time.Millisecond
	if _, err := tx.Exec(ctx, `SELECT pg_sleep($1)`, wait.Seconds()); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveWriteIntents(ctx, tx, store.WriteBudget{APIKeyID: key.ID, Limit: 10}, 2); err != nil {
		t.Fatalf("reserve intents: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	var oldest time.Time
	if err := pool.QueryRow(ctx,
		`SELECT count(*), min(created_at) FROM context_access_log WHERE api_key_id = $1::uuid AND action = 'write'`,
		key.ID).Scan(&rows, &oldest); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("booked rows %d, want 2", rows)
	}
	if got := oldest.Sub(started); got < wait {
		t.Errorf("booking row is dated %v after the start of its transaction, want at least the %v the transaction waited before it booked — the row carries the transaction's start", got, wait)
	}
}

// TestWriteBudgetCountsTheWindowWhenItCounts: a batch counts the key's window
// back from the moment its count runs, not from the start of its transaction
// (review of #389). The batch waits for the key's budget lock, which a second
// connection holds; while it waits, that connection books one write dated 57 s
// back and holds on for 4 s more. When the batch counts, the booking is at
// least 61 s old — outside the window — and a limit of 1 leaves room for the
// batch's one write. Counted from the transaction's start, the booking was 57 s
// old or less and the batch was refused.
func TestWriteBudgetCountsTheWindowWhenItCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	key, _, err := store.CreateApiKey(ctx, pool, "ub-window", "ub-w", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	budget := store.WriteBudget{APIKeyID: key.ID, Limit: 1}

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	// The budget lock, taken the way a stage takes it (nothing reserved).
	if err := store.ReserveWriteIntents(ctx, gate, budget, 0); err != nil {
		t.Fatalf("take the budget lock: %v", err)
	}
	actor := ubActor(t, dsn, "ub-window-batch", nil)
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlocks(ctx, actor, []store.BatchUpsert{ubItem("ub-w", "a")}, key.ID, nil, &budget)
		done <- err
	}()
	ubWaitForLockWait(t, pool, "ub-window-batch")
	// Booked after the batch's transaction began, so inside the window as
	// that transaction's start sees it.
	if _, err := gate.Exec(ctx,
		`INSERT INTO context_access_log (api_key_id, action, metadata, created_at)
		 VALUES ($1::uuid, 'write', '{}'::jsonb, clock_timestamp() - interval '57 seconds')`, key.ID); err != nil {
		t.Fatalf("backdated booking: %v", err)
	}
	time.Sleep(4 * time.Second)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ubAwait(t, "ub-window-batch", done); err != nil {
		t.Fatalf("batch after the wait: %v; want it written — the one booking of the window aged out while it waited", err)
	}
}
