//go:build integration

// Issue #284: the lock order of the supersedes paths after #278, each
// interleaving driven on the production entry points over connections of
// their own (actorPool) and stalled at a chosen statement by a third
// connection (gateTx, holdWriteGate), never by sleeping.
//
//   - TestSupersedesScopeMoveAndReclassifyingBatch: a scope move of S and a
//     dream batch of S that re-classifies S→T from supersedes to topical, in
//     both orders. The batch holds the link row and then locks T in its
//     reconcile; the move holds S and T from its lock pass and then sweeps the
//     link rows of S. Unless one excludes the other before its row locks,
//     PostgreSQL ends one of them with 40P01.
//   - TestSupersedesBatchOfASourceThatLeftItsScope: a dream batch runs under
//     the scope read at pick time. A source moved to another scope (or
//     archived) since then must not get links written in the old scope, under
//     the old scope's locks. TestRunDreamCycle_SourceMovedDuringEval_BooksTransient_DB
//     pins what the cycle books for it.
//   - TestSupersedesConfirmAndDeclarationOfOneTarget: an operator confirms the
//     dream guess S1→T while a writer declares S2→T. Run one after the other,
//     the declaration sees the confirmed pin and is refused; side by side,
//     without a lock that orders them, both commit and T has two settling
//     superseders.
//   - TestSupersedesDanglingCleanupAndScopeMove: the daily dangling-link
//     cleanup against a scope move of a block an archived block supersedes.
//   - TestSupersedesResolveDeleteAndRedeclaration: a delete-resolve of S→T and
//     a repeat declaration S→T, in both orders. The resolve holds the link row
//     and then locks T in its reconcile; the declaration holds T and then
//     writes the link row.
//
// Run:
//
//	go test -tags=integration -p 1 ./internal/dream/ -run 'TestSupersedes(ScopeMove|BatchOfASource|ConfirmAndDeclaration|DanglingCleanup|ResolveDelete)|TestRunDreamCycle_SourceMoved' -count=1 -v
package dream_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// orderSetup is raceSetup with the DSN the actor pools need.
func orderSetup(t *testing.T) (*pgxpool.Pool, string, *blocktype.Set) {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	return pool, dsn, reg.Snapshot()
}

// orderScope reads a block's scope.
func orderScope(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT scope FROM context_blocks WHERE id = $1::uuid`, id).Scan(&s); err != nil {
		t.Fatalf("read scope: %v", err)
	}
	return s
}

// orderLinks counts the dream links of a source.
func orderLinks(t *testing.T, pool *pgxpool.Pool, source string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_dream_links WHERE source_block_id = $1::uuid`, source).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

type moveResult struct{ err error }

func moveAsync(ctx context.Context, pool *pgxpool.Pool, id, dest string) <-chan moveResult {
	ch := make(chan moveResult, 1)
	go func() {
		_, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Scope: &dest}, []string{"private", dest})
		ch <- moveResult{err}
	}()
	return ch
}

func awaitMove(t *testing.T, ch <-chan moveResult) error {
	t.Helper()
	select {
	case r := <-ch:
		return r.err
	case <-time.After(60 * time.Second):
		t.Fatalf("the scope move did not return within 60s")
		return nil
	}
}

