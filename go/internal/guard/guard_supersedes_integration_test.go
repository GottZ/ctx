//go:build integration

// Issue #284 for the guard run:
//
//   - point 3: the archive verdict ends the supersedes of the archived block,
//     so the targets of its supersedes links are reconciled in the verdict's
//     savepoint (a target no other live block supersedes is knowledge again,
//     not a snapshot of an archived block until the daily link cleanup);
//   - the verdict keeps out what could add a supersedes link from the block
//     while it reads them — a dream batch with a supersedes guess, through
//     the scope's guess lock taken shared — and locks the targets' rows, but
//     never waits for them: the run holds its picked blocks for its whole
//     batch, so a wait would add an edge a lock cycle could run through. A
//     dream batch with a guess in flight in the scope, or a target another
//     transaction holds, leaves the block pending for the next run, with no
//     half verdict;
//   - a declaration whose source is the matched neighbour of a block the run
//     holds, and a batch upsert over blocks the run holds, held their identity
//     rows FOR UPDATE: the run's audit insert takes the foreign key's
//     key-share lock on the matched neighbour and waited for them while they
//     waited for the run's block (40P01).
//
// Every block goes through store.UpsertBlock / UpsertBlockSupersedes /
// UpsertBlocks and store.StoreEmbedding, every verdict through
// guard.RunGuardBatch, the dream batch through dream.WriteLinks. The
// interleavings are held at statements of the production path (a query tracer
// on a pool, or testdb.HoldCommitPool at a COMMIT).
//
//	go test -tags=integration -p 1 ./internal/guard/ -run 'TestGuardSupersedes' -count=20 -v
package guard_test

import (
	"context"
	"errors"
	"fmt"
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

// g284Vec is a unit vector with weight a on axis i and b on axis j.
func g284Vec(i int, a float32, j int, b float32) []float32 {
	v := make([]float32, 1024)
	v[i] = a
	if b != 0 {
		v[j] = b
	}
	return v
}

var (
	g284Neighbour = g284Vec(10, 1, 0, 0)
	g284Review    = g284Vec(10, 0.96, 11, 0.28)    // cosine 0.96: needs_review
	g284Duplicate = g284Vec(10, 0.995, 11, 0.0999) // cosine 0.995: near_duplicate, archived
)

type g284Fixture struct {
	pool *pgxpool.Pool
	dsn  string
	set  *blocktype.Set
}

func newG284Fixture(t *testing.T) *g284Fixture {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	return &g284Fixture{pool: pool, dsn: dsn, set: stateLockSet(t, pool)}
}

func (f *g284Fixture) block(t *testing.T, title string, vec []float32) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, "g284", title, title+" content", nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	if vec != nil {
		f.embed(t, b.ID, vec)
	}
	return b.ID
}

func (f *g284Fixture) embed(t *testing.T, id string, vec []float32) {
	t.Helper()
	if err := store.StoreEmbedding(context.Background(), f.pool, id, "g284", vec); err != nil {
		t.Fatalf("embed %s: %v", id, err)
	}
}

func (f *g284Fixture) declare(pool *pgxpool.Pool, title, target string) (*store.Block, error) {
	return store.UpsertBlockSupersedes(context.Background(), pool, "g284", title, title+" content", nil, nil, "private", true,
		store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: target, APIKeyID: "g284", Types: f.set})
}

func (f *g284Fixture) run(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := guard.RunGuardBatch(context.Background(), pool, f.set, 50, guard.SearchIndexForTest); err != nil {
		t.Fatalf("guard run: %v", err)
	}
}

// g284State is what one block carries after the guard (auditRows: the
// guard's own audit rows, not the dream batch's).
type g284State struct {
	archived     bool
	checked      bool // metadata.guard_checked_at: a verdict was written
	guardStatus  string
	lifecycle    string
	supersededBy string
	auditRows    int
}

func (f *g284Fixture) state(t *testing.T, id string) g284State {
	t.Helper()
	var s g284State
	if err := f.pool.QueryRow(context.Background(),
		`SELECT is_archived, metadata ? 'guard_checked_at', COALESCE(guard_status, ''), lifecycle_state, COALESCE(superseded_by::text, ''),
		        (SELECT count(*)::int FROM context_write_log w WHERE w.block_id = cb.id AND w.decision NOT LIKE 'dream%')
		   FROM context_blocks cb WHERE id = $1::uuid`, id).
		Scan(&s.archived, &s.checked, &s.guardStatus, &s.lifecycle, &s.supersededBy, &s.auditRows); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return s
}

