//go:build integration

// Issue #197, review threads 2 and 3: a dream supersedes guess must not close
// a cycle with writer-declared (pinned) supersedes links.
//
//   - TestDreamSupersedesWaitsForADeclaration: a declaration "B supersedes A"
//     is in flight (a second transaction holds the scope's supersedes lock
//     exclusively and B, as UpsertBlockSupersedes does, and has written its
//     link) while dream
//     writes the guess "A supersedes B". The guess must see the declaration
//     once it commits and be dropped; checked before it, the pair would end as
//     a two-block cycle with B a snapshot of A.
//   - TestDreamSupersedesRejectsAPinnedChain: B supersedes X supersedes Y
//     supersedes A, all declared; the guess "A supersedes B" closes the chain
//     and is dropped. A probe of the direct reverse link alone misses it.
//
// The in-flight declaration is written by hand on the second connection with
// the production lock function: the store function runs its own transaction
// and cannot be paused before its commit, which is the interleaving under test.
//
//	go test -tags=integration ./internal/dream/ -run 'TestDreamSupersedesWaitsForADeclaration|TestDreamSupersedesRejectsAPinnedChain' -count=20 -v
package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func raceSetup(t *testing.T) (*pgxpool.Pool, *blocktype.Set) {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	return pool, reg.Snapshot()
}

func raceBlock(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "race", title, title+" content", nil, nil, "private", true,
		store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return b.ID
}

// raceSupersedesRow reports whether source → target is a supersedes link.
func raceSupersedesRow(t *testing.T, pool *pgxpool.Pool, source, target string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_dream_links
		  WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid AND relationship = 'supersedes'`,
		source, target).Scan(&n); err != nil {
		t.Fatalf("read link: %v", err)
	}
	return n > 0
}

func raceLifecycle(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var l string
	if err := pool.QueryRow(context.Background(), `SELECT lifecycle_state FROM context_blocks WHERE id = $1::uuid`, id).Scan(&l); err != nil {
		t.Fatalf("read lifecycle: %v", err)
	}
	return l
}

func TestDreamSupersedesWaitsForADeclaration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, set := raceSetup(t)
	ctx := context.Background()
	b := raceBlock(t, pool, "race-note")    // older
	a := raceBlock(t, pool, "race-note v2") // newer: dream may guess "a supersedes b"

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
		{`SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, []any{b}},
		{`SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR NO KEY UPDATE`, []any{a}},
		{`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, metadata, pinned)
		  VALUES ($1::uuid, $2::uuid, 'supersedes', 1, 1, 'private', '{"origin":"declared"}', true)`, []any{b, a}},
		{`UPDATE context_blocks SET lifecycle_state = 'snapshot', superseded_by = $1::uuid WHERE id = $2::uuid`, []any{b, a}},
	} {
		if _, err := decl.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("declaration: %v", err)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := dream.WriteLinks(ctx, pool, set, a, "private", 1.0,
			[]dream.Link{{TargetID: b, Relationship: "supersedes", Confidence: 0.9}})
		done <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the dream batch never waited for the declaration")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := decl.Commit(ctx); err != nil {
		t.Fatalf("commit declaration: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write links: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the dream batch did not finish")
	}

	if raceSupersedesRow(t, pool, a, b) {
		t.Errorf("dream wrote %s supersedes %s against the committed declaration %s supersedes %s — a two-block cycle", a, b, b, a)
	}
	if l := raceLifecycle(t, pool, b); l == "snapshot" {
		t.Errorf("the declared successor %s became a snapshot", b)
	}
}

