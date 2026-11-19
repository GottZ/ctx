//go:build integration

// Issue #360: the pick locks the oldest waiting blocks of each checked type
// (step 1), keeps the oldest over all types, gives every lock of step 1 back
// (ROLLBACK TO SAVEPOINT) and locks the kept blocks again by row version
// (step 2). Between the two steps the kept blocks are not locked. These
// tests hold a run in that window, on real connections, and let other runs,
// writers and VACUUM act in it; then they run guard picks side by side
// with writers and VACUUM and a widened window over many rounds. A run must
// never take a block another run holds, never a block that no longer waits,
// never a block that step 1 did not keep (a row version VACUUM freed and a
// later write reused), and no run may stay empty while blocks wait.
//
// In-package test on purpose: it runs the ACTUAL pickPending.
package guard

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
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

type windowKey struct{}

// pickTracer watches the pick's statements on a pool: it records the ids
// step 2 is asked to lock again (per connection) and acts at the end of the
// ROLLBACK TO SAVEPOINT that opens the window — it holds the first run that
// gets there until released (hold), or sleeps a random moment (widen).
type pickTracer struct {
	mu      sync.Mutex
	kept    map[*pgx.Conn][]string
	windows atomic.Int64

	hold             bool
	reached, release chan struct{}
	once             sync.Once

	widen time.Duration
}

func newPickTracer() *pickTracer {
	return &pickTracer{kept: map[*pgx.Conn][]string{}, reached: make(chan struct{}), release: make(chan struct{})}
}

func (p *pickTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "ctid = ANY") && len(d.Args) == 3 {
		if ids, ok := d.Args[2].([]string); ok {
			p.mu.Lock()
			p.kept[conn] = append([]string(nil), ids...)
			p.mu.Unlock()
		}
	}
	if strings.Contains(d.SQL, "ROLLBACK TO SAVEPOINT "+guardPickSavepoint) {
		return context.WithValue(ctx, windowKey{}, true)
	}
	return ctx
}

func (p *pickTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if ctx.Value(windowKey{}) == nil || d.Err != nil {
		return
	}
	p.windows.Add(1)
	if p.hold {
		p.once.Do(func() {
			close(p.reached)
			<-p.release
		})
	}
	if p.widen > 0 {
		time.Sleep(time.Duration(rand.Int63n(int64(p.widen))))
	}
}

// keptOf returns and forgets the ids the last step 2 on conn was asked for.
func (p *pickTracer) keptOf(conn *pgx.Conn) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.kept[conn]
	delete(p.kept, conn)
	return k
}

func tracedPickPool(t *testing.T, dsn string, tr pgx.QueryTracer, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tr
	cfg.MaxConns = maxConns
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// openPick is a pick whose transaction stays open.
type openPick struct {
	tx  pgx.Tx
	ids []string
	err error
}

func ids(bs []pendingBlock) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.id
	}
	return out
}

// startPick begins a transaction on pool and runs pickPending in it, in the
// background.
func startPick(pool *pgxpool.Pool, types []string, limit int) chan openPick {
	done := make(chan openPick, 1)
	go func() {
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			done <- openPick{err: err}
			return
		}
		bs, err := pickPending(ctx, tx, types, limit)
		done <- openPick{tx: tx, ids: ids(bs), err: err}
	}()
	return done
}

// windowFixture writes waiting blocks of two checked types through the
// production write path (store.UpsertBlock, store.StoreEmbedding), one
// millisecond apart and alternating, and returns them oldest first.
type windowFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	dsn    string
	types  []string // the allowlist of the shipped registry
	byType map[string][]string
	all    []string // oldest first
}

func newWindowFixture(t *testing.T, perType int) *windowFixture {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := &windowFixture{t: t, pool: pool, dsn: dsn, types: reg.Snapshot().GuardCheckTypes(), byType: map[string][]string{}}
	if !contains(f.types, "knowledge") || !contains(f.types, "reference") {
		t.Fatalf("fixture: the shipped registry checks %v", f.types)
	}
	for i := 0; i < perType; i++ {
		for _, typ := range []string{"knowledge", "reference"} {
			id := f.block(fmt.Sprintf("%s %d", typ, i), typ)
			f.byType[typ] = append(f.byType[typ], id)
			f.all = append(f.all, id)
			time.Sleep(time.Millisecond)
		}
	}
	return f
}