// g284Before is a pgx tracer that calls before on the caller's goroutine right
// before each statement of its pool is sent, with the connection's backend pid
// and the statement's arguments; the caller's transaction stays open, with
// every lock it took, while before runs.
type g284Before struct {
	before func(pid uint32, sql string, args []any)
}

func (b *g284Before) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	b.before(conn.PgConn().PID(), d.SQL, d.Args)
	return ctx
}

func (b *g284Before) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (f *g284Fixture) hooked(t *testing.T, before func(pid uint32, sql string, args []any)) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = &g284Before{before: before}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// atAuditInsertOf runs fn once, before the run's audit-log insert of block id.
func atAuditInsertOf(id string, fn func(pid uint32)) func(uint32, string, []any) {
	var once sync.Once
	return func(pid uint32, sql string, args []any) {
		if strings.Contains(sql, "INSERT INTO context_write_log") && len(args) > 0 && fmt.Sprint(args[0]) == id {
			once.Do(func() { fn(pid) })
		}
	}
}

// waitedOn polls until a backend waits on a lock pid holds (true) or done is
// closed (false), whichever comes first.
func waitedOn(t *testing.T, pool *pgxpool.Pool, pid uint32, done <-chan struct{}) bool {
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

func deadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// TestGuardSupersedesArchiveVerdictReconcilesTargets: S declares that it
// supersedes T; the guard archives S as a near duplicate of a third block.
func TestGuardSupersedesArchiveVerdictReconcilesTargets(t *testing.T) {
	f := newG284Fixture(t)
	f.block(t, "g284 neighbour", g284Neighbour)
	f.run(t, f.pool)
	tgt := f.block(t, "g284 target", nil)
	src, err := f.declare(f.pool, "g284 source", tgt)
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	f.embed(t, src.ID, g284Duplicate)
	if s := f.state(t, tgt); s.lifecycle != "snapshot" {
		t.Fatalf("target before the guard = %s, want snapshot", s.lifecycle)
	}
	f.run(t, f.pool)
	if s := f.state(t, src.ID); !s.archived || s.auditRows != 1 {
		t.Fatalf("source after the guard: %+v, want archived with one audit row", s)
	}
	if s := f.state(t, tgt); s.lifecycle != "knowledge" || s.supersededBy != "" {
		t.Errorf("target after the guard archived its superseder = %s superseded_by %q, want knowledge without pointer", s.lifecycle, s.supersededBy)
	}
}

// TestGuardSupersedesArchiveVerdictBesideADreamBatchOfTheScope: a dream batch
// of S has written "S supersedes X" and made X its snapshot and stands before
// its COMMIT, holding the scope's supersedes lock (shared). The guard's
// archive verdict on S must neither wait for the batch nor archive S beside
// it: the batch commits a supersedes from S the verdict could not see, and X
// would stay a snapshot of an archived block. S stays pending; the next run,
// after the batch, archives it and reconciles X.
func TestGuardSupersedesArchiveVerdictBesideADreamBatchOfTheScope(t *testing.T) {
	f := newG284Fixture(t)
	ctx := context.Background()
	f.block(t, "g284 neighbour", g284Neighbour)
	f.run(t, f.pool)
	x := f.block(t, "g284 note", nil)
	src := f.block(t, "g284 note v2", nil) // newer than X: dream's direction rule

	batchPool, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_dream_links") })
	batchDone := make(chan error, 1)
	go func() {
		_, err := dream.WriteLinks(ctx, batchPool, f.set, src, "private", 1.0,
			[]dream.Link{{TargetID: x, Relationship: "supersedes", Confidence: 0.9}})
		batchDone <- err
	}()
	batchPid := hold.Wait(t)

	f.embed(t, src, g284Duplicate)
	runDone := make(chan struct{})
	var runErr error
	go func() {
		defer close(runDone)
		_, runErr = guard.RunGuardBatch(ctx, f.pool, f.set, 50, guard.SearchIndexForTest)
	}()
	if waitedOn(t, f.pool, batchPid, runDone) {
		t.Errorf("the guard run waits for the dream batch of the scope")
	}
	hold.Release()
	<-runDone
	if err := <-batchDone; err != nil {
		t.Fatalf("dream batch: %v", err)
	}
	if runErr != nil {
		t.Fatalf("guard run: %v", runErr)
	}
	if s := f.state(t, src); s.archived || s.auditRows != 0 || s.checked {
		t.Errorf("source after the run beside the batch: %+v, want pending (not archived, no verdict, no audit row)", s)
	}
	f.run(t, f.pool)
	if s := f.state(t, src); !s.archived {
		t.Fatalf("source after the next run: %+v, want archived", s)
	}
	if s := f.state(t, x); s.lifecycle != "knowledge" || s.supersededBy != "" {
		t.Errorf("X after its superseder was archived = %s superseded_by %q, want knowledge", s.lifecycle, s.supersededBy)
	}
}

// TestGuardSupersedesArchiveVerdictDoesNotWaitForATarget: S supersedes T and
// is a near duplicate; an update of T stands before its COMMIT. The verdict
// on S must not wait for T (the run holds its picks): S stays pending with no
// half verdict, and the next run archives it and reconciles T.
func TestGuardSupersedesArchiveVerdictDoesNotWaitForATarget(t *testing.T) {
	f := newG284Fixture(t)
	ctx := context.Background()
	f.block(t, "g284 neighbour", g284Neighbour)
	f.run(t, f.pool)
	tgt := f.block(t, "g284 target", nil)
	src, err := f.declare(f.pool, "g284 source", tgt)
	if err != nil {
		t.Fatalf("declare: %v", err)
	}

	updPool, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.Contains(sql, "UPDATE context_blocks SET") })
	updDone := make(chan error, 1)
	go func() {
		_, _, err := store.UpdateBlock(ctx, updPool, tgt, store.UpdateBlockData{Metadata: map[string]any{"g284": "held"}}, []string{"private"})
		updDone <- err
	}()
	updPid := hold.Wait(t)

	f.embed(t, src.ID, g284Duplicate)
	runDone := make(chan struct{})
	var runErr error
	go func() {
		defer close(runDone)
		_, runErr = guard.RunGuardBatch(ctx, f.pool, f.set, 50, guard.SearchIndexForTest)
	}()
	if waitedOn(t, f.pool, updPid, runDone) {
		t.Errorf("the guard run waits for a supersedes target another transaction holds")
	}
	hold.Release()
	<-runDone
	if err := <-updDone; err != nil {
		t.Fatalf("update of the target: %v", err)
	}
	if runErr != nil {
		t.Fatalf("guard run: %v", runErr)
	}
	if s := f.state(t, src.ID); s.archived || s.auditRows != 0 || s.checked {
		t.Errorf("source after the run beside the held target: %+v, want pending with no verdict", s)
	}
	f.run(t, f.pool)
	if s := f.state(t, src.ID); !s.archived {
		t.Fatalf("source after the next run: %+v, want archived", s)
	}
	if s := f.state(t, tgt); s.lifecycle != "knowledge" || s.supersededBy != "" {
		t.Errorf("target after the archive of its superseder = %s superseded_by %q, want knowledge", s.lifecycle, s.supersededBy)
	}
}

