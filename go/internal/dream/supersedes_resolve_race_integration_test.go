//go:build integration

// Issue #284:
//
//   - point 2, guard batch resolve against a dream batch: the batch's link
//     inserts take the foreign key's key-share lock on every block they link,
//     and its next insert waits for the next block. A guard resolve batch that
//     locked its blocks FOR UPDATE (which conflicts with key share) held the
//     lower block and waited for the higher one the dream batch had linked,
//     while the dream batch's next insert waited for the lower one: 40P01;
//   - point 4, sequential: a declaration settles a target; the operator's
//     confirm of another source's dream guess on the same target pinned it
//     without the declaration's already-superseded gate, and two blocks
//     settled one target. The confirm now runs the gate, under the scope's
//     supersedes lock held exclusively (the gate reads the pins of other
//     sources, which no row lock of the confirm covers), and is refused with
//     the head of the chain, as a declaration is.
//
// Blocks, declarations and guesses go through the store's write functions and
// dream.WriteLinks; the interleavings are held at a statement of the
// production path (a query tracer on a pool).
//
//	go test -tags=integration -p 1 ./internal/dream/ -run 'TestGuardResolveBatchAndADreamBatch|TestSupersedesConfirm' -count=20 -v
package dream_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// raceBefore is a pgx tracer that calls before on the caller's goroutine right
// before each statement of its pool is sent, with the connection's backend
// pid; the caller's transaction stays open, with its locks, while it runs.
type raceBefore struct {
	before func(pid uint32, sql string)
}

func (b *raceBefore) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	b.before(conn.PgConn().PID(), d.SQL)
	return ctx
}

