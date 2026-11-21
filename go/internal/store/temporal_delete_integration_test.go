//go:build integration

// Issue #465: a temporal writer of a block and a hard delete of the same
// block (a purge; the DELETE on context_blocks cascades into context_temporal
// and context_temporal_reviews) take the block row and the block's temporal
// rows. The writer takes the block row (FOR KEY SHARE) before its rows, the
// order the delete takes them in, so the two wait for each other in one
// direction only; a writer that loses a deadlock or meets the foreign-key
// refusal all the same reports the block as gone only once it is gone.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestTemporalWriter' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i465Writer is one temporal writer of a block under test: the write
// afterwork (RefreshTemporal, not covered by a review) or the dream's review
// (PopulateTemporalReview).
type i465Writer struct {
	name  string
	write func(ctx context.Context, pool *pgxpool.Pool, b *store.Block) (bool, error)
}

var i465Writers = []i465Writer{
	{"afterwork", func(ctx context.Context, pool *pgxpool.Pool, b *store.Block) (bool, error) {
		return store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt)
	}},
	{"review", func(ctx context.Context, pool *pgxpool.Pool, b *store.Block) (bool, error) {
		return store.PopulateTemporalReview(ctx, pool, b.ID, b.Title, b.Content, store.ExtractDates(b.Content), b.CreatedAt)
	}},
}

// i465Block stores a block and its temporal rows through the write path.
func i465Block(t *testing.T, pool *pgxpool.Pool, title string) *store.Block {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "i465", title, "Review on 2026-03-02 and 2026-04-13.", nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}
	return b
}

// i465Conn is a connection of its own with deadlock_timeout set, the one
// setting that decides which side of a cycle PostgreSQL aborts: the backend
// whose wait reaches it first runs the check and is the victim.
func i465Conn(t *testing.T, dsn, deadlockTimeout string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(context.Background(), `SET deadlock_timeout = '`+deadlockTimeout+`'`); err != nil {
		t.Fatalf("set deadlock_timeout: %v", err)
	}
	return conn
}

// i465Pid is the backend pid of conn.
func i465Pid(t *testing.T, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) uint32 {
	t.Helper()
	var pid uint32
	if err := conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("pid: %v", err)
	}
	return pid
}

// i465WaitLockWaiters waits until n backends of the database wait on a lock.
func i465WaitLockWaiters(t *testing.T, pool *pgxpool.Pool, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(testdb.CommitHoldBound)
	for {
		var got int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			t.Fatalf("poll lock waiters: %v", err)
		}
		if got >= n {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s: %d backend(s) wait on a lock within %s, want %d", what, got, testdb.CommitHoldBound, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// i465Code is the SQLSTATE of err, "" without one.
func i465Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// i465Describe is err with the SQLSTATE and, for a deadlock, PostgreSQL's
// account of the cycle (Detail) and where it was met (Where).
func i465Describe(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Sprint(err)
	}
	return fmt.Sprintf("%v [SQLSTATE %s; detail: %s; where: %s]", err, pgErr.Code, pgErr.Detail, pgErr.Where)
}

// i465Gone asserts that the block and every row of it in context_temporal and
// context_temporal_reviews are gone.
func i465Gone(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	var blocks, rows, versions int
	if err := pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM context_blocks WHERE id = $1),
		        (SELECT count(*) FROM context_temporal WHERE block_id = $1),
		        (SELECT count(*) FROM context_temporal_reviews WHERE block_id = $1)`, id).
		Scan(&blocks, &rows, &versions); err != nil {
		t.Fatalf("count leftovers: %v", err)
	}
	if blocks != 0 || rows != 0 || versions != 0 {
		t.Errorf("after the delete: %d block row(s), %d temporal row(s), %d version row(s), want none", blocks, rows, versions)
	}
}

// i465Result is what a goroutine of a test reports back.
type i465Result struct {
	applied bool
	err     error
}

// TestTemporalWriterAgainstADeleteThatGetsTheBlockFirst: a hard delete of
// the block waits for a transaction that holds the block row (FOR UPDATE, as
// a purge's pick takes it); a temporal writer of the block starts after it
// and waits too. Once the holder commits, the delete gets the row first and
// its cascade reaches the block's temporal rows. Before, the writer had
// deleted those rows already (its DELETE ran before its first insert took
// the block row) and waited for the block row the delete held: a deadlock,
// with the delete as the victim (its deadlock_timeout is the shorter one, so
// it runs the check). Now the writer waits for the block row before it
// touches a row of its own; the delete commits, and the writer reports the
// block as gone.
func TestTemporalWriterAgainstADeleteThatGetsTheBlockFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	for _, w := range i465Writers {
		t.Run(w.name, func(t *testing.T) {
			ctx := context.Background()
			b := i465Block(t, pool, "Delete first "+w.name)

			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin holder: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, b.ID); err != nil {
				t.Fatalf("hold the block row: %v", err)
			}

			deleter := i465Conn(t, dsn, "50ms")
			deleted := make(chan error, 1)
			go func() {
				_, err := deleter.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1`, b.ID)
				deleted <- err
			}()
			i465WaitLockWaiters(t, pool, 1, "the delete")

			written := make(chan i465Result, 1)
			go func() {
				applied, err := w.write(ctx, pool, b)
				written <- i465Result{applied, err}
			}()
			i465WaitLockWaiters(t, pool, 2, "the writer")

			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("commit holder: %v", err)
			}
			if err := <-deleted; err != nil {
				t.Errorf("delete: %s, want it to commit", i465Describe(err))
			}
			if r := <-written; r.err != nil || r.applied {
				t.Errorf("writer: applied=%v err=%s, want applied=false without an error (the block is gone)", r.applied, i465Describe(r.err))
			}
			i465Gone(t, pool, b.ID)
		})
	}
}