// TestGuardSupersedesRunAndADeclarationOfItsMatch: the run holds T (picked)
// and stands before T's audit insert, whose foreign key takes the key-share
// lock on T's match S. A store of S declaring that it supersedes T holds S
// and waits for T. Neither may end with 40P01: the run goes through, then the
// declaration.
func TestGuardSupersedesRunAndADeclarationOfItsMatch(t *testing.T) {
	f := newG284Fixture(t)
	ctx := context.Background()
	f.block(t, "g284 decl source", g284Neighbour)
	f.run(t, f.pool)
	tgt := f.block(t, "g284 decl target", g284Review)

	declDone := make(chan struct{})
	var declErr error
	started := false
	runPool := f.hooked(t, atAuditInsertOf(tgt, func(pid uint32) {
		started = true
		go func() {
			defer close(declDone)
			_, declErr = f.declare(f.pool, "g284 decl source", tgt)
		}()
		if !waitedOn(t, f.pool, pid, declDone) {
			t.Errorf("the declaration did not wait for the run's block")
		}
	}))
	_, runErr := guard.RunGuardBatch(ctx, runPool, f.set, 50, guard.SearchIndexForTest)
	if !started {
		t.Fatalf("the run never reached the audit insert of the target")
	}
	<-declDone
	if runErr != nil || deadlock(declErr) || declErr != nil {
		t.Errorf("run = %v, declaration = %v, want both through", runErr, declErr)
	}
	if s := f.state(t, tgt); s.auditRows != 1 || s.guardStatus != "needs_review" {
		t.Errorf("target after the run: %+v, want needs_review with one audit row", s)
	}
}