func (f *windowFixture) block(title, typ string) string {
	f.t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, f.pool, "q360w", title, "content of "+title, nil, nil, "private", true, store.SensitivityWrite{}, typ)
	if err != nil {
		f.t.Fatal(err)
	}
	v := make([]float32, 1024)
	for i := range v {
		v[i] = rand.Float32() - 0.5
	}
	if err := store.StoreEmbedding(ctx, f.pool, b.ID, "q360", v); err != nil {
		f.t.Fatal(err)
	}
	return b.ID
}

// waitingNow reports which of ids the pick's predicate holds now.
func (f *windowFixture) waitingNow(ids []string) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT id::text FROM context_blocks WHERE id = ANY($1::uuid[]) AND `+guardPendingWhere("$2")+` ORDER BY created_at`, ids, f.types)
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *windowFixture) ctidOf(id string) string {
	f.t.Helper()
	var tid string
	if err := f.pool.QueryRow(context.Background(), `SELECT ctid::text FROM context_blocks WHERE id = $1::uuid`, id).Scan(&tid); err != nil {
		f.t.Fatal(err)
	}
	return tid
}

func waitWindow(t *testing.T, tr *pickTracer) {
	t.Helper()
	select {
	case <-tr.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not reach the window between the two pick steps within 10 s")
	}
}

func recv(t *testing.T, c chan openPick) openPick {
	t.Helper()
	select {
	case r := <-c:
		if r.err != nil {
			t.Fatalf("pick: %v", r.err)
		}
		t.Cleanup(func() { _ = r.tx.Rollback(context.Background()) })
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("pick did not return within 10 s")
		return openPick{}
	}
}

func TestGuardPickWindow(t *testing.T) {
	ctx := context.Background()

	// Run A stands between its two steps with the four oldest blocks kept;
	// run B picks in that moment. B takes the oldest four (A holds none of
	// them), A's step 2 finds all four held by B and skips them: the runs
	// never share a block, and A's batch is empty this round. After B's
	// commit the next run takes the next four.
	t.Run("another_run_in_the_window", func(t *testing.T) {
		f := newWindowFixture(t, 6)
		tr := newPickTracer()
		tr.hold = true
		a := startPick(tracedPickPool(t, f.dsn, tr, 1), f.types, 4)
		waitWindow(t, tr)
		b := recv(t, startPick(f.pool, f.types, 4))
		close(tr.release)
		ra := recv(t, a)
		if !slices.Equal(b.ids, f.all[:4]) {
			t.Errorf("run B in A's window took %v, want the four oldest %v", b.ids, f.all[:4])
		}
		if len(ra.ids) != 0 {
			t.Errorf("run A took %v after B took its kept blocks, want none (B holds them)", ra.ids)
		}
		if _, err := b.tx.Exec(ctx, `UPDATE context_blocks SET metadata = metadata || jsonb_build_object('guard_checked_at', now()) WHERE id = ANY($1::uuid[])`, b.ids); err != nil {
			t.Fatal(err)
		}
		if err := b.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		_ = ra.tx.Rollback(ctx)
		c := recv(t, startPick(f.pool, f.types, 4))
		if !slices.Equal(c.ids, f.all[4:8]) {
			t.Errorf("the next run took %v, want %v", c.ids, f.all[4:8])
		}
	})

	// Writers in the window, each through its production path: the oldest
	// knowledge block archived, the oldest reference block re-typed to a type
	// with guard.check = false, the second knowledge block edited (its vector
	// goes stale, its verdict is dropped), the second reference block given a
	// verdict by another run. A takes the two kept blocks nobody touched and
	// none of the four.
	t.Run("writers_in_the_window", func(t *testing.T) {
		f := newWindowFixture(t, 8)
		k, r := f.byType["knowledge"], f.byType["reference"]
		tr := newPickTracer()
		tr.hold = true
		a := startPick(tracedPickPool(t, f.dsn, tr, 1), f.types, 6)
		waitWindow(t, tr)
		if _, err := store.DeleteBlock(ctx, f.pool, k[0], []string{"private"}); err != nil {
			t.Fatal(err)
		}
		off := "tool-evidence"
		if contains(f.types, off) {
			t.Fatalf("fixture: %s is checked", off)
		}
		if _, _, err := store.UpdateBlock(ctx, f.pool, r[0], store.UpdateBlockData{Type: &off}, []string{"private"}); err != nil {
			t.Fatal(err)
		}
		edited := "content edited in the window"
		if _, _, err := store.UpdateBlock(ctx, f.pool, k[1], store.UpdateBlockData{Content: &edited}, []string{"private"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET guard_status = 'clean', metadata = metadata || jsonb_build_object('guard_checked_at', now(), 'guard_status', 'clean') WHERE id = $1::uuid`, r[1]); err != nil {
			t.Fatal(err)
		}
		close(tr.release)
		ra := recv(t, a)
		if want := []string{k[2], r[2]}; !slices.Equal(ra.ids, want) {
			t.Errorf("run A took %v, want the two kept blocks nobody wrote %v", ra.ids, want)
		}
		if got := f.waitingNow(ra.ids); !slices.Equal(got, ra.ids) {
			t.Errorf("run A took blocks that do not wait: took %v, waiting %v", ra.ids, got)
		}
	})

	// A row version freed in the window and reused by another block. A kept
	// block X; a writer whose transaction is older than A's re-types X (a new
	// row version elsewhere, X's old version dead), VACUUM frees the old
	// version's line pointer, and an update of a waiting block Z that A did
	// not keep puts Z's new version on exactly that line pointer. A's step 2
	// finds Z at X's old row version: Z waits, but A did not keep it, so it
	// must not enter A's batch.
	t.Run("row_version_reused_after_vacuum", func(t *testing.T) {
		f := newWindowFixture(t, 6)
		// One compact heap page: no free line pointer left over from the
		// fixture's writes, so the update below can only reuse the one VACUUM
		// frees in the window.
		if _, err := f.pool.Exec(ctx, `VACUUM FULL context_blocks`); err != nil {
			t.Fatal(err)
		}
		x, z := f.all[0], f.all[8] // kept by A (limit 4) / waiting, not kept
		tidX := f.ctidOf(x)
		if page := func(tid string) string { return strings.SplitN(strings.Trim(tid, "()"), ",", 2)[0] }; page(tidX) != page(f.ctidOf(z)) {
			t.Fatalf("fixture: X at %s and Z at %s are not on one heap page", tidX, f.ctidOf(z))
		}
		// The writer's transaction id is older than A's: VACUUM may remove
		// the version it deletes while A is still open.
		w, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.Rollback(ctx) }()
		if _, err := w.Exec(ctx, `SELECT txid_current()`); err != nil {
			t.Fatal(err)
		}
		tr := newPickTracer()
		tr.hold = true
		a := startPick(tracedPickPool(t, f.dsn, tr, 1), f.types, 4)
		waitWindow(t, tr)
		if _, err := w.Exec(ctx, `UPDATE context_blocks SET type_name = 'tool-evidence' WHERE id = $1::uuid`, x); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `VACUUM (INDEX_CLEANUP ON) context_blocks`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET updated_at = updated_at + interval '1 millisecond' WHERE id = $1::uuid`, z); err != nil {
			t.Fatal(err)
		}
		if got := f.ctidOf(z); got != tidX {
			t.Fatalf("fixture: Z's new row version is at %s, not at X's freed %s — the reuse did not happen", got, tidX)
		}
		close(tr.release)
		ra := recv(t, a)
		if slices.Contains(ra.ids, z) {
			t.Errorf("run A took %s, a block it did not keep, found at the reused row version %s of %s", z, tidX, x)
		}
		// Nor does run A hold it: a writer of Z does not wait for A's batch.
		if _, err := f.pool.Exec(ctx, `SET LOCAL lock_timeout = '200ms'; UPDATE context_blocks SET updated_at = updated_at WHERE id = '`+z+`'`); err != nil {
			t.Errorf("a write of %s waits for run A, which does not take it: %v", z, err)
		}
		if slices.Contains(ra.ids, x) {
			t.Errorf("run A took %s, re-typed to a type with guard.check = false in the window", x)
		}
		if want := f.all[1:4]; !slices.Equal(ra.ids, want) {
			t.Errorf("run A took %v, want the three kept blocks that still wait %v", ra.ids, want)
		}
	})
}

// relockFixture: step 2 alone, given kept row versions whose rows no longer
// wait (or are not the kept block): the column waiting keeps every one of
// them out of the batch, and the one that still waits in.
func TestGuardPickRelockTakesOnlyKeptBlocksThatWait(t *testing.T) {
	f := newWindowFixture(t, 5)
	ctx := context.Background()
	k, r := f.byType["knowledge"], f.byType["reference"]
	set := func(id, sql string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET `+sql+` WHERE id = $1::uuid`, id); err != nil {
			t.Fatal(err)
		}
	}
	// Each change is made in place of what a writer would do; the kept row
	// version handed to step 2 is the row's current one, so only the column
	// waiting can keep it out.
	set(k[0], `is_archived = true`)
	set(k[1], `metadata = metadata || jsonb_build_object('guard_checked_at', now())`)
	set(k[2], `embedding_stale = true`)
	set(k[3], `category = 'index'`)
	set(k[4], `embedding = NULL`)
	set(r[0], `lifecycle_state = 'synthesis'`)
	set(r[1], `type_name = 'tool-evidence'`)
	kept := []lockedPick{}
	for _, id := range []string{k[0], k[1], k[2], k[3], k[4], r[0], r[1], r[2]} {
		kept = append(kept, lockedPick{tid: f.ctidOf(id), block: pendingBlock{id: id}})
	}
	// r[3]'s row version under the id of r[4]: a version that holds another
	// block than the one step 1 kept.
	kept = append(kept, lockedPick{tid: f.ctidOf(r[3]), block: pendingBlock{id: r[4]}})
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	got, err := relockKept(ctx, tx, kept, f.types)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{r[2]}; !slices.Equal(ids(got), want) {
		t.Errorf("step 2 took %v, want only %v (archived, checked, stale vector, category index, no vector, lifecycle synthesis, unchecked type and another block's row version stay out)", ids(got), want)
	}
	// Another block's row version is not even locked: its writer goes on.
	if _, err := f.pool.Exec(ctx, `SET LOCAL lock_timeout = '200ms'; UPDATE context_blocks SET updated_at = updated_at WHERE id = '`+r[3]+`'`); err != nil {
		t.Errorf("a write of %s waits for step 2, which did not keep it: %v", r[3], err)
	}
}