func (b *raceBefore) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func raceHookedPool(t *testing.T, dsn string, before func(pid uint32, sql string)) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = &raceBefore{before: before}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// raceWaitedOn polls until a backend waits on a lock pid holds (true) or done
// is closed (false).
func raceWaitedOn(t *testing.T, pool *pgxpool.Pool, pid uint32, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(testdb.CommitHoldBound)
	for {
		select {
		case <-done:
			return false
		default:
		}
		var waiting bool
		if err := pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`, int64(pid)).Scan(&waiting); err != nil {
			t.Fatalf("poll lock wait: %v", err)
		}
		if waiting {
			return true
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing waited on pid %d and nothing finished within %s", pid, testdb.CommitHoldBound)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func raceSetupDSN(t *testing.T) (*pgxpool.Pool, string, *blocktype.Set) {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	return pool, dsn, reg.Snapshot()
}

func raceDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// TestGuardResolveBatchAndADreamBatchOfOneScope: a dream batch of X links
// [hi, lo] (topical) and stands before its second insert, holding the
// key-share lock on hi; a guard resolve batch over [lo, hi] starts. Neither
// may end with 40P01.
func TestGuardResolveBatchAndADreamBatchOfOneScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, resolution := range []string{"keep", "archive"} {
		t.Run(resolution, func(t *testing.T) {
			pool, dsn, set := raceSetupDSN(t)
			ctx := context.Background()
			lo, hi := raceBlock(t, pool, "gr-dream a"), raceBlock(t, pool, "gr-dream b")
			if lo > hi {
				lo, hi = hi, lo
			}
			x := raceBlock(t, pool, "gr-dream source")

			var mu sync.Mutex
			inserts := 0
			started := false
			resolveDone := make(chan struct{})
			var resolveErr error
			dreamPool := raceHookedPool(t, dsn, func(pid uint32, sql string) {
				if !strings.Contains(sql, "INSERT INTO context_dream_links") {
					return
				}
				mu.Lock()
				inserts++
				second := inserts == 2
				mu.Unlock()
				if !second {
					return
				}
				started = true
				go func() {
					defer close(resolveDone)
					_, _, resolveErr = store.GuardResolveBatch(ctx, pool, []string{lo, hi}, resolution, []string{"private"})
				}()
				raceWaitedOn(t, pool, pid, resolveDone) // either: the resolve waits, or it is through
			})
			n, dreamErr := dream.WriteLinks(ctx, dreamPool, set, x, "private", 1.0, []dream.Link{
				{TargetID: hi, Relationship: "topical", Confidence: 0.9},
				{TargetID: lo, Relationship: "topical", Confidence: 0.9},
			})
			if !started {
				t.Fatalf("the dream batch never reached its second insert")
			}
			<-resolveDone
			if dreamErr != nil || resolveErr != nil {
				t.Errorf("dream batch = (%d, %v) (deadlock %v), resolve batch = %v (deadlock %v), want both through",
					n, dreamErr, raceDeadlock(dreamErr), resolveErr, raceDeadlock(resolveErr))
			}
			if n != 2 {
				t.Errorf("dream batch wrote %d links, want 2", n)
			}
		})
	}
}

// raceGuess writes the dream guess "source supersedes target" at confidence c.
func raceGuess(t *testing.T, pool *pgxpool.Pool, set *blocktype.Set, source, target string, c float64) {
	t.Helper()
	if _, err := dream.WriteLinks(context.Background(), pool, set, source, "private", 1.0,
		[]dream.Link{{TargetID: target, Relationship: "supersedes", Confidence: c}}); err != nil {
		t.Fatalf("guess %s supersedes %s: %v", source, target, err)
	}
	if !raceSupersedesRow(t, pool, source, target) {
		t.Fatalf("the guess %s supersedes %s was not written", source, target)
	}
}

func raceDeclare(t *testing.T, pool *pgxpool.Pool, set *blocktype.Set, title, target string) string {
	t.Helper()
	b, err := store.UpsertBlockSupersedes(context.Background(), pool, "race", title, title+" content", nil, nil, "private", true,
		store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: target, APIKeyID: "race", Types: set})
	if err != nil {
		t.Fatalf("declare %s: %v", title, err)
	}
	return b.ID
}

// raceSettlers lists the sources whose pinned supersedes link at the
// snapshot confidence settles target.
func raceSettlers(t *testing.T, pool *pgxpool.Pool, target string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT dl.source_block_id::text FROM context_dream_links dl
		  WHERE dl.target_block_id = $1::uuid AND dl.relationship = 'supersedes' AND `+store.SettledSupersedesSQL+`
		  ORDER BY 1`, target)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func racePointer(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var by *string
	if err := pool.QueryRow(context.Background(), `SELECT superseded_by::text FROM context_blocks WHERE id = $1::uuid`, id).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if by == nil {
		return ""
	}
	return *by
}

// TestSupersedesConfirmAfterADeclarationOfTheTarget: S1 declares that it
// supersedes T; dream guesses that S2 supersedes T. Confirming the guess must
// be refused like a second declaration, naming S1 as the head — also for a
// guess at exactly the snapshot confidence; a guess that would not settle the
// target (below it), and the confirm of the declared pair itself, stay
// confirmable.
func TestSupersedesConfirmAfterADeclarationOfTheTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, set := raceSetup(t)
	ctx := context.Background()
	tgt := raceBlock(t, pool, "p4 note")
	s1 := raceDeclare(t, pool, set, "p4 note v2", tgt)
	s2 := raceBlock(t, pool, "p4 note v3")
	s3 := raceBlock(t, pool, "p4 note v4")
	s4 := raceBlock(t, pool, "p4 note v5")
	raceGuess(t, pool, set, s2, tgt, 0.9)
	raceGuess(t, pool, set, s3, tgt, 0.5)
	raceGuess(t, pool, set, s4, tgt, 0.7) // exactly the snapshot confidence: it would settle

	res, err := store.DreamLinkResolve(ctx, pool, s2, tgt, "supersedes", "confirm", "", []string{"private"})
	var conflict *store.SupersedesConflictError
	if !errors.As(err, &conflict) || conflict.Kind != store.SupersedesSuperseded || conflict.Head != s1 {
		t.Errorf("confirm of a second superseder = (%+v, %v), want the already-superseded refusal with head %s", res, err, s1)
	}
	if _, err := store.DreamLinkResolve(ctx, pool, s4, tgt, "supersedes", "confirm", "", []string{"private"}); !errors.As(err, &conflict) {
		t.Errorf("confirm of a guess at exactly the snapshot confidence = %v, want the already-superseded refusal", err)
	}
	if got := raceSettlers(t, pool, tgt); len(got) != 1 || got[0] != s1 {
		t.Errorf("settling superseders of the target = %v, want only the declaration %s", got, s1)
	}
	if p := racePointer(t, pool, tgt); p != s1 {
		t.Errorf("target pointer = %s, want %s", p, s1)
	}

	if res, err := store.DreamLinkResolve(ctx, pool, s3, tgt, "supersedes", "confirm", "", []string{"private"}); err != nil || res == nil || !res.Pinned {
		t.Errorf("confirm of a guess below the snapshot confidence = (%+v, %v), want pinned", res, err)
	}
	if res, err := store.DreamLinkResolve(ctx, pool, s1, tgt, "supersedes", "confirm", "kept", []string{"private"}); err != nil || res == nil || !res.Pinned {
		t.Errorf("confirm of the declared pair = (%+v, %v), want pinned", res, err)
	}
	if got := raceSettlers(t, pool, tgt); len(got) != 1 || got[0] != s1 {
		t.Errorf("settling superseders after the other confirms = %v, want only %s", got, s1)
	}
}