// TestGuardSupersedesRunAndABatchUpsertOfItsBlocks: the run holds X (picked)
// and stands before X's audit insert, whose foreign key takes the key-share
// lock on X's match M. A batch upsert over [M, X] locks M and waits for X.
// Neither may end with 40P01 (the measurement of issue #284 found 97–100 of
// 200 rounds of a batch upsert beside a guard run ending so).
func TestGuardSupersedesRunAndABatchUpsertOfItsBlocks(t *testing.T) {
	f := newG284Fixture(t)
	ctx := context.Background()
	f.block(t, "g284 batch a", g284Neighbour)
	f.run(t, f.pool)
	x := f.block(t, "g284 batch b", g284Review)

	batchDone := make(chan struct{})
	var batchErr error
	started := false
	runPool := f.hooked(t, atAuditInsertOf(x, func(pid uint32) {
		started = true
		go func() {
			defer close(batchDone)
			_, batchErr = store.UpsertBlocks(ctx, f.pool, []store.BatchUpsert{
				{Category: "g284", Title: "g284 batch b", Content: "g284 batch b changed", Scope: "private"},
				{Category: "g284", Title: "g284 batch a", Content: "g284 batch a changed", Scope: "private"},
			}, "", f.set, nil)
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
	if s := f.state(t, x); s.auditRows != 1 {
		t.Errorf("X after the run: %+v, want one audit row", s)
	}
}

// TestGuardSupersedesDreamBatchWaitingForTheRunHoldsNoWriterOfTheScope: the
// run has archived A (its archive verdict holds the scope's guess lock shared
// until the batch commits) and stands before the audit insert of a later
// block B. A dream batch with a supersedes guess of the scope waits for the
// run; it must hold nothing while it waits, so a block batch of the scope
// (which takes the scope's supersedes lock exclusively) goes through at once.
// Waiting with the scope lock in hand, the dream batch would hold the block
// batch behind the rest of the guard batch.
func TestGuardSupersedesDreamBatchWaitingForTheRunHoldsNoWriterOfTheScope(t *testing.T) {
	f := newG284Fixture(t)
	ctx := context.Background()
	f.block(t, "g284 hub a", g284Neighbour)
	f.block(t, "g284 hub b", g284Vec(20, 1, 0, 0))
	x := f.block(t, "g284 guess", nil)
	y := f.block(t, "g284 guess v2", nil) // newer than x: dream's direction rule
	f.run(t, f.pool)
	a := f.block(t, "g284 dup a", g284Duplicate)
	b := f.block(t, "g284 review b", g284Vec(20, 0.96, 21, 0.28))

	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "g284-block-batch"
	batchPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(batchPool.Close)

	dreamDone, batchDone := make(chan struct{}), make(chan struct{})
	var dreamErr, batchErr error
	started := false
	runPool := f.hooked(t, atAuditInsertOf(b, func(pid uint32) {
		started = true
		go func() {
			defer close(dreamDone)
			_, dreamErr = dream.WriteLinks(ctx, f.pool, f.set, y, "private", 1.0,
				[]dream.Link{{TargetID: x, Relationship: "supersedes", Confidence: 0.9}})
		}()
		testdb.WaitBlockedBy(t, f.pool, pid, dreamDone, "the dream batch with a guess", "advisory")
		go func() {
			defer close(batchDone)
			_, batchErr = store.UpsertBlocks(ctx, batchPool, []store.BatchUpsert{
				{Category: "g284", Title: "g284 unrelated", Content: "g284 unrelated content", Scope: "private"},
			}, "", f.set, nil)
		}()
		for deadline := time.Now().Add(testdb.CommitHoldBound); ; {
			select {
			case <-batchDone:
				return
			default:
			}
			var waiting bool
			if err := f.pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name = 'g284-block-batch' AND wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
				t.Errorf("poll: %v", err)
				return
			}
			if waiting {
				t.Errorf("the block batch of the scope waits while a dream batch waits for the guard run")
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("the block batch neither finished nor waited within %s", testdb.CommitHoldBound)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}))
	_, runErr := guard.RunGuardBatch(ctx, runPool, f.set, 50, guard.SearchIndexForTest)
	if !started {
		t.Fatalf("the run never reached the audit insert of B")
	}
	<-dreamDone
	<-batchDone
	if runErr != nil || dreamErr != nil || batchErr != nil {
		t.Errorf("run = %v, dream batch = %v, block batch = %v, want all through", runErr, dreamErr, batchErr)
	}
	if s := f.state(t, a); !s.archived {
		t.Errorf("A after the run: %+v, want archived", s)
	}
	if s := f.state(t, x); s.lifecycle != "snapshot" || s.supersededBy != y {
		t.Errorf("X after the dream batch = %s superseded_by %q, want a snapshot of %s", s.lifecycle, s.supersededBy, y)
	}
}