func TestSupersedesScopeMoveAndReclassifyingBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, order := range []string{"move_first", "batch_first"} {
		t.Run(order, func(t *testing.T) {
			pool, dsn, set := orderSetup(t)
			ctx := context.Background()
			tgt := raceBlock(t, pool, "move-note")    // older
			u := raceBlock(t, pool, "move-side")      // a topical neighbour
			src := raceBlock(t, pool, "move-note v2") // newer: src supersedes tgt
			if n, err := dream.WriteLinks(ctx, pool, set, src, "private", 1.0, []dream.Link{
				{TargetID: tgt, Relationship: "supersedes", Confidence: 0.95},
				{TargetID: u, Relationship: "topical", Confidence: 0.9},
			}); err != nil || n != 2 {
				t.Fatalf("seed links: n=%d err=%v", n, err)
			}
			if lc := raceLifecycle(t, pool, tgt); lc != "snapshot" {
				t.Fatalf("seed: target lifecycle = %s, want snapshot", lc)
			}
			const dest = "move-dest"
			reclassify := []dream.Link{{TargetID: tgt, Relationship: "topical", Confidence: 0.9}}

			var moveDone <-chan moveResult
			var batchDone <-chan writeResult
			if order == "move_first" {
				// The move holds S and T from its lock pass and parks at the end
				// of its UPDATE of S (write gate). The batch then re-classifies
				// S→T: it holds the link row and locks T in its reconcile.
				release := holdWriteGate(t, pool)
				defer release()
				moveDone = moveAsync(ctx, actorPool(t, dsn, "p1-move", nil), src, dest)
				waitForLockWait(t, pool, "p1-move")
				batchDone = writeAsyncSet(ctx, actorPool(t, dsn, "p1-batch", nil), set, src, reclassify)
				waitForLockWait(t, pool, "p1-batch")
				release()
			} else {
				// The batch holds the re-classified link row S→T and parks in its
				// stale sweep on the link row S→U a gate holds. The move then
				// locks S and T and sweeps the link rows of S.
				release := gateTx(t, pool,
					`SELECT 1 FROM context_dream_links WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid FOR UPDATE`,
					src, u)
				defer release()
				batchDone = writeAsyncSet(ctx, actorPool(t, dsn, "p1-batch", nil), set, src, reclassify)
				waitForLockWait(t, pool, "p1-batch")
				moveDone = moveAsync(ctx, actorPool(t, dsn, "p1-move", nil), src, dest)
				waitForLockWait(t, pool, "p1-move")
				release()
			}

			moveErr := awaitMove(t, moveDone)
			batch := awaitWrite(t, "batch", batchDone)
			if moveErr != nil {
				t.Errorf("scope move: %v", moveErr)
			}
			// Move first: the batch runs after the move's commit, finds its
			// source in the new scope and writes nothing. Batch first: it
			// commits before the move.
			if order == "move_first" && !errors.Is(batch.err, dream.ErrSourceLeftScope) {
				t.Errorf("dream batch after the move: %v, want ErrSourceLeftScope", batch.err)
			}
			if order == "batch_first" && batch.err != nil {
				t.Errorf("dream batch: %v", batch.err)
			}
			if t.Failed() {
				return
			}
			if s := orderScope(t, pool, src); s != dest {
				t.Errorf("source scope after the move = %s, want %s", s, dest)
			}
			if lc := raceLifecycle(t, pool, tgt); lc != "knowledge" {
				t.Errorf("target lifecycle = %s, want knowledge (no superseder left in its scope)", lc)
			}
		})
	}
}

// writeAsyncSet is writeAsync with the cycle's block-type snapshot.
func writeAsyncSet(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, sourceID string, links []dream.Link) <-chan writeResult {
	ch := make(chan writeResult, 1)
	go func() {
		n, err := dream.WriteLinks(ctx, pool, set, sourceID, "private", 1.0, links)
		ch <- writeResult{n, err}
	}()
	return ch
}

func TestSupersedesBatchOfASourceThatLeftItsScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, how := range []string{"moved", "archived"} {
		t.Run(how, func(t *testing.T) {
			pool, set := raceSetup(t)
			ctx := context.Background()
			tgt := raceBlock(t, pool, "left-note")    // older
			src := raceBlock(t, pool, "left-note v2") // newer
			// The cycle picked src in "private"; before its link batch the
			// source leaves that scope.
			if how == "moved" {
				dest := "left-dest"
				if _, _, err := store.UpdateBlock(ctx, pool, src, store.UpdateBlockData{Scope: &dest}, []string{"private", dest}); err != nil {
					t.Fatalf("move: %v", err)
				}
			} else if _, err := store.DeleteBlock(ctx, pool, src, []string{"private"}); err != nil {
				t.Fatalf("archive: %v", err)
			}

			n, err := dream.WriteLinks(ctx, pool, set, src, "private", 1.0,
				[]dream.Link{{TargetID: tgt, Relationship: "supersedes", Confidence: 0.95}})
			if how == "moved" && !errors.Is(err, dream.ErrSourceLeftScope) {
				t.Errorf("write links from a source that left its scope: err = %v, want ErrSourceLeftScope", err)
			}
			if how == "archived" && err != nil && !errors.Is(err, dream.ErrSourceLeftScope) {
				t.Errorf("write links from an archived source: err = %v", err)
			}
			if n != 0 {
				t.Errorf("written = %d, want 0", n)
			}
			if links := orderLinks(t, pool, src); links != 0 {
				t.Errorf("the batch wrote %d links from a source that is no longer a live block of scope private", links)
			}
			if lc := raceLifecycle(t, pool, tgt); lc != "knowledge" {
				t.Errorf("target lifecycle = %s, want knowledge", lc)
			}
		})
	}
}

