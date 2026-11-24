//go:build integration

// Issue #359: two or more guard runs at the same time (several ctx instances
// on one database) on blocks that are each other's nearest duplicate. Each
// run picks its blocks with SKIP LOCKED, so the runs never check the same
// block — but a verdict names the matched neighbour, and the neighbour can be
// a block another run has picked. Before the fix the pick held its blocks
// FOR UPDATE, and the audit row's foreign key on matched_block_id takes a
// key-share lock on the neighbour: two runs that each held one end of a pair
// waited for each other at their audit-log inserts, and PostgreSQL aborted
// one of the two inserts with 40P01 after deadlock_timeout (the verdict was
// rolled back to its savepoint and only the next run gave it).
//
// The interleavings below are held at defined statements of the production
// path (a gate transaction on a row, or a query tracer on the run's own
// pool), not left to a race. Every block is written through store.UpsertBlock
// and store.StoreEmbedding; every verdict through guard.RunGuardBatch.
//
//	go test -tags=integration -p 1 ./internal/guard/ -run TestConcurrentGuardRuns -count=1 -v
package guard_test

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// runResult is what one RunGuardBatch call returned.
type runResult struct {
	n   int
	err error
}

func startRun(ctx context.Context, pool *pgxpool.Pool, f *concurrentFixture, limit int) chan runResult {
	done := make(chan runResult, 1)
	go func() {
		n, err := guard.RunGuardBatch(ctx, pool, f.set, limit, guard.SearchIndexForTest)
		done <- runResult{n, err}
	}()
	return done
}

// stopTracer holds a guard run at one statement of its own: the first
// statement whose SQL contains sqlPart and whose first argument is blockID —
// before the statement is sent (atStart) or after its result has been read.
// The run's transaction stays open while it stands.
type stopTracer struct {
	sqlPart, blockID string
	atStart          bool
	reached, release chan struct{}
	once             sync.Once
}

type stopKey struct{}

func newStopTracer(sqlPart, blockID string, atStart bool) *stopTracer {
	return &stopTracer{sqlPart: sqlPart, blockID: blockID, atStart: atStart,
		reached: make(chan struct{}), release: make(chan struct{})}
}

func (s *stopTracer) matches(d pgx.TraceQueryStartData) bool {
	return strings.Contains(d.SQL, s.sqlPart) && len(d.Args) > 0 && fmt.Sprint(d.Args[0]) == s.blockID
}

func (s *stopTracer) stop() {
	s.once.Do(func() {
		close(s.reached)
		<-s.release
	})
}

func (s *stopTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !s.matches(d) {
		return ctx
	}
	if s.atStart {
		s.stop()
		return ctx
	}
	return context.WithValue(ctx, stopKey{}, true)
}

func (s *stopTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(stopKey{}) != nil {
		s.stop()
	}
}

func (s *stopTracer) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-s.reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("run did not reach its stop (%s, block %s) within 10 s", s.sqlPart, s.blockID)
	}
}