// pickRunStats is what one guard run loop saw.
type pickRunStats struct {
	picks, taken, empty, emptyStreakMax int
	relocks, lostInWindow               int
}

// TestGuardPickConcurrentRuns: two and four guard picks side by side on
// real connections, over many rounds, while writers archive, re-type, edit
// and re-embed waiting blocks and write new ones, VACUUM runs every few
// milliseconds, and the window between the two pick steps is widened by up
// to two milliseconds. Each run holds its batch a moment, writes its
// verdicts and commits. No block is in two open batches, every block a run
// takes waits (checked under the run's lock), a run that went through
// step 2 takes only blocks step 1 kept, no statement fails, the deadlock
// counter does not move, no run stays empty while many blocks wait, and in
// the end no block waits.
func TestGuardPickConcurrentRuns(t *testing.T) {
	for _, runs := range []int{2, 4} {
		t.Run(fmt.Sprintf("%d_runs", runs), func(t *testing.T) {
			concurrentPicks(t, runs)
		})
	}
}

func concurrentPicks(t *testing.T, runs int) {
	// Three checked types with many waiting blocks each and a small batch
	// limit: every pick locks more than its limit in step 1 and goes
	// through the window while the backlog lasts, and the writers keep
	// putting checked blocks back into it.
	const (
		perType = 400
		limit   = 10
		churn   = 15 * time.Second
	)
	f := newWindowFixture(t, perType)
	ctx := context.Background()
	for i := 0; i < perType; i++ {
		id := f.block(fmt.Sprintf("audit %d", i), "audit-trail")
		f.all = append(f.all, id)
	}
	// The HNSW index is not read by the pick; without it VACUUM and the
	// writers run several times faster in this throwaway database.
	if _, err := f.pool.Exec(ctx, `DROP INDEX IF EXISTS idx_embedding_hnsw`); err != nil {
		t.Fatal(err)
	}
	var deadlocks0 int64
	if err := f.pool.QueryRow(ctx, `SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`).Scan(&deadlocks0); err != nil {
		t.Fatal(err)
	}
	tr := newPickTracer()
	tr.widen = 2 * time.Millisecond
	runPool := tracedPickPool(t, f.dsn, tr, int32(runs))
	wcfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	wcfg.ConnConfig.RuntimeParams["lock_timeout"] = "300ms"
	writers, err := pgxpool.NewWithConfig(ctx, wcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer writers.Close()

	var (
		mu      sync.Mutex
		held    = map[string]int{} // block -> run holding it in an open batch
		fails   []string
		waiting atomic.Int64
		stop    atomic.Bool
		churnOn atomic.Bool
	)
	fail := func(format string, args ...any) {
		mu.Lock()
		fails = append(fails, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	countWaiting := func() int64 {
		var n int64
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE `+guardPendingWhere("$1"), f.types).Scan(&n); err != nil {
			fail("count waiting: %v", err)
		}
		return n
	}
	waiting.Store(countWaiting())
	churnOn.Store(true)

	var bg sync.WaitGroup
	// The waiting count, for the starvation check.
	bg.Add(1)
	go func() {
		defer bg.Done()
		for !stop.Load() {
			waiting.Store(countWaiting())
			time.Sleep(20 * time.Millisecond)
		}
	}()
	// VACUUM every few milliseconds while the writers run.
	var vacuums atomic.Int64
	bg.Add(1)
	go func() {
		defer bg.Done()
		for churnOn.Load() {
			if _, err := f.pool.Exec(ctx, `VACUUM (INDEX_CLEANUP ON) context_blocks`); err != nil {
				fail("vacuum: %v", err)
			}
			vacuums.Add(1)
			time.Sleep(time.Duration(5+rand.Intn(15)) * time.Millisecond)
		}
	}()
	// Two writers through the production write paths.
	var actions, writerTimeouts atomic.Int64
	writeOne := func(rng *rand.Rand) error {
		id := f.all[rng.Intn(len(f.all))]
		scopes := []string{"private"}
		switch rng.Intn(6) {
		case 0: // archive — rarely, the blocks are a finite set
			if rng.Intn(4) == 0 {
				_, err := store.DeleteBlock(ctx, writers, id, scopes)
				return err
			}
			return nil
		case 1: // re-type to a type the guard does not check, and back
			off, back := "tool-evidence", []string{"knowledge", "reference"}[rng.Intn(2)]
			if _, _, err := store.UpdateBlock(ctx, writers, id, store.UpdateBlockData{Type: &off}, scopes); err != nil {
				return err
			}
			_, _, err := store.UpdateBlock(ctx, writers, id, store.UpdateBlockData{Type: &back}, scopes)
			return err
		case 2: // edit: the vector goes stale, the verdict is dropped; then,
			// mostly, the backfill's new vector, and the block waits again
			c := fmt.Sprintf("edited %d", rng.Int())
			if _, _, err := store.UpdateBlock(ctx, writers, id, store.UpdateBlockData{Content: &c}, scopes); err != nil || rng.Intn(4) == 0 {
				return err
			}
			v := make([]float32, 1024)
			for i := range v {
				v[i] = rng.Float32() - 0.5
			}
			return store.StoreEmbedding(ctx, writers, id, "q360", v)
		case 3: // re-embed: a stale block waits again
			v := make([]float32, 1024)
			for i := range v {
				v[i] = rng.Float32() - 0.5
			}
			return store.StoreEmbedding(ctx, writers, id, "q360", v)
		case 4: // an update in place, which moves the row version on its page
			_, err := writers.Exec(ctx, `UPDATE context_blocks SET updated_at = updated_at + interval '1 millisecond' WHERE id = $1::uuid`, id)
			return err
		default: // a new waiting block
			b, err := store.UpsertBlock(ctx, writers, "q360w", fmt.Sprintf("new %d", rng.Int()), "new", nil, nil, "private", true, store.SensitivityWrite{}, "reference")
			if err != nil {
				return err
			}
			v := make([]float32, 1024)
			for i := range v {
				v[i] = rng.Float32() - 0.5
			}
			return store.StoreEmbedding(ctx, writers, b.ID, "q360", v)
		}
	}
	for w := 0; w < 2; w++ {
		bg.Add(1)
		go func(seed int64) {
			defer bg.Done()
			rng := rand.New(rand.NewSource(seed))
			for churnOn.Load() {
				err := writeOne(rng)
				var pgErr *pgconn.PgError
				switch {
				case err == nil:
					actions.Add(1)
				case errors.As(err, &pgErr) && pgErr.Code == "55P03":
					writerTimeouts.Add(1)
				case errors.As(err, &pgErr) && pgErr.Code == "40P01":
					fail("writer deadlock: %v", err)
				case strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "archived"):
					// the block was archived by the other writer
				default:
					fail("writer: %v", err)
				}
			}
		}(int64(w + 1))
	}

	// The runs.
	stats := make([]pickRunStats, runs)
	var rw sync.WaitGroup
	for k := 0; k < runs; k++ {
		rw.Add(1)
		go func(k int) {
			defer rw.Done()
			st := &stats[k]
			streak, quietRounds := 0, 0
			for {
				conn, err := runPool.Acquire(ctx)
				if err != nil {
					fail("run %d acquire: %v", k, err)
					return
				}
				plenty := waiting.Load() > int64((runs+1)*limit)
				tx, err := conn.Begin(ctx)
				if err != nil {
					conn.Release()
					fail("run %d begin: %v", k, err)
					return
				}
				bs, err := pickPending(ctx, tx, f.types, limit)
				if err != nil {
					_ = tx.Rollback(ctx)
					conn.Release()
					fail("run %d pick: %v", k, err)
					return
				}
				taken := ids(bs)
				st.picks++
				st.taken += len(taken)
				if kept := tr.keptOf(conn.Conn()); kept != nil {
					st.relocks++
					st.lostInWindow += len(kept) - len(taken)
					for _, id := range taken {
						if !slices.Contains(kept, id) {
							fail("run %d took %s, which step 1 did not keep", k, id)
						}
					}
				}
				mu.Lock()
				for _, id := range taken {
					if other, ok := held[id]; ok {
						fails = append(fails, fmt.Sprintf("block %s in the open batches of runs %d and %d", id, other, k))
					}
					held[id] = k
				}
				mu.Unlock()
				if len(taken) > 0 {
					var n int
					if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE id = ANY($1::uuid[]) AND `+guardPendingWhere("$2"), taken, f.types).Scan(&n); err != nil {
						fail("run %d recheck: %v", k, err)
					} else if n != len(taken) {
						fail("run %d took %d blocks, %d of them wait under its lock", k, len(taken), n)
					}
				}
				time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
				if len(taken) > 0 {
					if _, err := tx.Exec(ctx, `UPDATE context_blocks SET guard_status = 'clean', metadata = metadata || jsonb_build_object('guard_checked_at', now(), 'guard_status', 'clean') WHERE id = ANY($1::uuid[])`, taken); err != nil {
						fail("run %d verdicts: %v", k, err)
					}
				}
				mu.Lock()
				for _, id := range taken {
					delete(held, id)
				}
				mu.Unlock()
				if err := tx.Commit(ctx); err != nil {
					fail("run %d commit: %v", k, err)
				}
				conn.Release()
				if len(taken) == 0 {
					st.empty++
					if plenty {
						streak++
						st.emptyStreakMax = max(st.emptyStreakMax, streak)
					}
				} else {
					streak = 0
				}
				if churnOn.Load() {
					continue
				}
				// After the churn: go on until three rounds in a row took
				// nothing and nothing waits.
				if len(taken) == 0 {
					quietRounds++
				} else {
					quietRounds = 0
				}
				if quietRounds >= 3 && waiting.Load() == 0 {
					return
				}
				if stop.Load() {
					return
				}
			}
		}(k)
	}
	time.Sleep(churn)
	churnOn.Store(false)
	done := make(chan struct{})
	go func() { rw.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		fail("the runs did not drain the waiting blocks within 60 s after the writers stopped (%d waiting)", waiting.Load())
	}
	stop.Store(true)
	<-done
	bg.Wait()

	time.Sleep(1500 * time.Millisecond) // the deadlock counter is flushed when a backend goes idle
	var deadlocks1 int64
	if err := f.pool.QueryRow(ctx, `SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`).Scan(&deadlocks1); err != nil {
		t.Fatal(err)
	}
	left := countWaiting()
	for k, st := range stats {
		t.Logf("run %d: %d picks, %d blocks taken, %d empty picks (longest run of empty picks while more than %d waited: %d), %d picks through step 2, %d kept blocks lost in the window",
			k, st.picks, st.taken, st.empty, (runs+1)*limit, st.emptyStreakMax, st.relocks, st.lostInWindow)
		if st.taken == 0 {
			fail("run %d took nothing in %d picks", k, st.picks)
		}
		if st.emptyStreakMax >= 10 {
			fail("run %d took nothing %d times in a row while more than %d blocks waited", k, st.emptyStreakMax, (runs+1)*limit)
		}
	}
	t.Logf("%d runs: windows %d, VACUUM %d, writer actions %d (lock timeouts %d), deadlocks %d -> %d, waiting at the end %d",
		runs, tr.windows.Load(), vacuums.Load(), actions.Load(), writerTimeouts.Load(), deadlocks0, deadlocks1, left)
	if deadlocks1 != deadlocks0 {
		fail("deadlocks %d -> %d", deadlocks0, deadlocks1)
	}
	if left != 0 {
		fail("%d blocks still wait after the runs drained", left)
	}
	relocks := 0
	for _, st := range stats {
		relocks += st.relocks
	}
	if relocks < 100 {
		fail("only %d picks went through step 2: the fixture does not test the window", relocks)
	}
	for _, f := range fails {
		t.Error(f)
	}
}
