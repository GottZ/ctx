//go:build integration

// Issue #211 for the guard run: a block batch of UPDATE items over blocks the
// run holds. The run picks its blocks FOR NO KEY UPDATE and holds them for its
// whole batch; at the audit insert of a verdict the foreign key takes the
// key-share lock on the matched neighbour. A batch that holds the neighbour and
// waits for the picked block closes a lock cycle unless its row lock lets that
// key-share lock through: an update item's row is locked FOR NO KEY UPDATE
// (store's lockBatchUpdate), the lock the UPDATE itself takes, which does not
// conflict with a key-share lock (issue #284 measured 97–100 of 200 rounds in
// 40P01 while batch rows were locked FOR UPDATE).
//
// Every block goes through store.UpsertBlock and store.StoreEmbedding, every
// verdict through guard.RunGuardBatch, every batch through store.UpsertBlocks
// with update items, the baseline through store.UpdateBlock.
//
//	go test -tags=integration -p 1 ./internal/guard/ -run 'TestGuard(SupersedesRunAndABatchOfUpdateItems|RunBesideUpdateItemsDeadlockCount)' -count=1 -v
package guard_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
)

// gbuUpdate is an update item of block id in the fixture's scope.
func gbuUpdate(id string, data store.UpdateBlockData) store.BatchUpsert {
	return store.BatchUpsert{Update: &store.BatchUpdate{ID: id, Data: data, WriteScopes: []string{"private"}}}
}

func gbuContent(id, content string) store.BatchUpsert {
	return gbuUpdate(id, store.UpdateBlockData{Content: &content})
}

func gbuContentOf(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var content string
	if err := pool.QueryRow(context.Background(), `SELECT content FROM context_blocks WHERE id = $1`, id).Scan(&content); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return content
}

// TestGuardSupersedesRunAndABatchOfUpdateItems: the run holds X (picked) and
// stands before X's audit insert, whose foreign key takes the key-share lock
// on X's match M. A batch of update items over M and X — in both list orders;
// it locks by identity, M ("g284 batch a") before X — locks M and waits for X.
// Neither may end with 40P01: the run goes through (a 40P01 at its audit
// insert would roll the verdict back to its savepoint, so X would carry no
// audit row and no verdict), then the batch, whose content both blocks hold.
func TestGuardSupersedesRunAndABatchOfUpdateItems(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "list M, X"
		if reverse {
			name = "list X, M"
		}
		t.Run(name, func(t *testing.T) {
			f := newG284Fixture(t)
			ctx := context.Background()
			m := f.block(t, "g284 batch a", g284Neighbour)
			f.run(t, f.pool)
			x := f.block(t, "g284 batch b", g284Review)
			items := []store.BatchUpsert{gbuContent(m, "g284 batch a changed"), gbuContent(x, "g284 batch b changed")}
			if reverse {
				items[0], items[1] = items[1], items[0]
			}

			batchDone := make(chan struct{})
			var batchErr error
			started := false
			runPool := f.hooked(t, atAuditInsertOf(x, func(pid uint32) {
				started = true
				go func() {
					defer close(batchDone)
					_, batchErr = store.UpsertBlocks(ctx, f.pool, items, "", f.set, nil)
				}()
				if !waitedOn(t, f.pool, pid, batchDone) {
					t.Errorf("the batch did not wait for the run's block")
				}
			}))
			_, runErr := guard.RunGuardBatch(ctx, runPool, f.set, 50, guard.SearchIndexForTest)
			if !started {
				t.Fatalf("the run never reached the audit insert of X")
			}
			<-batchDone
			if runErr != nil || batchErr != nil {
				t.Errorf("run = %v, batch = %v (deadlock: %v), want both through", runErr, batchErr, deadlock(batchErr))
			}
			if s := f.state(t, x); s.auditRows != 1 || s.guardStatus != "needs_review" {
				t.Errorf("X after the run: %+v, want needs_review with one audit row", s)
			}
			if got := gbuContentOf(t, f.pool, m); got != "g284 batch a changed" {
				t.Errorf("M holds %q, want the batch's content", got)
			}
			if got := gbuContentOf(t, f.pool, x); got != "g284 batch b changed" {
				t.Errorf("X holds %q, want the batch's content", got)
			}
		})
	}
}

// gbuCountRounds is how many rounds the count runs in each mode.
const gbuCountRounds = 200