// TestTemporalWriterAgainstADeleteThatComesSecond is the other order: the
// writer waits for the held block row first, the delete after it. The writer
// gets the row first, writes and commits; the delete then takes the block
// and its cascade removes the writer's rows. No deadlock either way, before
// and after issue #465.
func TestTemporalWriterAgainstADeleteThatComesSecond(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	for _, w := range i465Writers {
		t.Run(w.name, func(t *testing.T) {
			ctx := context.Background()
			b := i465Block(t, pool, "Delete second "+w.name)

			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin holder: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, b.ID); err != nil {
				t.Fatalf("hold the block row: %v", err)
			}

			written := make(chan i465Result, 1)
			go func() {
				applied, err := w.write(ctx, pool, b)
				written <- i465Result{applied, err}
			}()
			i465WaitLockWaiters(t, pool, 1, "the writer")

			deleter := i465Conn(t, dsn, "50ms")
			deleted := make(chan error, 1)
			go func() {
				_, err := deleter.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1`, b.ID)
				deleted <- err
			}()
			i465WaitLockWaiters(t, pool, 2, "the delete")

			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("commit holder: %v", err)
			}
			if r := <-written; r.err != nil || !r.applied {
				t.Errorf("writer: applied=%v err=%s, want applied=true (it had the block first)", r.applied, i465Describe(r.err))
			}
			if err := <-deleted; err != nil {
				t.Errorf("delete: %s, want it to commit", i465Describe(err))
			}
			i465Gone(t, pool, b.ID)
		})
	}
}

// i465VictimSetup stores a block, has a transaction of its own (20 s
// deadlock_timeout: never the victim) lock one of the block's temporal rows,
// starts the writer, which waits for that row (1 s, the default: the victim),
// and then deletes the block in that transaction, which waits for the block
// row the writer holds. PostgreSQL aborts the writer. It returns the
// transaction, once its DELETE went through, and the writer's result channel.
func i465VictimSetup(t *testing.T, pool *pgxpool.Pool, dsn string, w i465Writer) (pgx.Tx, uint32, *store.Block, <-chan i465Result, <-chan struct{}) {
	t.Helper()
	ctx := context.Background()
	b := i465Block(t, pool, "Victim "+w.name)

	conn := i465Conn(t, dsn, "20s")
	other, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = other.Rollback(context.Background()) })
	pid := i465Pid(t, other)
	if _, err := other.Exec(ctx,
		`SELECT 1 FROM context_temporal WHERE block_id = $1 ORDER BY ctid DESC LIMIT 1 FOR UPDATE`, b.ID); err != nil {
		t.Fatalf("lock a temporal row: %v", err)
	}

	written := make(chan i465Result, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		applied, err := w.write(ctx, pool, b)
		written <- i465Result{applied, err}
	}()
	testdb.WaitBlockedBy(t, pool, pid, done, "the writer", "transactionid")

	if _, err := other.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1`, b.ID); err != nil {
		t.Fatalf("delete beside the writer: %s, want the writer as the victim", i465Describe(err))
	}
	return other, pid, b, written, done
}

// TestTemporalWriterVictimOfAGoingBlockReportsItGone: a deadlock the writer
// loses against a transaction that deletes the block (here one that locked a
// temporal row first, a cycle the order of issue #465 cannot rule out for a
// deleter that takes the rows before the block) is the block going, once the
// delete commits: the writer reads the block again, waits for the delete to
// end, and reports applied=false without an error, as it does for the
// foreign-key refusal. Before, it returned the 40P01.
func TestTemporalWriterVictimOfAGoingBlockReportsItGone(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	for _, w := range i465Writers {
		t.Run(w.name, func(t *testing.T) {
			other, pid, b, written, done := i465VictimSetup(t, pool, dsn, w)
			// The writer, aborted, reads the block again and waits for the
			// delete to end: it answers for the delete's outcome, not for a
			// snapshot taken while the delete was in flight.
			testdb.WaitBlockedBy(t, pool, pid, done, "the writer's read after the deadlock", "transactionid")
			if err := other.Commit(context.Background()); err != nil {
				t.Fatalf("commit the delete: %v", err)
			}
			if r := <-written; r.err != nil || r.applied {
				t.Errorf("writer: applied=%v err=%s, want applied=false without an error (the block is gone)", r.applied, i465Describe(r.err))
			}
			i465Gone(t, pool, b.ID)
		})
	}
}

// TestTemporalWriterVictimKeepsTheErrorWhenTheBlockStays: the same deadlock,
// but the deleting transaction rolls back. The block is still there, so the
// abort was no block going: the writer returns the 40P01.
func TestTemporalWriterVictimKeepsTheErrorWhenTheBlockStays(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	for _, w := range i465Writers {
		t.Run(w.name, func(t *testing.T) {
			other, _, b, written, _ := i465VictimSetup(t, pool, dsn, w)
			if err := other.Rollback(context.Background()); err != nil {
				t.Fatalf("roll back the delete: %v", err)
			}
			r := <-written
			if r.applied || i465Code(r.err) != "40P01" {
				t.Errorf("writer: applied=%v err=%v, want the deadlock (40P01): the block stays", r.applied, r.err)
			}
			var n int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_blocks WHERE id = $1`, b.ID).Scan(&n); err != nil || n != 1 {
				t.Fatalf("block rows = %d (%v), want 1", n, err)
			}
		})
	}
}

// i465LoadOps bounds TestTemporalWritersAgainstHardDeletes: operations in
// all (temporal writes and hard deletes with their re-stores), a count, not a
// duration, so a slow runner does the same work.
const i465LoadOps = 2400

// TestTemporalWritersAgainstHardDeletes is the load probe of the review of
// PR #464 as a test: temporal writers (afterwork and review, in turn) of a
// small set of blocks against repeated hard deletes of those blocks, each
// deleted block stored again. Writers of a deleted block report applied=false;
// no side may fail — before issue #465 a run of 45 761 operations saw 15
// deadlocks (40P01), on the writer's first insert and on the delete.
func TestTemporalWritersAgainstHardDeletes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const nBlocks, nWriters = 8, 4

	var mu sync.Mutex
	blocks := make([]*store.Block, nBlocks)
	for i := range blocks {
		blocks[i] = i465Block(t, pool, fmt.Sprintf("Load %d", i))
	}
	pick := func(i int) *store.Block {
		mu.Lock()
		defer mu.Unlock()
		return blocks[i%nBlocks]
	}

	var (
		emu    sync.Mutex
		errs   = map[string]int{}
		sample = map[string]string{}
		ops    int
	)
	record := func(err error) bool {
		emu.Lock()
		defer emu.Unlock()
		if err != nil {
			code := i465Code(err)
			if code == "" {
				code = "other"
			}
			errs[code]++
			if _, ok := sample[code]; !ok {
				sample[code] = err.Error()
			}
		}
		ops++
		return ops < i465LoadOps
	}

	var wg sync.WaitGroup
	for w := 0; w < nWriters; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			writer := i465Writers[w%len(i465Writers)]
			for i := w; ; i++ {
				_, err := writer.write(ctx, pool, pick(i))
				if !record(err) {
					return
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			old := pick(i)
			_, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1`, old.ID)
			if err == nil {
				nb, serr := store.UpsertBlock(ctx, pool, "i465", old.Title, old.Content, nil, nil, "private", false, store.SensitivityWrite{}, "")
				if serr == nil {
					mu.Lock()
					blocks[i%nBlocks] = nb
					mu.Unlock()
				}
				err = serr
			}
			if !record(err) {
				return
			}
		}
	}()
	wg.Wait()

	if len(errs) > 0 {
		codes := make([]string, 0, len(errs))
		for c, n := range errs {
			codes = append(codes, fmt.Sprintf("%s=%d (%s)", c, n, sample[c]))
		}
		sort.Strings(codes)
		t.Errorf("%d operations, errors: %s", ops, strings.Join(codes, "; "))
	}
}
