//go:build integration

// Issue #341: no block writer waits on context_guard_state. Before migration
// 181 every counted block write took the one state row and held it to commit,
// so a second writer queued behind any open block write, a guard run held all
// block writers for its whole duration, and the run closed a deadlock cycle
// with a writer over the audit log's foreign key. Every write below goes
// through the production paths (store.StoreEmbedding, guard.RunGuardBatch).
//
//	go test -tags=integration -p 1 ./internal/guard/ -run TestGuardState -count=1 -v
package guard_test

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func stateLockVec(base []float32, noise float32) []float32 {
	v := make([]float32, 1024)
	for i := range v {
		if base != nil {
			v[i] = base[i] + (rand.Float32()-0.5)*noise
		} else {
			v[i] = rand.Float32() - 0.5
		}
	}
	return v
}

func stateLockBlock(t *testing.T, pool *pgxpool.Pool, title string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "q341l", title, "content of "+title, nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if vec != nil {
		if err := store.StoreEmbedding(ctx, pool, b.ID, "q341", vec); err != nil {
			t.Fatal(err)
		}
	}
	return b.ID
}

func stateLockSet(t *testing.T, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	return reg.Snapshot()
}

// writeWithLockTimeout writes a vector for blockID in its own transaction
// under lock_timeout = 500ms and returns the error of the write.
func writeWithLockTimeout(ctx context.Context, pool *pgxpool.Pool, blockID string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
			return err
		}
		return store.StoreEmbedding(ctx, tx, blockID, "q341", stateLockVec(nil, 0))
	})
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Pflichttest 1: an open block write does not hold a second block writer.
func TestGuardState_OpenBlockWriteDoesNotHoldOtherWriters(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	a := stateLockBlock(t, pool, "writer a", nil)
	b := stateLockBlock(t, pool, "writer b", nil)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.StoreEmbedding(ctx, tx, a, "q341", stateLockVec(nil, 0)); err != nil {
		t.Fatal(err)
	}
	if err := writeWithLockTimeout(ctx, pool, b); err != nil {
		t.Fatalf("a second block writer behind an open block write: %v (SQLSTATE %s), want no wait", err, sqlState(err))
	}
}

// Pflichttest 2: a guard run that stands still (here at its audit step, held
// by a stop on its first statement; until issue #547 a third transaction
// holding the matched neighbour FOR UPDATE stood it there, but the run now
// defers such a block) does not hold an unrelated block writer.
func TestGuardState_StandingGuardRunDoesNotHoldWriters(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := stateLockSet(t, pool)
	base := stateLockVec(nil, 0)
	m := stateLockBlock(t, pool, "neighbour m", base)
	if _, err := guard.RunGuardBatch(ctx, pool, set, 100, guard.SearchIndexForTest); err != nil {
		t.Fatal(err)
	}
	p := stateLockBlock(t, pool, "pending p", stateLockVec(base, 0.2))
	z := stateLockBlock(t, pool, "unrelated z", nil)

	_ = m
	tr := newStopTracer("SELECT title, category, scope FROM context_blocks", p, true)
	runPool := tracedPool(t, pool, tr)
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, err := guard.RunGuardBatch(ctx, runPool, set, 100, guard.SearchIndexForTest)
		done <- res{n, err}
	}()
	tr.waitReached(t) // the run stands at p's audit step

	werr := writeWithLockTimeout(ctx, pool, z)
	close(tr.release)
	r := <-done
	if werr != nil {
		t.Errorf("an unrelated block writer during a standing guard run: %v (SQLSTATE %s), want no wait", werr, sqlState(werr))
	}
	if r.err != nil || r.n != 1 {
		t.Errorf("guard run: processed=%d err=%v, want 1 (p=%s)", r.n, r.err, p)
	}
}

// deadlockFixture: m1 and m2 are checked neighbours, p1 and p2 pending blocks
// next to them (p1 picked first).
type deadlockFixture struct {
	pool                   *pgxpool.Pool
	set                    *blocktype.Set
	m1, m2, p1, p2 string
}

func newDeadlockFixture(t *testing.T) *deadlockFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := stateLockSet(t, pool)
	b1, b2 := stateLockVec(nil, 0), stateLockVec(nil, 0)
	f := &deadlockFixture{pool: pool, set: set}
	f.m1 = stateLockBlock(t, pool, "neighbour m1", b1)
	f.m2 = stateLockBlock(t, pool, "neighbour m2", b2)
	if _, err := guard.RunGuardBatch(ctx, pool, set, 100, guard.SearchIndexForTest); err != nil {
		t.Fatal(err)
	}
	f.p1 = stateLockBlock(t, pool, "pending p1", stateLockVec(b1, 0.2))
	time.Sleep(2 * time.Millisecond)
	f.p2 = stateLockBlock(t, pool, "pending p2", stateLockVec(b2, 0.2))
	return f
}