// TestDreamSupersedesBatchesDoNotCloseACycleTogether: two dream batches of one
// scope each guess a supersedes that is acyclic on its own but closes a cycle
// with the other's. Declared (pinned) links B→C and D→A run against
// created_at; batch 1 guesses A→B, batch 2 guesses C→D (both newer → older,
// so the V8 filter passes them). Batch 1 has walked and inserted, and is held
// before its commit (a third connection holds B, which its reconcile locks);
// batch 2 then runs. Without a lock that orders the two, batch 2's walk from
// C (B above C, nothing committed above B) misses the uncommitted A→B, both
// commit, and A→B→C→D→A is a cycle. With it, batch 2 walks after batch 1's
// commit, finds D above C through B and A, and drops its guess.
func TestDreamSupersedesBatchesDoNotCloseACycleTogether(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	set := reg.Snapshot()

	// created_at order B < A < D < C: A→B and C→D point to an older block.
	b := raceBlock(t, pool, "cycle-note b")
	a := raceBlock(t, pool, "cycle-note a")
	d := raceBlock(t, pool, "cycle-note d")
	c := raceBlock(t, pool, "cycle-note c")
	declare := func(source, target string) {
		t.Helper()
		src, err := store.GetBlock(ctx, pool, nil, source, []string{"private"}, nil)
		if err != nil || src == nil {
			t.Fatalf("read %s: %v", source, err)
		}
		if _, err := store.UpsertBlockSupersedes(ctx, pool, src.Category, src.Title, src.Content, nil, nil, "private", true,
			store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: target, APIKeyID: "race", Types: set}); err != nil {
			t.Fatalf("declare %s supersedes %s: %v", source, target, err)
		}
	}
	declare(b, c) // B supersedes C
	declare(d, a) // D supersedes A

	release := gateTx(t, pool, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR NO KEY UPDATE`, b)

	run := func(name, source, target string) <-chan writeResult {
		p := actorPool(t, dsn, name, nil)
		ch := make(chan writeResult, 1)
		go func() {
			n, err := dream.WriteLinks(ctx, p, set, source, "private", 1.0,
				[]dream.Link{{TargetID: target, Relationship: "supersedes", Confidence: 0.95}})
			ch <- writeResult{n, err}
		}()
		return ch
	}
	done1 := run("t1-batch-1", a, b)
	waitForLockWait(t, pool, "t1-batch-1") // walked, inserted, waits for B in its reconcile

	done2 := run("t1-batch-2", c, d)
	var res2 writeResult
	finished2 := false
	for deadline := time.Now().Add(30 * time.Second); !finished2; {
		select {
		case res2 = <-done2:
			finished2 = true
		default:
		}
		if finished2 || lockWaiting(t, pool, "t1-batch-2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch 2 neither finished nor waited on a lock within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()
	res1 := awaitWrite(t, "batch 1", done1)
	if !finished2 {
		res2 = awaitWrite(t, "batch 2", done2)
	}
	if res1.err != nil || res2.err != nil {
		t.Fatalf("batch 1 err=%v, batch 2 err=%v", res1.err, res2.err)
	}

	ab, cd := raceSupersedesRow(t, pool, a, b), raceSupersedesRow(t, pool, c, d)
	if ab && cd {
		t.Errorf("both guesses committed: %s→%s→%s→%s→%s is a supersedes cycle", a, b, c, d, a)
	}
	if !ab {
		t.Errorf("batch 1's guess %s supersedes %s, acyclic when it ran, is missing", a, b)
	}
}

// TestDreamSupersedesGuessWaitsForAnArchiveOfItsSource: an archive of S has
// locked S and its supersedes targets and is held before its commit (a third
// connection holds the guard-state row its UPDATE needs); a dream batch then
// guesses S supersedes L. Without the archive's shared guess lock the batch
// reads S as live, makes L a snapshot of S and commits beside the archive —
// L ends a snapshot of an archived block, and the archive's reconcile never
// saw L. With it, the batch waits for the archive, reads S as archived and
// leaves L knowledge.
func TestDreamSupersedesGuessWaitsForAnArchiveOfItsSource(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	set := reg.Snapshot()
	l := raceBlock(t, pool, "archive-note")    // older
	s := raceBlock(t, pool, "archive-note v2") // newer: dream may guess "s supersedes l"

	release := gateTx(t, pool, `SELECT 1 FROM context_guard_state FOR NO KEY UPDATE`)
	archivePool := actorPool(t, dsn, "guess-archive", nil)
	archived := make(chan error, 1)
	go func() {
		_, err := store.DeleteBlock(ctx, archivePool, s, []string{"private"})
		archived <- err
	}()
	waitForLockWait(t, pool, "guess-archive") // locked S, waits for the guard row in its UPDATE

	batchPool := actorPool(t, dsn, "guess-batch", nil)
	done := make(chan writeResult, 1)
	go func() {
		n, err := dream.WriteLinks(ctx, batchPool, set, s, "private", 1.0,
			[]dream.Link{{TargetID: l, Relationship: "supersedes", Confidence: 0.95}})
		done <- writeResult{n, err}
	}()
	waitForLockWait(t, pool, "guess-batch")
	release()

	select {
	case err := <-archived:
		if err != nil {
			t.Fatalf("archive: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("archive did not finish")
	}
	if res := awaitWrite(t, "batch", done); res.err != nil {
		t.Fatalf("batch: %v", res.err)
	}
	if lc := raceLifecycle(t, pool, l); lc == "snapshot" {
		t.Errorf("%s is a snapshot of %s, which the concurrent archive left archived", l, s)
	}
}

func TestDreamSupersedesRejectsAPinnedChain(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, set := raceSetup(t)
	ctx := context.Background()
	decl := func(source, target string) {
		t.Helper()
		src, err := store.GetBlock(ctx, pool, nil, source, []string{"private"}, nil)
		if err != nil || src == nil {
			t.Fatalf("read %s: %v", source, err)
		}
		if _, err := store.UpsertBlockSupersedes(ctx, pool, src.Category, src.Title, src.Content, nil, nil, "private", true,
			store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: target, APIKeyID: "race", Types: set}); err != nil {
			t.Fatalf("declare %s supersedes %s: %v", source, target, err)
		}
	}
	b := raceBlock(t, pool, "chain-note")    // older
	a := raceBlock(t, pool, "chain-note v2") // newer
	y := raceBlock(t, pool, "chain-y")
	x := raceBlock(t, pool, "chain-x")
	decl(y, a) // y supersedes a
	decl(x, y) // x supersedes y
	decl(b, x) // b supersedes x: b ⇝ a through two blocks

	if _, err := dream.WriteLinks(ctx, pool, set, a, "private", 1.0,
		[]dream.Link{{TargetID: b, Relationship: "supersedes", Confidence: 0.9}}); err != nil {
		t.Fatalf("write links: %v", err)
	}
	if raceSupersedesRow(t, pool, a, b) {
		t.Errorf("dream wrote %s supersedes %s, closing the declared chain %s ⇝ %s", a, b, b, a)
	}
	if l := raceLifecycle(t, pool, b); l == "snapshot" {
		t.Errorf("the head of the declared chain %s became a snapshot", b)
	}
}
