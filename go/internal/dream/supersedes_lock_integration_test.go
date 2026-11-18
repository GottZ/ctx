//go:build integration

// Lock discipline of the supersedes reconcile (PR #44 hardening, F1 + F6).
//
// Both gates drive the PRODUCTION entry point dream.WriteLinks over pools of
// their own (one backend per actor, tagged via application_name) and use a
// third connection as a gate that stalls one actor at a chosen statement. The
// stall is observed in pg_stat_activity (wait_event_type = 'Lock'), never by
// sleeping, so both gates are deterministic.
//
//	go test -tags=integration -p 1 ./internal/dream/ -run 'TestWriteLinks_Reconcile' -count=1 -v
package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

// Lock fixture: two older targets (T < U in uuid order) and two newer
// sources, all in one category with near-identical titles so the V8
// supersedes pre-filter (same category, source newer, title similarity)
// accepts every pair.
const (
	lkSourceA = "019d0000-0000-7000-9000-0000000000a1"
	lkSourceB = "019d0000-0000-7000-9000-0000000000a2"
	lkTargetT = "019d0000-0000-7000-9000-0000000000b1"
	lkTargetU = "019d0000-0000-7000-9000-0000000000b2"
)

func seedLockFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	insertBlock(t, pool, lkTargetT, "private", "decisions", "state v1", tEarly, tEarly)
	insertBlock(t, pool, lkTargetU, "private", "decisions", "state v0", tEarly, tEarly)
	insertBlock(t, pool, lkSourceA, "private", "decisions", "state v2", tLate, tLate)
	insertBlock(t, pool, lkSourceB, "private", "decisions", "state v3", tLate, tLate)
}

// actorPool opens a single-connection pool whose backend is identifiable in
// pg_stat_activity by application_name. extra carries further startup GUCs.
func actorPool(t *testing.T, dsn, name string, extra map[string]string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	for k, v := range extra {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("actor pool %s: %v", name, err)
	}
	t.Cleanup(p.Close)
	return p
}