func (f *deadlockFixture) writeLogRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_write_log WHERE block_id = ANY($1::uuid[])`, []string{f.p1, f.p2}).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Pflichttest 3, the guard meets a writer of the neighbour: a writer locks the
// neighbour m2 FOR UPDATE, the guard reaches its audit step for p2, then the
// writer writes m2. Before 181 the guard held the state row from its verdict
// and the writer needed it: 40P01, the guard's verdict lost. Since issue #547
// the guard does not wait for m2 at all: p2 stays pending, and the next run
// writes its verdict.
func TestGuardState_GuardAuditInsertAndNeighbourWriterDoNotDeadlock(t *testing.T) {
	f := newDeadlockFixture(t)
	ctx := context.Background()
	w, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Rollback(ctx) }()
	if _, err := w.Exec(ctx, `SELECT content FROM context_blocks WHERE id = $1 FOR UPDATE`, f.m2); err != nil {
		t.Fatal(err)
	}
	n, rerr := guard.RunGuardBatch(ctx, f.pool, f.set, 100, guard.SearchIndexForTest)
	_, werr := w.Exec(ctx, `UPDATE context_blocks SET content = content || ' changed' WHERE id = $1`, f.m2)
	if werr == nil {
		werr = w.Commit(ctx)
	}
	if werr != nil {
		t.Errorf("writer: %v (SQLSTATE %s), want through", werr, sqlState(werr))
	}
	if rerr != nil || n != 1 {
		t.Errorf("guard run beside the held neighbour: processed=%d err=%v, want 1 (p2 deferred)", n, rerr)
	}
	if n, err := guard.RunGuardBatch(ctx, f.pool, f.set, 100, guard.SearchIndexForTest); err != nil || n != 1 {
		t.Errorf("the next run: processed=%d err=%v, want p2", n, err)
	}
	if n := f.writeLogRows(t); n != 2 {
		t.Errorf("write-log rows for p1/p2: %d, want 2", n)
	}
}

// Pflichttest 3, the writer waits first (the client write was the victim
// before 181): the guard has written p1's verdict (state row held) and stands
// at p1's audit-log insert behind a gate on m1; the writer locks m2 and then
// writes a block, which needed the state row; the gate opens, the guard
// reaches p2's audit-log insert and waits for m2. The writer had waited
// longer, so its deadlock check ran first and it was the one aborted. Since
// issue #547 a stop on the guard's first audit statement stands it at p1 (a
// gate on m1 now defers p1 instead), and the guard defers p2 while the writer
// holds m2; every verdict is written by the next run at the latest.
func TestGuardState_ClientWriteIsNoDeadlockVictim(t *testing.T) {
	f := newDeadlockFixture(t)
	ctx := context.Background()
	tr := newStopTracer("SELECT title, category, scope FROM context_blocks", f.p1, true)
	runPool := tracedPool(t, f.pool, tr)
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, err := guard.RunGuardBatch(ctx, runPool, f.set, 100, guard.SearchIndexForTest)
		done <- res{n, err}
	}()
	tr.waitReached(t) // guard at p1's audit step, p1 and p2 picked

	w, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Rollback(ctx) }()
	if _, err := w.Exec(ctx, `SELECT content FROM context_blocks WHERE id = $1 FOR UPDATE`, f.m2); err != nil {
		t.Fatal(err)
	}
	wdone := make(chan error, 1)
	go func() {
		_, err := w.Exec(ctx, `UPDATE context_blocks SET content = content || ' changed' WHERE id = $1`, f.m2)
		if err == nil {
			err = w.Commit(ctx)
		}
		wdone <- err
	}()
	// Before 181 the writer now waits for the state row; after it, it is
	// through. Either way give it a moment, then let the guard go on.
	time.Sleep(150 * time.Millisecond)
	close(tr.release)
	werr := <-wdone
	r := <-done
	if werr != nil {
		t.Errorf("client write: %v (SQLSTATE %s), want through", werr, sqlState(werr))
	}
	if r.err != nil || r.n < 1 {
		t.Errorf("guard run: processed=%d err=%v, want p1 (and p2 unless the writer still held m2: deferred, issue #547)", r.n, r.err)
	}
	if _, err := guard.RunGuardBatch(ctx, f.pool, f.set, 100, guard.SearchIndexForTest); err != nil {
		t.Errorf("the next run: %v", err)
	}
	if n := f.writeLogRows(t); n != 2 {
		t.Errorf("write-log rows for p1/p2: %d, want 2", n)
	}
}