// TestRunDreamCycle_SourceMovedDuringEval_BooksTransient_DB: a full cycle
// whose source moves to another scope while its eval runs (the move commits
// inside the eval seam, between the pick and the link batch). The batch
// writes nothing (ErrSourceLeftScope), and the cycle books the transient
// cooldown — dream_eval_count unchanged, a stamp minutes out — instead of an
// empty eval, which would advance the count and push the block up the inert
// back-off curve (~515h with these settings) although no verdict about it was
// recorded; in its new scope it is picked again after the transient cooldown.
func TestRunDreamCycle_SourceMovedDuringEval_BooksTransient_DB(t *testing.T) {
	const (
		srcID  = "019d0000-0000-7000-9000-000000002b01"
		candID = "019d0000-0000-7000-9000-000000002b02"
		scope  = "lsc-it"
		dest   = "lsc-dest"
	)
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insertBlock(t, pool, srcID, scope, "decisions", "Left scope source block", tEarly, tEarly)
	insertBlock(t, pool, candID, scope, "projects", "Left scope candidate block", tEarly, tEarly)
	armCapRetryBlock(t, pool, srcID)
	armCapRetryBlock(t, pool, candID)
	backend := crBackend()
	seedKeywordEmbed(t, pool, backend)
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{backend})
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	r := &dream.Router{Pool: p, Blocktypes: reg, Admit: llm.Admission{Admitter: d, Class: dispatch.ClassBackground}}

	var moved atomic.Bool
	swapChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, systemPrompt, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		if !strings.Contains(systemPrompt, crEvalMarker) {
			return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: crBenign}}, nil
		}
		if moved.CompareAndSwap(false, true) {
			to := dest
			if _, _, err := store.UpdateBlock(ctx, pool, srcID, store.UpdateBlockData{Scope: &to}, []string{scope, dest}); err != nil {
				t.Errorf("move during eval: %v", err)
			}
		}
		return &llm.ChatResponse{Message: llm.Message{Role: "assistant",
			Content: `[{"target_id":"` + candID + `","type":"topical","confidence":0.9}]`}}, nil
	})

	backoff := dream.BackoffConfig{Mode: "exp", Factor: 1.6, Grace: 0, MinHours: 12, CapHours: 1080, InertOffset: 7}
	written, err := dream.RunDreamCycle(ctx, pool, r, dream.DreamOptions(), backoff, []string{scope}, dream.NoThrottle)
	if err != nil || written != 0 {
		t.Fatalf("cycle = (%d, %v), want (0, nil)", written, err)
	}
	if !moved.Load() {
		t.Fatalf("the eval seam never ran — the cycle did not reach its eval")
	}
	if n := countLinks(t, pool, srcID); n != 0 {
		t.Errorf("the cycle wrote %d links from a source that left its scope", n)
	}
	var evalCount int
	var cooldownSecs float64
	if err := pool.QueryRow(ctx,
		`SELECT dream_eval_count, EXTRACT(EPOCH FROM (dream_cooldown_until - now())) FROM context_blocks WHERE id = $1::uuid`,
		srcID).Scan(&evalCount, &cooldownSecs); err != nil {
		t.Fatalf("read block state: %v", err)
	}
	if evalCount != 0 {
		t.Errorf("dream_eval_count = %d, want 0 — no verdict was recorded", evalCount)
	}
	if cooldownSecs > 3600 {
		t.Errorf("dream_cooldown_until is %.0fs out, want the transient stamp (minutes), not the back-off", cooldownSecs)
	}
}