// lockWaiting reports whether the backend of actor name currently waits on a
// heavyweight lock (row locks surface as tuple/transactionid waits).
func lockWaiting(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity
		  WHERE application_name = $1 AND datname = current_database()
		    AND wait_event_type = 'Lock'`, name).Scan(&n); err != nil {
		t.Fatalf("pg_stat_activity poll: %v", err)
	}
	return n > 0
}

func waitForLockWait(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !lockWaiting(t, pool, name); {
		if time.Now().After(deadline) {
			t.Fatalf("actor %s never reached its lock wait", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type writeResult struct {
	written int
	err     error
}

func writeAsync(ctx context.Context, pool *pgxpool.Pool, sourceID string, links []dream.Link) <-chan writeResult {
	ch := make(chan writeResult, 1)
	go func() {
		n, err := dream.WriteLinks(ctx, pool, icBuiltinSet, sourceID, "private", 1.0, links)
		ch <- writeResult{n, err}
	}()
	return ch
}

func awaitWrite(t *testing.T, label string, ch <-chan writeResult) writeResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(60 * time.Second):
		t.Fatalf("%s: WriteLinks did not return within 60s", label)
		return writeResult{}
	}
}

// gateTx opens the third connection that stalls an actor: it takes the given
// lock and holds it until release() is called (rollback).
func gateTx(t *testing.T, pool *pgxpool.Pool, lockSQL string, args ...any) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("gate begin: %v", err)
	}
	var one int
	if err := tx.QueryRow(ctx, lockSQL, args...).Scan(&one); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("gate lock: %v", err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	return release
}

// writeGateDDL installs, in the test's own database, the shared row that every
// block write used to take: until migration 181, mark_guard_dirty (per
// statement since 177) updated the one context_guard_state row at the end of
// each INSERT or UPDATE on context_blocks and held it until commit. The gates
// that park a writer at its first block write need such a row; 181 dropped
// the trigger, so they install this one. test_take_write_gate updates the one
// test_write_gate row at the end of every statement that inserted or updated
// at least one block row, from statement-level AFTER triggers with transition
// tables like those of 177, named to fire after trg_block_counts_* as
// trg_guard_dirty did. mark_guard_dirty's skip rules (category index, a
// guard verdict write with unchanged content) are left out: no fixture here
// writes such a row.
const writeGateDDL = `
CREATE TABLE test_write_gate (
    id     boolean PRIMARY KEY DEFAULT true CHECK (id),
    writes bigint  NOT NULL DEFAULT 0
);
INSERT INTO test_write_gate DEFAULT VALUES;
CREATE FUNCTION test_take_write_gate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM new_rows) THEN
        UPDATE test_write_gate SET writes = writes + 1;
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER trg_test_write_gate
    AFTER INSERT ON context_blocks
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION test_take_write_gate();
CREATE TRIGGER trg_test_write_gate_upd
    AFTER UPDATE ON context_blocks
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION test_take_write_gate();`

// holdWriteGate installs writeGateDDL and holds the gate row on a third
// connection until release: every transaction that writes a block row in
// between parks at the end of that statement, keeping every lock it took
// before. After release the first one queued takes the row and holds it until
// its commit, and the others wait for it in turn — the shared lock, taken at
// the end of each block write and held until commit, that makes "row locks
// first, block writes after" a rule for every write path.
func holdWriteGate(t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), writeGateDDL); err != nil {
		t.Fatalf("install write gate: %v", err)
	}
	return gateTx(t, pool, `SELECT 1 FROM test_write_gate FOR NO KEY UPDATE`)
}

// TestWriteLinks_ReconcileLock_DoesNotBlockConcurrentLinkInsert is the F1
// gate. Writer A supersedes T and U; a gate pins U, so A stalls in its
// reconcile phase while it already holds its lock on T. Writer B then writes
// an unrelated topical link onto T. The INSERT's foreign-key check takes FOR
// KEY SHARE on T: a FOR UPDATE reconcile lock conflicts with it (B waits until
// A commits — here: until B's lock_timeout fires, 55P03), FOR NO KEY UPDATE
// does not (the reconcile changes no key column, so it needs no stronger lock).
func TestWriteLinks_ReconcileLock_DoesNotBlockConcurrentLinkInsert(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	seedLockFixture(t, pool)

	release := gateTx(t, pool,
		`SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR NO KEY UPDATE`, lkTargetU)

	poolA := actorPool(t, dsn, "f1-writer-a", nil)
	doneA := writeAsync(ctx, poolA, lkSourceA, []dream.Link{
		{TargetID: lkTargetT, Relationship: "supersedes", Confidence: 0.95},
		{TargetID: lkTargetU, Relationship: "supersedes", Confidence: 0.95},
	})
	waitForLockWait(t, pool, "f1-writer-a")

	poolB := actorPool(t, dsn, "f1-writer-b", map[string]string{"lock_timeout": "3000"})
	bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	writtenB, errB := dream.WriteLinks(bctx, poolB, icBuiltinSet, lkSourceB, "private", 1.0,
		[]dream.Link{{TargetID: lkTargetT, Relationship: "topical", Confidence: 0.9}})

	release()
	resA := awaitWrite(t, "writer A", doneA)

	if errB != nil || writtenB != 1 {
		t.Errorf("writer B (topical link onto T) blocked behind A's reconcile lock: written=%d err=%v", writtenB, errB)
	}
	if resA.err != nil || resA.written != 2 {
		t.Fatalf("writer A: written=%d err=%v", resA.written, resA.err)
	}
	assertSnapshotState(t, pool, lkTargetT, lkSourceA)
	assertSnapshotState(t, pool, lkTargetU, lkSourceA)
}

// TestWriteLinks_ReconcileLock_CrossOrderNoDeadlock is the F6 gate: two
// writers change the supersedes state of the same two targets, the model emits
// them in opposite order (A: T,U — B: U,T). B re-classifies its own supersedes
// links onto both targets as topical (a batch without a supersedes guess, so
// it does not take the scope's guess lock, store.LockSupersedesGuesses, and
// runs beside A); A supersedes both. A gate pins B's link row B→T, so B stalls
// in its link loop right after it has handled U. If the snapshot side effect
// locked targets per link in model order, B would now hold U, A would take T
// and wait for U, and B — once the gate opens — would wait for T: 40P01, one
// batch lost. Collecting the targets and locking them at the end of the
// transaction in ascending id order leaves B holding no target lock while it
// waits, so A runs through and B follows. (Two batches that both carry a
// supersedes guess no longer run side by side in one scope: the guess lock
// orders them, issue #197.)
func TestWriteLinks_ReconcileLock_CrossOrderNoDeadlock(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	seedLockFixture(t, pool)

	// B's existing supersedes links onto U and T — B re-classifies them below;
	// B→T is the row the gate pins.
	if n, err := dream.WriteLinks(ctx, pool, icBuiltinSet, lkSourceB, "private", 1.0, []dream.Link{
		{TargetID: lkTargetU, Relationship: "supersedes", Confidence: 0.95},
		{TargetID: lkTargetT, Relationship: "supersedes", Confidence: 0.95},
	}); err != nil || n != 2 {
		t.Fatalf("seed B's supersedes links: n=%d err=%v", n, err)
	}
	release := gateTx(t, pool,
		`SELECT 1 FROM context_dream_links
		  WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid
		  FOR UPDATE`, lkSourceB, lkTargetT)

	poolB := actorPool(t, dsn, "f6-writer-b", nil)
	doneB := writeAsync(ctx, poolB, lkSourceB, []dream.Link{
		{TargetID: lkTargetU, Relationship: "topical", Confidence: 0.95},
		{TargetID: lkTargetT, Relationship: "topical", Confidence: 0.95},
	})
	waitForLockWait(t, pool, "f6-writer-b")

	poolA := actorPool(t, dsn, "f6-writer-a", nil)
	doneA := writeAsync(ctx, poolA, lkSourceA, []dream.Link{
		{TargetID: lkTargetT, Relationship: "supersedes", Confidence: 0.95},
		{TargetID: lkTargetU, Relationship: "supersedes", Confidence: 0.95},
	})

	// A either finishes on its own (ordered locking) or parks behind B's
	// target lock (model-order locking) — in both cases the gate may open.
	var resA writeResult
	finishedA := false
	for deadline := time.Now().Add(30 * time.Second); !finishedA; {
		select {
		case resA = <-doneA:
			finishedA = true
		default:
		}
		if finishedA || lockWaiting(t, pool, "f6-writer-a") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer A neither finished nor waited on a lock within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !finishedA {
		t.Log("writer A waits on a target lock held by the stalled writer B")
	}

	release()
	if !finishedA {
		resA = awaitWrite(t, "writer A", doneA)
	}
	resB := awaitWrite(t, "writer B", doneB)

	if resA.err != nil || resA.written != 2 {
		t.Errorf("writer A: written=%d err=%v", resA.written, resA.err)
	}
	if resB.err != nil || resB.written != 2 {
		t.Errorf("writer B: written=%d err=%v", resB.written, resB.err)
	}
	if t.Failed() {
		return
	}
	// A committed first (the pointers stayed on B, whose supersedes links were
	// still there); B's re-classification then moved both to A, the superseder
	// left.
	assertSnapshotState(t, pool, lkTargetT, lkSourceA)
	assertSnapshotState(t, pool, lkTargetU, lkSourceA)
}

// TestWriteLinks_ReconcileLock_LocksAllTargetsBeforeFirstUpdate pins the
// end-of-transaction half of F6: the reconcile takes every target lock before
// its first block write. A row that each block write takes and holds until
// commit — the context_guard_state row of mark_guard_dirty until migration
// 181, the test's write gate (holdWriteGate) here — turns a target lock taken
// after a write into a lock-order cycle. If the reconcile locked and updated
// targets one by one (in model order, or even in id order), A could hold T
// plus the shared row and wait for U while B holds U and waits for the shared
// row: 40P01. The gate holds that row so both writers park at their first
// UPDATE; A queues first. Locking ALL targets in one ordered statement before
// the first UPDATE makes B wait on A's lock on U instead of holding it. Since
// 181 no schema trigger takes such a row (the block counts of 170 insert delta
// rows); the gate keeps the rule pinned for any shared lock a write path or
// trigger takes at a block write: row locks first, block writes after. B
// re-classifies its own supersedes link onto U as topical: a batch without a
// supersedes guess, so it does not wait for A's guess lock
// (store.LockSupersedesGuesses, issue #197) and reaches its reconcile beside
// A.
func TestWriteLinks_ReconcileLock_LocksAllTargetsBeforeFirstUpdate(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	seedLockFixture(t, pool)
	if n, err := dream.WriteLinks(ctx, pool, icBuiltinSet, lkSourceB, "private", 1.0, []dream.Link{
		{TargetID: lkTargetU, Relationship: "supersedes", Confidence: 0.95},
	}); err != nil || n != 1 {
		t.Fatalf("seed B's supersedes link: n=%d err=%v", n, err)
	}

	release := holdWriteGate(t, pool)

	poolA := actorPool(t, dsn, "f6b-writer-a", nil)
	doneA := writeAsync(ctx, poolA, lkSourceA, []dream.Link{
		{TargetID: lkTargetT, Relationship: "supersedes", Confidence: 0.95},
		{TargetID: lkTargetU, Relationship: "supersedes", Confidence: 0.95},
	})
	waitForLockWait(t, pool, "f6b-writer-a")

	poolB := actorPool(t, dsn, "f6b-writer-b", nil)
	doneB := writeAsync(ctx, poolB, lkSourceB, []dream.Link{
		{TargetID: lkTargetU, Relationship: "topical", Confidence: 0.95},
	})
	waitForLockWait(t, pool, "f6b-writer-b")

	release()
	resA := awaitWrite(t, "writer A", doneA)
	resB := awaitWrite(t, "writer B", doneB)

	if resA.err != nil || resA.written != 2 {
		t.Errorf("writer A: written=%d err=%v", resA.written, resA.err)
	}
	if resB.err != nil || resB.written != 1 {
		t.Errorf("writer B: written=%d err=%v", resB.written, resB.err)
	}
	if t.Failed() {
		return
	}
	assertSnapshotState(t, pool, lkTargetT, lkSourceA)
	assertSnapshotState(t, pool, lkTargetU, lkSourceA)
}