// TestSupersedesConfirmsOfTwoSourcesOnOneTarget: dream guesses that S1 and S2
// both supersede T; two operators confirm them at the same time. The first
// confirm stands before its pin when the second starts. Exactly one may
// settle T, the other is refused.
func TestSupersedesConfirmsOfTwoSourcesOnOneTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn, set := raceSetupDSN(t)
	ctx := context.Background()
	tgt := raceBlock(t, pool, "p4c note")
	s1 := raceBlock(t, pool, "p4c note v2")
	s2 := raceBlock(t, pool, "p4c note v3")
	raceGuess(t, pool, set, s1, tgt, 0.9)
	raceGuess(t, pool, set, s2, tgt, 0.9)

	secondDone := make(chan struct{})
	var secondErr error
	started := false
	var once sync.Once
	firstPool := raceHookedPool(t, dsn, func(pid uint32, sql string) {
		if !strings.Contains(sql, "SET pinned = true") {
			return
		}
		once.Do(func() {
			started = true
			go func() {
				defer close(secondDone)
				_, secondErr = store.DreamLinkResolve(ctx, pool, s2, tgt, "supersedes", "confirm", "", []string{"private"})
			}()
			raceWaitedOn(t, pool, pid, secondDone)
		})
	})
	_, firstErr := store.DreamLinkResolve(ctx, firstPool, s1, tgt, "supersedes", "confirm", "", []string{"private"})
	if !started {
		t.Fatalf("the first confirm never reached its pin")
	}
	<-secondDone
	if firstErr != nil {
		t.Errorf("first confirm: %v", firstErr)
	}
	var conflict *store.SupersedesConflictError
	if !errors.As(secondErr, &conflict) || conflict.Head != s1 {
		t.Errorf("second confirm = %v, want the already-superseded refusal with head %s", secondErr, s1)
	}
	if got := raceSettlers(t, pool, tgt); len(got) != 1 || got[0] != s1 {
		t.Errorf("settling superseders = %v, want only %s", got, s1)
	}
}

// TestGuardResolveBatchArchiveBesideADreamBatchOfItsBlock: a dream batch of S
// has written the guess "S supersedes X" (X its snapshot) and stands before
// its COMMIT when the operator archives S through a guard resolve batch. The
// archive takes the scope's supersedes lock exclusively, so it waits for the
// batch, then sees the guess and reconciles X; beside it (a shared lock) it
// would archive S without the guess, and X would stay a snapshot of an
// archived block.
func TestGuardResolveBatchArchiveBesideADreamBatchOfItsBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn, set := raceSetupDSN(t)
	ctx := context.Background()
	vec := func(i int, a float32, j int, b float32) []float32 {
		v := make([]float32, 1024)
		v[i], v[j] = a, b
		return v
	}
	m := raceBlock(t, pool, "gr-dream hub")
	x := raceBlock(t, pool, "gr-dream note")
	s := raceBlock(t, pool, "gr-dream note v2") // newer than x: dream's direction rule
	for id, v := range map[string][]float32{m: vec(10, 1, 0, 0), s: vec(10, 0.96, 11, 0.28)} {
		if err := store.StoreEmbedding(ctx, pool, id, "gr", v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := guard.RunGuardBatch(ctx, pool, set, 50, guard.Search{EfSearch: 800, ExactMaxCandidates: 400}); err != nil {
		t.Fatalf("guard run: %v", err)
	}

	batchPool, hold := testdb.HoldCommitPool(t, dsn, func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_dream_links") })
	batchDone := make(chan error, 1)
	go func() {
		_, err := dream.WriteLinks(ctx, batchPool, set, s, "private", 1.0,
			[]dream.Link{{TargetID: x, Relationship: "supersedes", Confidence: 0.9}})
		batchDone <- err
	}()
	batchPid := hold.Wait(t)

	resolveDone := make(chan struct{})
	var resolved []store.Block
	var resolveErr error
	go func() {
		defer close(resolveDone)
		resolved, _, resolveErr = store.GuardResolveBatch(ctx, pool, []string{s}, "archive", []string{"private"})
	}()
	waited := raceWaitedOn(t, pool, batchPid, resolveDone)
	hold.Release()
	<-resolveDone
	if err := <-batchDone; err != nil {
		t.Fatalf("dream batch: %v", err)
	}
	if !waited {
		t.Errorf("the archive did not wait for the dream batch of its block")
	}
	if resolveErr != nil || len(resolved) != 1 {
		t.Fatalf("guard resolve batch = (%v, %v), want S archived", resolved, resolveErr)
	}
	if l, p := raceLifecycle(t, pool, x), racePointer(t, pool, x); l != "knowledge" || p != "" {
		t.Errorf("X after the archive of its guessed superseder = %s superseded_by %q, want knowledge", l, p)
	}
}