// linkWriteGateDDL is writeGateDDL for context_dream_links: every statement
// that updates a dream link row takes the one test_link_gate row at its end
// and holds it until commit, so a gate on that row parks a transaction right
// after its link update, with every lock it took so far.
const linkWriteGateDDL = `
CREATE TABLE test_link_gate (
    id     boolean PRIMARY KEY DEFAULT true CHECK (id),
    writes bigint  NOT NULL DEFAULT 0
);
INSERT INTO test_link_gate DEFAULT VALUES;
CREATE FUNCTION test_take_link_gate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM new_rows) THEN
        UPDATE test_link_gate SET writes = writes + 1;
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER trg_test_link_gate_upd
    AFTER UPDATE ON context_dream_links
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION test_take_link_gate();`

func TestSupersedesConfirmAndDeclarationOfOneTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn, set := orderSetup(t)
	ctx := context.Background()
	tgt := raceBlock(t, pool, "settle-note")     // older
	s1 := raceBlock(t, pool, "settle-note v2")   // newer: dream guesses s1 supersedes tgt
	s2 := raceBlock(t, pool, "settle-note v2.1") // the writer declares s2 supersedes tgt
	if n, err := dream.WriteLinks(ctx, pool, set, s1, "private", 1.0,
		[]dream.Link{{TargetID: tgt, Relationship: "supersedes", Confidence: 0.95}}); err != nil || n != 1 {
		t.Fatalf("seed guess: n=%d err=%v", n, err)
	}

	// The confirm pins s1→tgt and parks at the end of that update, before its
	// commit.
	if _, err := pool.Exec(ctx, linkWriteGateDDL); err != nil {
		t.Fatalf("install link gate: %v", err)
	}
	release := gateTx(t, pool, `SELECT 1 FROM test_link_gate FOR NO KEY UPDATE`)
	defer release()
	confirmPool := actorPool(t, dsn, "p4-confirm", nil)
	confirmed := make(chan error, 1)
	go func() {
		_, err := store.DreamLinkResolve(ctx, confirmPool, s1, tgt, "supersedes", "confirm", "", []string{"private"})
		confirmed <- err
	}()
	waitForLockWait(t, pool, "p4-confirm")

	declPool := actorPool(t, dsn, "p4-declare", nil)
	declared := make(chan error, 1)
	go func() {
		src, err := store.GetBlock(ctx, declPool, nil, s2, []string{"private"}, nil)
		if err != nil || src == nil {
			declared <- errors.Join(errors.New("read s2"), err)
			return
		}
		_, err = store.UpsertBlockSupersedes(ctx, declPool, src.Category, src.Title, src.Content, nil, nil, "private", true,
			store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: tgt, APIKeyID: "p4", Types: set})
		declared <- err
	}()
	// The declaration either finishes beside the parked confirm or waits for
	// it; both are allowed to proceed once the gate opens.
	var declErr error
	finished := false
	for deadline := time.Now().Add(30 * time.Second); !finished; {
		select {
		case declErr = <-declared:
			finished = true
		default:
		}
		if finished || lockWaiting(t, pool, "p4-declare") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the declaration neither finished nor waited within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()
	select {
	case err := <-confirmed:
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the confirm did not finish")
	}
	if !finished {
		select {
		case declErr = <-declared:
		case <-time.After(30 * time.Second):
			t.Fatalf("the declaration did not finish")
		}
	}

	var conflict *store.SupersedesConflictError
	if !errors.As(declErr, &conflict) || conflict.Kind != store.SupersedesSuperseded {
		t.Errorf("declaration of %s supersedes %s after the confirmed %s supersedes %s = %v, want a SupersedesSuperseded conflict", s2, tgt, s1, tgt, declErr)
	}
	var settled int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM context_dream_links dl
		  WHERE dl.target_block_id = $1::uuid AND dl.relationship = 'supersedes' AND `+store.SettledSupersedesSQL, tgt).Scan(&settled); err != nil {
		t.Fatalf("count settled: %v", err)
	}
	if settled != 1 {
		t.Errorf("%s has %d settling superseders, want 1", tgt, settled)
	}
}

// TestSupersedesDanglingCleanupAndScopeMove: the daily cleanup deletes the
// links of archived blocks and reconciles their supersedes targets; a scope
// move of S holds S and its supersedes targets and then sweeps the link rows
// of S. X (archived) still supersedes S through a link the cleanup deletes,
// and Y (archived) supersedes R, whose id sorts before S's: the cleanup holds
// both link rows and parks on R (a gate holds it) before it reaches S. The
// move then locks S and sweeps the link X→S. Once the gate opens, the cleanup
// locks S. Unless the two exclude each other before their row locks,
// PostgreSQL ends one of them with 40P01. A dangling link of another scope,
// cleaned up first, pins that each scope's transaction deletes its own rows.
func TestSupersedesDanglingCleanupAndScopeMove(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn, set := orderSetup(t)
	ctx := context.Background()
	// R must sort before S: the cleanup locks its targets in id order.
	a, b := raceBlock(t, pool, "dangle-note"), raceBlock(t, pool, "dangle-other")
	r, s := a, b
	if r > s {
		r, s = s, r
	}
	x := raceBlock(t, pool, "dangle-note v2")
	y := raceBlock(t, pool, "dangle-other v2")
	for _, p := range [][2]string{{x, a}, {y, b}} {
		if n, err := dream.WriteLinks(ctx, pool, set, p[0], "private", 1.0,
			[]dream.Link{{TargetID: p[1], Relationship: "supersedes", Confidence: 0.95}}); err != nil || n != 1 {
			t.Fatalf("seed %s supersedes %s: n=%d err=%v", p[0], p[1], n, err)
		}
	}
	// Archive both superseders; their links stay until the cleanup.
	for _, id := range []string{x, y} {
		if _, err := store.DeleteBlock(ctx, pool, id, []string{"private"}); err != nil {
			t.Fatalf("archive %s: %v", id, err)
		}
	}
	sup := map[string]string{a: x, b: y}
	// A dangling link in a scope that sorts before "private": the cleanup's
	// first transaction is that scope's, and it must delete that scope's rows
	// only — not the rows of "private" under its own scope's lock.
	other := func(title string) string {
		blk, err := store.UpsertBlock(ctx, pool, "race", title, title+" content", nil, nil, "a-other", true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
		return blk.ID
	}
	otherTgt, otherSrc := other("other-note"), other("other-note v2")
	if n, err := dream.WriteLinks(ctx, pool, set, otherSrc, "a-other", 1.0,
		[]dream.Link{{TargetID: otherTgt, Relationship: "topical", Confidence: 0.9}}); err != nil || n != 1 {
		t.Fatalf("seed other-scope link: n=%d err=%v", n, err)
	}
	if _, err := store.DeleteBlock(ctx, pool, otherSrc, []string{"a-other"}); err != nil {
		t.Fatalf("archive other-scope source: %v", err)
	}

	release := gateTx(t, pool, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR SHARE`, r)
	defer release()
	cleanupPool := actorPool(t, dsn, "p7-cleanup", nil)
	cleaned := make(chan error, 1)
	go func() {
		_, err := dream.CleanupDanglingLinks(ctx, cleanupPool)
		cleaned <- err
	}()
	waitForLockWait(t, pool, "p7-cleanup")
	const dest = "dangle-dest"
	moveDone := moveAsync(ctx, actorPool(t, dsn, "p7-move", nil), s, dest)
	waitForLockWait(t, pool, "p7-move")
	release()

	moveErr := awaitMove(t, moveDone)
	var cleanErr error
	select {
	case cleanErr = <-cleaned:
	case <-time.After(60 * time.Second):
		t.Fatalf("the cleanup did not finish")
	}
	if moveErr != nil {
		t.Errorf("scope move: %v", moveErr)
	}
	if cleanErr != nil {
		t.Errorf("cleanup: %v", cleanErr)
	}
	if t.Failed() {
		return
	}
	if n := orderLinks(t, pool, sup[s]); n != 0 {
		t.Errorf("the archived superseder of the moved block still has %d links", n)
	}
	if n := orderLinks(t, pool, sup[r]); n != 0 {
		t.Errorf("the archived superseder of the other target still has %d links", n)
	}
	if got := orderScope(t, pool, s); got != dest {
		t.Errorf("moved block scope = %s, want %s", got, dest)
	}
	if n := orderLinks(t, pool, otherSrc); n != 0 {
		t.Errorf("the archived source of scope a-other still has %d links", n)
	}
}