// tracedPool is a second pool on the test database whose connections carry tr.
func tracedPool(t *testing.T, pool *pgxpool.Pool, tr pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Tracer = tr
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

type concurrentFixture struct {
	pool *pgxpool.Pool
	set  *blocktype.Set
}

// deadlockCount reads the database's deadlock counter. A backend reports it
// to the shared statistics when it next flushes, at most once a second after
// going idle, so the read waits that long first.
func deadlockCount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	time.Sleep(1500 * time.Millisecond)
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// verdict is what the guard left on one block.
type verdict struct {
	title    string
	archived bool
	matched  string
	checked  bool
	logRows  int
}

func readVerdicts(t *testing.T, pool *pgxpool.Pool, ids []string) map[string]verdict {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT cb.id::text, cb.title, cb.is_archived, COALESCE(cb.metadata->>'guard_matched_id', ''),
		        (cb.metadata->>'guard_checked_at') IS NOT NULL,
		        (SELECT count(*)::int FROM context_write_log w WHERE w.block_id = cb.id)
		   FROM context_blocks cb WHERE cb.id = ANY($1::uuid[])`, ids)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]verdict{}
	for rows.Next() {
		var id string
		var v verdict
		if err := rows.Scan(&id, &v.title, &v.archived, &v.matched, &v.checked, &v.logRows); err != nil {
			t.Fatal(err)
		}
		out[id] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertSettled pins what "correct" means for concurrent runs, read after a
// single run has taken whatever the concurrent runs left pending: every block
// has exactly one verdict and exactly one audit row, and no two blocks are
// archived as each other's duplicate (both would be gone).
func assertSettled(t *testing.T, pool *pgxpool.Pool, ids []string) map[string]verdict {
	t.Helper()
	v := readVerdicts(t, pool, ids)
	for _, id := range ids {
		s := v[id]
		if !s.checked || s.logRows != 1 {
			t.Errorf("%s: checked=%v, audit rows=%d, want one verdict and one audit row", s.title, s.checked, s.logRows)
		}
		if !s.archived {
			continue
		}
		if m, ok := v[s.matched]; ok && m.archived && m.matched == id {
			t.Errorf("%s and %s are archived as each other's duplicate", s.title, m.title)
		}
	}
	return v
}

func archivedOf(v map[string]verdict, ids ...string) int {
	n := 0
	for _, id := range ids {
		if v[id].archived {
			n++
		}
	}
	return n
}

// drain runs the guard alone until nothing is pending.
func drain(t *testing.T, f *concurrentFixture) {
	t.Helper()
	for i := 0; i < 5; i++ {
		n, err := guard.RunGuardBatch(context.Background(), f.pool, f.set, 100, guard.SearchIndexForTest)
		if err != nil {
			t.Fatalf("drain run: %v", err)
		}
		if n == 0 {
			return
		}
	}
}

func newConcurrentFixture(t *testing.T) *concurrentFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	return &concurrentFixture{pool: pool, set: stateLockSet(t, pool)}
}

// pendingPair writes two pending blocks next to each other (each the other's
// nearest neighbour, similarity well above the 0.98 archive threshold), a
// first, b a moment later, so a pick by created_at takes a first.
func pendingPair(t *testing.T, pool *pgxpool.Pool, prefix string) (a, b string) {
	t.Helper()
	base := stateLockVec(nil, 0)
	a = stateLockBlock(t, pool, prefix+" a", stateLockVec(base, 0.05))
	time.Sleep(2 * time.Millisecond)
	b = stateLockBlock(t, pool, prefix+" b", stateLockVec(base, 0.05))
	time.Sleep(2 * time.Millisecond)
	return a, b
}

// Run X has picked x0 and A and stands at x0's audit step (a stop at its
// first statement; until issue #547 a gate holding x0's neighbour m FOR
// UPDATE stood it there, but the run now defers such a block instead of
// waiting); run Y picks B, whose neighbour A is X's. Before the fix of #359
// Y waited at its audit insert for A, X went on, reached A's audit insert and
// waited for B: 40P01.
func TestConcurrentGuardRuns_MutualPairAuditInsertsDoNotWait(t *testing.T) {
	f := newConcurrentFixture(t)
	ctx := context.Background()
	b0 := stateLockVec(nil, 0)
	m := stateLockBlock(t, f.pool, "neighbour m", b0)
	drain(t, f)
	x0 := stateLockBlock(t, f.pool, "pending x0", stateLockVec(b0, 0.1))
	time.Sleep(2 * time.Millisecond)
	a, b := pendingPair(t, f.pool, "pair")
	dl0 := deadlockCount(t, f.pool)

	_ = m
	tr := newStopTracer("SELECT title, category, scope FROM context_blocks", x0, true)
	dx := startRun(ctx, tracedPool(t, f.pool, tr), f, 2)
	tr.waitReached(t) // X at x0's audit step, holding x0 and A
	dy := startRun(ctx, f.pool, f, 1)
	var ry runResult
	yThrough := false
	select {
	case ry = <-dy:
		yThrough = true
	case <-time.After(3 * time.Second):
	}
	close(tr.release)
	rx := <-dx
	if !yThrough {
		ry = <-dy
	}
	dl1 := deadlockCount(t, f.pool)
	if !yThrough {
		t.Errorf("run Y waited for run X (B's audit insert on A, held by X's pick)")
	}
	if dl1 != dl0 {
		t.Errorf("deadlocks %d -> %d, want none", dl0, dl1)
	}
	if rx.err != nil || ry.err != nil {
		t.Errorf("run errors: X=%v Y=%v", rx.err, ry.err)
	}
	// Y decided B in its own run: its audit row names A, which X's pick
	// holds FOR NO KEY UPDATE — a lock the foreign key's key-share lock
	// passes (#359). Under a pick FOR UPDATE Y would defer B instead of
	// waiting (issue #547), so "Y went through" alone no longer pins it.
	if yThrough && ry.n != 1 {
		t.Errorf("run Y processed %d blocks while X held A, want B decided (1): its audit insert must pass X's pick", ry.n)
	}
	drain(t, f)
	v := assertSettled(t, f.pool, []string{x0, a, b})
	if n := archivedOf(v, a, b); n != 1 {
		t.Errorf("archived of the pair: %d, want exactly 1", n)
	}
}

// X stands right after ctx_guard_check(A) has named B (B not archived in its
// snapshot); Y decides B against A and commits; then X goes on. X must not
// archive A against a B that is gone by now.
func TestConcurrentGuardRuns_PairDecidedAfterTheOtherCheck(t *testing.T) {
	f := newConcurrentFixture(t)
	ctx := context.Background()
	a, b := pendingPair(t, f.pool, "pair")
	dl0 := deadlockCount(t, f.pool)

	tr := newStopTracer("ctx_guard_check", a, false)
	dx := startRun(ctx, tracedPool(t, f.pool, tr), f, 1)
	tr.waitReached(t)
	dy := startRun(ctx, f.pool, f, 1)
	var ry runResult
	yThrough := false
	select {
	case ry = <-dy:
		yThrough = true
	case <-time.After(3 * time.Second):
	}
	close(tr.release)
	rx := <-dx
	if !yThrough {
		ry = <-dy
	}
	dl1 := deadlockCount(t, f.pool)
	if !yThrough {
		t.Errorf("run Y waited for run X")
	}
	if dl1 != dl0 {
		t.Errorf("deadlocks %d -> %d, want none", dl0, dl1)
	}
	if rx.err != nil || ry.err != nil {
		t.Errorf("run errors: X=%v Y=%v", rx.err, ry.err)
	}
	drain(t, f)
	v := assertSettled(t, f.pool, []string{a, b})
	if n := archivedOf(v, a, b); n != 1 {
		t.Errorf("archived of the pair: %d, want exactly 1", n)
	}
}

// X has archived A against B and stands before A's audit insert (its
// transaction open, A's archive not visible to others); Y checks B, sees A
// live and names it. Y must not archive B against A.
func TestConcurrentGuardRuns_PairDecidedWhileTheOtherStandsUncommitted(t *testing.T) {
	f := newConcurrentFixture(t)
	ctx := context.Background()
	a, b := pendingPair(t, f.pool, "pair")
	dl0 := deadlockCount(t, f.pool)

	tr := newStopTracer("INSERT INTO context_write_log", a, true)
	dx := startRun(ctx, tracedPool(t, f.pool, tr), f, 1)
	tr.waitReached(t)
	dy := startRun(ctx, f.pool, f, 1)
	var ry runResult
	yThrough := false
	select {
	case ry = <-dy:
		yThrough = true
	case <-time.After(3 * time.Second):
	}
	close(tr.release)
	rx := <-dx
	if !yThrough {
		ry = <-dy
	}
	dl1 := deadlockCount(t, f.pool)
	if !yThrough {
		t.Errorf("run Y waited for run X")
	}
	if dl1 != dl0 {
		t.Errorf("deadlocks %d -> %d, want none", dl0, dl1)
	}
	if rx.err != nil || ry.err != nil {
		t.Errorf("run errors: X=%v Y=%v", rx.err, ry.err)
	}
	drain(t, f)
	v := assertSettled(t, f.pool, []string{a, b})
	if n := archivedOf(v, a, b); n != 1 {
		t.Errorf("archived of the pair: %d, want exactly 1", n)
	}
}

// chainBlocks writes three pending blocks a, b, c (in that order) whose
// nearest neighbours form a chain: a's is b, b's is c, c's is b. With b
// archived, c's nearest live neighbour is a, still a duplicate (0.985).
func chainBlocks(t *testing.T, pool *pgxpool.Pool) (a, b, c string) {
	t.Helper()
	const c1, c2 = 0.995, 0.99 // sim(b,c) = c1, sim(a,b) = c2, sim(a,c) = c1*c2
	s1, s2 := math.Sqrt(1-c1*c1), math.Sqrt(1-c2*c2)
	vec := func(x, y, z float64) []float32 {
		v := make([]float32, 1024)
		v[0], v[1], v[2] = float32(x), float32(y), float32(z)
		return v
	}
	a = stateLockBlock(t, pool, "chain a", vec(c2*c1, c2*s1, s2))
	time.Sleep(2 * time.Millisecond)
	b = stateLockBlock(t, pool, "chain b", vec(c1, s1, 0))
	time.Sleep(2 * time.Millisecond)
	c = stateLockBlock(t, pool, "chain c", vec(1, 0, 0))
	return a, b, c
}

// Three runs, each holding one block of a chain: A's nearest neighbour is B,
// B's is C, C's is B. (A cycle A→B→C→A of nearest neighbours cannot exist:
// it would need sim(A,B) < sim(B,C) < sim(C,A) < sim(A,B).) All three stand
// after their check, then go on together. Before the fix B's and C's runs
// waited for each other at their audit inserts, and A's run behind B's.
func TestConcurrentGuardRuns_ThreeRunsOnAChain(t *testing.T) {
	f := newConcurrentFixture(t)
	ctx := context.Background()
	a, b, c := chainBlocks(t, f.pool)
	dl0 := deadlockCount(t, f.pool)

	var dones []chan runResult
	var trs []*stopTracer
	for _, id := range []string{a, b, c} {
		tr := newStopTracer("ctx_guard_check", id, false)
		trs = append(trs, tr)
		dones = append(dones, startRun(ctx, tracedPool(t, f.pool, tr), f, 1))
		tr.waitReached(t)
	}
	for _, tr := range trs {
		close(tr.release)
	}
	for i, d := range dones {
		if r := <-d; r.err != nil {
			t.Errorf("run %d: %v", i, r.err)
		}
	}
	if dl1 := deadlockCount(t, f.pool); dl1 != dl0 {
		t.Errorf("deadlocks %d -> %d, want none", dl0, dl1)
	}
	drain(t, f)
	v := assertSettled(t, f.pool, []string{a, b, c})
	if n := archivedOf(v, a, b, c); n > 2 {
		t.Errorf("all three blocks of the chain archived")
	}
}

// An archive cycle over three blocks: X has archived A against B and stands
// before A's audit insert (uncommitted); Y archives B against C and commits;
// Z checks C, finds B gone and A live, and archives C against A. A rule that
// only keeps two blocks from being archived against each other lets all
// three go (A→B, B→C, C→A: three different pairs). On root the same
// interleaving archives all three as well: Z's audit insert waits for X's
// pick of A, and X commits.
func TestConcurrentGuardRuns_NoArchiveCycleOverThreeBlocks(t *testing.T) {
	f := newConcurrentFixture(t)
	ctx := context.Background()
	a, b, c := chainBlocks(t, f.pool)
	dl0 := deadlockCount(t, f.pool)

	tr := newStopTracer("INSERT INTO context_write_log", a, true)
	dx := startRun(ctx, tracedPool(t, f.pool, tr), f, 1)
	tr.waitReached(t)
	others := []chan runResult{startRun(ctx, f.pool, f, 1)} // Y picks B
	select {
	case r := <-others[0]:
		others[0] <- r
	case <-time.After(3 * time.Second):
	}
	others = append(others, startRun(ctx, f.pool, f, 1)) // Z picks C
	select {
	case r := <-others[1]:
		others[1] <- r
	case <-time.After(3 * time.Second):
	}
	close(tr.release)
	if r := <-dx; r.err != nil {
		t.Errorf("run X: %v", r.err)
	}
	for i, d := range others {
		if r := <-d; r.err != nil {
			t.Errorf("run %d: %v", i+1, r.err)
		}
	}
	if dl1 := deadlockCount(t, f.pool); dl1 != dl0 {
		t.Errorf("deadlocks %d -> %d, want none", dl0, dl1)
	}
	drain(t, f)
	v := assertSettled(t, f.pool, []string{a, b, c})
	if n := archivedOf(v, a, b, c); n > 2 {
		t.Errorf("all three blocks archived: a→%s b→%s c→%s", v[v[a].matched].title, v[v[b].matched].title, v[v[c].matched].title)
	}
}

// Two runs archive a block each against the same standing neighbour H (a
// hub; the two blocks are each other's duplicates too, but each is closer to
// H than to the other). X stands before its audit insert with its archive
// open; Y must still archive its block against H in the same moment instead
// of leaving it for the next run: H stays, so the two verdicts cannot form a
// cycle. On root this held as well (H is no pick of either run); it pins
// that the archive locks take the standing neighbour shared.
func TestConcurrentGuardRuns_ArchivesAgainstOneStandingBlockGoSideBySide(t *testing.T) {
	f := newConcurrentFixture(t)
	ctx := context.Background()
	const c = 0.995 // sim(s,h) = c, sim(s1,s2) = c*c
	s := math.Sqrt(1 - c*c)
	vec := func(x, y, z float64) []float32 {
		v := make([]float32, 1024)
		v[0], v[1], v[2] = float32(x), float32(y), float32(z)
		return v
	}
	h := stateLockBlock(t, f.pool, "hub h", vec(1, 0, 0))
	drain(t, f)
	s1 := stateLockBlock(t, f.pool, "spoke s1", vec(c, s, 0))
	time.Sleep(2 * time.Millisecond)
	s2 := stateLockBlock(t, f.pool, "spoke s2", vec(c, 0, s))

	tr := newStopTracer("INSERT INTO context_write_log", s1, true)
	dx := startRun(ctx, tracedPool(t, f.pool, tr), f, 1)
	tr.waitReached(t)
	var ry runResult
	select {
	case ry = <-startRun(ctx, f.pool, f, 1):
	case <-time.After(5 * time.Second):
		t.Fatal("run Y did not finish while run X stood")
	}
	close(tr.release)
	rx := <-dx
	if rx.err != nil || ry.err != nil || rx.n != 1 || ry.n != 1 {
		t.Errorf("runs: X processed=%d err=%v, Y processed=%d err=%v, want one verdict each", rx.n, rx.err, ry.n, ry.err)
	}
	v := assertSettled(t, f.pool, []string{s1, s2})
	if !v[s1].archived || !v[s2].archived || v[s1].matched != h || v[s2].matched != h {
		t.Errorf("spokes: s1 archived=%v against hub=%v, s2 archived=%v against hub=%v, want both archived against h",
			v[s1].archived, v[s1].matched == h, v[s2].archived, v[s2].matched == h)
	}
}

// Free-running: k runs started together over rounds of pending pairs laid
// out so that every pair is split across two runs, then one run alone takes
// what is left. The deadlock counter must not move.
func TestConcurrentGuardRuns_FreeRunning(t *testing.T) {
	for _, tc := range []struct {
		runs, limit, rounds int
	}{
		{runs: 2, limit: 3, rounds: 4},
		{runs: 3, limit: 2, rounds: 4},
	} {
		t.Run(fmt.Sprintf("%d_runs", tc.runs), func(t *testing.T) {
			f := newConcurrentFixture(t)
			ctx := context.Background()
			dl0 := deadlockCount(t, f.pool)
			var errs, verdicts int
			var concurrent time.Duration
			var pairs [][2]string
			for round := 0; round < tc.rounds; round++ {
				// p1 p2 p3 q1 q2 q3 by created_at, p_i next to q_i: the
				// picks split every pair across two runs.
				bases := [][]float32{stateLockVec(nil, 0), stateLockVec(nil, 0), stateLockVec(nil, 0)}
				ids := map[string]string{}
				for _, prefix := range []string{"p", "q"} {
					for i, base := range bases {
						key := fmt.Sprintf("%s%d", prefix, i+1)
						ids[key] = stateLockBlock(t, f.pool, fmt.Sprintf("%s round %d", key, round), stateLockVec(base, 0.05))
						time.Sleep(time.Millisecond)
					}
				}
				for i := 1; i <= 3; i++ {
					pairs = append(pairs, [2]string{ids[fmt.Sprintf("p%d", i)], ids[fmt.Sprintf("q%d", i)]})
				}
				var wg sync.WaitGroup
				start := make(chan struct{})
				res := make([]runResult, tc.runs)
				for k := 0; k < tc.runs; k++ {
					wg.Add(1)
					go func(k int) {
						defer wg.Done()
						<-start
						n, err := guard.RunGuardBatch(ctx, f.pool, f.set, tc.limit, guard.SearchIndexForTest)
						res[k] = runResult{n, err}
					}(k)
				}
				t0 := time.Now()
				close(start)
				wg.Wait()
				concurrent += time.Since(t0)
				for _, r := range res {
					verdicts += r.n
					if r.err != nil {
						errs++
					}
				}
				drain(t, f)
			}
			dl1 := deadlockCount(t, f.pool)
			t.Logf("%d runs × %d rounds: verdicts of the concurrent runs %d of %d blocks in %s, run errors %d, deadlocks %d -> %d",
				tc.runs, tc.rounds, verdicts, 6*tc.rounds, concurrent.Round(time.Millisecond), errs, dl0, dl1)
			if dl1 != dl0 || errs > 0 {
				t.Errorf("concurrent guard runs: deadlocks +%d, run errors %d, want none", dl1-dl0, errs)
			}
			var all []string
			for _, p := range pairs {
				all = append(all, p[0], p[1])
			}
			v := assertSettled(t, f.pool, all)
			for _, p := range pairs {
				if n := archivedOf(v, p[0], p[1]); n != 1 {
					t.Errorf("pair %s/%s: %d archived, want exactly 1", v[p[0]].title, v[p[1]].title, n)
				}
			}
		})
	}
}

// windowWidener sleeps a random moment of up to max at the end of the pick's
// ROLLBACK TO SAVEPOINT (issue #360): the window between the pick's two
// steps, in which the kept blocks are not locked, grows from a round trip to
// that, so the runs around it act inside it more often.
type windowWidener struct{ max time.Duration }

type widenKey struct{}

func (w windowWidener) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "ROLLBACK TO SAVEPOINT guard_pick") {
		return context.WithValue(ctx, widenKey{}, true)
	}
	return ctx
}

func (w windowWidener) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(widenKey{}) != nil {
		time.Sleep(time.Duration(rand.Int63n(int64(w.max))))
	}
}

// typedBlock writes a pending block of a given type through the production
// write path.
func typedBlock(t *testing.T, pool *pgxpool.Pool, title, typ string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "q360c", title, "content of "+title, nil, nil, "private", true, store.SensitivityWrite{}, typ)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, pool, b.ID, "q360", vec); err != nil {
		t.Fatal(err)
	}
	return b.ID
}

// Free-running across types (issue #360): two and four runs started
// together over rounds of duplicate pairs whose two blocks have different
// checked types, so every pick locks more than its limit in step 1 and goes
// through the window between its two steps; the window is widened and
// VACUUM runs throughout. Every pair keeps exactly one block, every block
// has one verdict and one audit row, no two blocks are archived as each
// other's duplicate (#364), no run fails and the deadlock counter does not
// move.
func TestConcurrentGuardRuns_FreeRunningAcrossTypes(t *testing.T) {
	for _, tc := range []struct{ runs, limit, rounds int }{{2, 3, 6}, {4, 2, 6}} {
		t.Run(fmt.Sprintf("%d_runs", tc.runs), func(t *testing.T) {
			f := newConcurrentFixture(t)
			ctx := context.Background()
			widened := tracedPool(t, f.pool, windowWidener{max: 3 * time.Millisecond})
			dl0 := deadlockCount(t, f.pool)
			var stop atomic.Bool
			var vacuums atomic.Int64
			vacDone := make(chan struct{})
			go func() {
				defer close(vacDone)
				for !stop.Load() {
					if _, err := f.pool.Exec(ctx, `VACUUM (INDEX_CLEANUP ON) context_blocks`); err != nil {
						t.Errorf("vacuum: %v", err)
						return
					}
					vacuums.Add(1)
					time.Sleep(5 * time.Millisecond)
				}
			}()
			var errs, verdicts int
			var pairs [][2]string
			for round := 0; round < tc.rounds; round++ {
				// p1 q1 p2 q2 p3 q3 by created_at, p_i knowledge and q_i
				// reference, each pair near-identical.
				for i := 0; i < 3; i++ {
					base := stateLockVec(nil, 0)
					p := typedBlock(t, f.pool, fmt.Sprintf("p%d round %d", i, round), "knowledge", stateLockVec(base, 0.05))
					time.Sleep(time.Millisecond)
					q := typedBlock(t, f.pool, fmt.Sprintf("q%d round %d", i, round), "reference", stateLockVec(base, 0.05))
					time.Sleep(time.Millisecond)
					pairs = append(pairs, [2]string{p, q})
				}
				var wg sync.WaitGroup
				start := make(chan struct{})
				res := make([]runResult, tc.runs)
				for k := 0; k < tc.runs; k++ {
					wg.Add(1)
					go func(k int) {
						defer wg.Done()
						<-start
						n, err := guard.RunGuardBatch(ctx, widened, f.set, tc.limit, guard.SearchIndexForTest)
						res[k] = runResult{n, err}
					}(k)
				}
				close(start)
				wg.Wait()
				for _, r := range res {
					verdicts += r.n
					if r.err != nil {
						errs++
						t.Errorf("run: %v", r.err)
					}
				}
				drain(t, f)
			}
			stop.Store(true)
			<-vacDone
			dl1 := deadlockCount(t, f.pool)
			t.Logf("%d runs × %d rounds: verdicts of the concurrent runs %d of %d blocks, run errors %d, VACUUM %d, deadlocks %d -> %d",
				tc.runs, tc.rounds, verdicts, 6*tc.rounds, errs, vacuums.Load(), dl0, dl1)
			if dl1 != dl0 {
				t.Errorf("deadlocks %d -> %d, want none", dl0, dl1)
			}
			var all []string
			for _, p := range pairs {
				all = append(all, p[0], p[1])
			}
			v := assertSettled(t, f.pool, all)
			for _, p := range pairs {
				if n := archivedOf(v, p[0], p[1]); n != 1 {
					t.Errorf("pair %s/%s: %d archived, want exactly 1", v[p[0]].title, v[p[1]].title, n)
				}
			}
		})
	}
}