// gbuRoundBound bounds one round of the count: a round still running then is
// a hang, and its parties' context ends it (counted as an error).
const gbuRoundBound = 30 * time.Second

// gbuTracer counts the statements of a pool that ended in 40P01 — for the run
// as well, whose verdict savepoint swallows the error. With a monitor it also
// reads, from that third connection, right before each audit-log insert of
// the run (the statement whose foreign key takes the key-share lock on the
// matched neighbour), whether the writer's backend (application_name
// gbuWriterName) waits for a row lock: the moment a batch that holds the
// neighbour and waits for the run's block would close the cycle.
type gbuTracer struct {
	monitor     *pgxpool.Pool
	deadlocks   atomic.Int64
	writerWaits atomic.Int64
}

// gbuWriterName is the application_name of the writer's connection.
const gbuWriterName = "gbu-writer"

func (tr *gbuTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if tr.monitor == nil || !strings.Contains(d.SQL, "INSERT INTO context_write_log") {
		return ctx
	}
	var waits bool
	if err := tr.monitor.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
		  WHERE application_name = $1 AND datname = current_database() AND wait_event_type = 'Lock')`, gbuWriterName).Scan(&waits); err == nil && waits {
		tr.writerWaits.Add(1)
	}
	return ctx
}

func (tr *gbuTracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, e pgx.TraceQueryEndData) {
	if deadlock(e.Err) {
		tr.deadlocks.Add(1)
	}
}

// gbuActor is a one-connection pool on the fixture's database named name, with
// tracer.
func gbuActor(t *testing.T, f *g284Fixture, name string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	cfg.ConnConfig.Tracer = tracer
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// gbuDrain runs the guard alone until nothing is pending.
func gbuDrain(t *testing.T, f *g284Fixture) {
	t.Helper()
	for i := 0; i < 20; i++ {
		n, err := guard.RunGuardBatch(context.Background(), f.pool, f.set, 100, guard.SearchIndexForTest)
		if err != nil {
			t.Fatalf("drain run: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatalf("the guard still had pending blocks after 20 drain runs")
}

// gbuTally is one mode's outcome over the rounds of the count. checkedInRace
// counts the rounds whose run checked X (it picked X before the writer locked
// it), writerWaited the rounds in which the writer was seen waiting for a
// lock at X's audit insert.
type gbuTally struct {
	runDeadlocks, writerDeadlocks int
	runErrors, writerErrors       int
	checkedInRace, writerWaited   int
	deadRounds                    []int
	firstError                    string
	took                          time.Duration
}

func (g *gbuTally) note(r int, what string, err error) {
	if g.firstError == "" {
		g.firstError = fmt.Sprintf("round %d, %s: %v", r, what, err)
	}
}

// TestGuardRunBesideUpdateItemsDeadlockCount counts 40P01 over 200 rounds of a
// guard run beside a writer of the run's block X and of X's match M, both
// started at once. The writer is the head — one store.UpsertBlocks with both
// tag updates as update items, list order M, X and X, M by turns — or the
// basis — the same two updates as two store.UpdateBlock calls one after the
// other, which never hold two rows at once. Every round has its own M (a unit
// vector on an axis of its own, checked before the rounds) and X (cosine 0.96
// to its M: needs_review, M the matched neighbour, so the audit insert of X
// takes the key-share lock on M); tags keep both vectors current and X
// pending. A 40P01 is counted at the statement on both connections (the run's
// verdict savepoint swallows it) and in the answers. Right before X's audit
// insert a third connection reads whether the writer waits for a lock — in
// the head mode the batch then holds M and waits for X, the interleaving of
// the cycle. A round whose run did not check X — the writer held X at the
// pick, which skips locked rows — is left to a run alone before the next
// round. Asserted: no 40P01 and no error in the head mode; the basis is
// reported.
func TestGuardRunBesideUpdateItemsDeadlockCount(t *testing.T) {
	f := newG284Fixture(t)
	ctx := context.Background()
	modes := []string{"basis", "head"}
	ms := make([][]string, len(modes))
	for mi, mode := range modes {
		ms[mi] = make([]string, gbuCountRounds)
		for r := range ms[mi] {
			ms[mi][r] = f.block(t, fmt.Sprintf("g284 count %s %03d m", mode, r), g284Vec(mi*gbuCountRounds+r, 1, 0, 0))
		}
	}
	gbuDrain(t, f)

	runTr, writerTr := &gbuTracer{monitor: f.pool}, &gbuTracer{}
	runPool := gbuActor(t, f, "gbu-run", runTr)
	writerPool := gbuActor(t, f, gbuWriterName, writerTr)
	table := []string{fmt.Sprintf("%-5s %6s %11s %14s %10s %13s %17s %22s %9s",
		"mode", "rounds", "40P01 run", "40P01 writer", "run errs", "writer errs", "X checked in race",
		"writer waits at audit", "time")}
	for mi, mode := range modes {
		head := mode == "head"
		var g gbuTally
		start := time.Now()
		for r := 0; r < gbuCountRounds; r++ {
			m := ms[mi][r]
			x := f.block(t, fmt.Sprintf("g284 count %s %03d x", mode, r), g284Vec(mi*gbuCountRounds+r, 0.96, 1000, 0.28))
			ids := []string{m, x}
			if r%2 == 1 {
				ids = []string{x, m}
			}
			tags := store.UpdateBlockData{Tags: []string{fmt.Sprintf("round %d", r)}}
			writer := func(ctx context.Context) error {
				if head {
					_, err := store.UpsertBlocks(ctx, writerPool, []store.BatchUpsert{gbuUpdate(ids[0], tags), gbuUpdate(ids[1], tags)}, "", f.set, nil)
					return err
				}
				for _, id := range ids {
					if _, _, err := store.UpdateBlock(ctx, writerPool, id, tags, []string{"private"}); err != nil {
						return err
					}
				}
				return nil
			}

			beforeRun, beforeWriter, beforeWaits := runTr.deadlocks.Load(), writerTr.deadlocks.Load(), runTr.writerWaits.Load()
			rctx, cancel := context.WithTimeout(ctx, gbuRoundBound)
			begin := make(chan struct{})
			var wg sync.WaitGroup
			var processed int
			var runErr, writerErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-begin
				processed, runErr = guard.RunGuardBatch(rctx, runPool, f.set, 50, guard.SearchIndexForTest)
			}()
			go func() {
				defer wg.Done()
				<-begin
				writerErr = writer(rctx)
			}()
			close(begin)
			wg.Wait()
			cancel()

			runDeadlocked := runTr.deadlocks.Load() > beforeRun || deadlock(runErr)
			writerDeadlocked := writerTr.deadlocks.Load() > beforeWriter || deadlock(writerErr)
			if runTr.writerWaits.Load() > beforeWaits {
				g.writerWaited++
			}
			if runDeadlocked {
				g.runDeadlocks++
			}
			if writerDeadlocked {
				g.writerDeadlocks++
			}
			if runDeadlocked || writerDeadlocked {
				g.deadRounds = append(g.deadRounds, r)
			}
			if runErr != nil && !deadlock(runErr) {
				g.runErrors++
				g.note(r, "run", runErr)
			}
			if writerErr != nil && !writerDeadlocked {
				g.writerErrors++
				g.note(r, "writer", writerErr)
			}
			if s := f.state(t, x); s.checked {
				if processed == 1 {
					g.checkedInRace++
				}
			} else {
				gbuDrain(t, f)
			}
		}
		g.took = time.Since(start)
		row := fmt.Sprintf("%-5s %6d %11d %14d %10d %13d %17d %22d %9s", mode, gbuCountRounds, g.runDeadlocks, g.writerDeadlocks,
			g.runErrors, g.writerErrors, g.checkedInRace, g.writerWaited, g.took.Round(time.Millisecond))
		table = append(table, row)
		t.Log(row)
		if g.firstError != "" {
			t.Logf("%s, first error: %s", mode, g.firstError)
		}
		if head && (g.runDeadlocks > 0 || g.writerDeadlocks > 0) {
			t.Errorf("head: 40P01 in %d of %d rounds (run %d, batch %d; rounds %v)",
				len(g.deadRounds), gbuCountRounds, g.runDeadlocks, g.writerDeadlocks, g.deadRounds)
		}
		if head && (g.runErrors > 0 || g.writerErrors > 0) {
			t.Errorf("head: %d run errors and %d batch errors (%s)", g.runErrors, g.writerErrors, g.firstError)
		}
	}
	t.Logf("40P01 count, guard run beside a writer of its block and its match, %d rounds per mode:\n%s",
		gbuCountRounds, strings.Join(table, "\n"))
}