func TestSupersedesResolveDeleteAndRedeclaration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, order := range []string{"declaration_first", "resolve_first"} {
		t.Run(order, func(t *testing.T) {
			pool, dsn, set := orderSetup(t)
			ctx := context.Background()
			tgt := raceBlock(t, pool, "redecl-note")
			src := raceBlock(t, pool, "redecl-note v2")
			srcBlock, err := store.GetBlock(ctx, pool, nil, src, []string{"private"}, nil)
			if err != nil || srcBlock == nil {
				t.Fatalf("read source: %v", err)
			}
			declare := func(p *pgxpool.Pool) error {
				_, err := store.UpsertBlockSupersedes(ctx, p, srcBlock.Category, srcBlock.Title, srcBlock.Content, nil, nil, "private", true,
					store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: tgt, APIKeyID: "p2", Types: set})
				return err
			}
			if err := declare(pool); err != nil {
				t.Fatalf("seed declaration: %v", err)
			}

			resolvePool := actorPool(t, dsn, "p2-resolve", nil)
			resolved := make(chan error, 1)
			resolve := func() {
				go func() {
					_, err := store.DreamLinkResolve(ctx, resolvePool, src, tgt, "supersedes", "delete", "", []string{"private"})
					resolved <- err
				}()
			}

			if order == "declaration_first" {
				// The repeat declaration is driven by hand up to and including
				// its target lock (store.lockSupersedesPair's statements): the
				// store function cannot be paused there. The resolve then locks
				// the link row and waits for the target in its reconcile; the
				// declaration's link write follows.
				decl, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				defer func() { _ = decl.Rollback(ctx) }()
				if err := store.LockSupersedesScope(ctx, decl, "private"); err != nil {
					t.Fatalf("scope lock: %v", err)
				}
				for _, st := range []struct {
					sql  string
					args []any
				}{
					{`SELECT 1 FROM context_blocks WHERE category = $1 AND title = $2 AND scope = 'private' AND NOT is_archived FOR UPDATE`, []any{srcBlock.Category, srcBlock.Title}},
					{`SELECT 1 FROM context_blocks WHERE id = $1::uuid AND scope = 'private' AND NOT is_archived FOR NO KEY UPDATE`, []any{tgt}},
				} {
					if _, err := decl.Exec(ctx, st.sql, st.args...); err != nil {
						t.Fatalf("declaration lock: %v", err)
					}
				}
				resolve()
				waitForLockWait(t, pool, "p2-resolve")
				wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				if _, err := decl.Exec(wctx,
					`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, metadata, pinned)
					 VALUES ($1::uuid, $2::uuid, 'supersedes', 1, 1, 'private', '{"origin":"declared"}', true)
					 ON CONFLICT (source_block_id, target_block_id) DO UPDATE SET pinned = true`, src, tgt); err != nil {
					t.Errorf("declaration link write: %v", err)
				} else if err := decl.Commit(ctx); err != nil {
					t.Errorf("declaration commit: %v", err)
				}
			} else {
				// The resolve locks the link row, deletes it and parks in its
				// reconcile on the target a gate holds; the declaration then
				// locks the source and waits for the target.
				release := gateTx(t, pool, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR SHARE`, tgt)
				defer release()
				resolve()
				waitForLockWait(t, pool, "p2-resolve")
				declPool := actorPool(t, dsn, "p2-declare", nil)
				declared := make(chan error, 1)
				go func() { declared <- declare(declPool) }()
				waitForLockWait(t, pool, "p2-declare")
				release()
				select {
				case err := <-declared:
					if err != nil {
						t.Errorf("declaration: %v", err)
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("the declaration did not finish")
				}
			}
			select {
			case err := <-resolved:
				if err != nil {
					t.Errorf("resolve delete: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("the resolve did not finish")
			}
		})
	}
}
