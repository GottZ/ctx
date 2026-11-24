//go:build integration

// Issue #284, points 2 (guard batch resolve) and 3 (archive without
// reconcile) for the operator's guard resolves:
//
//   - an archive by GuardResolve or GuardResolveBatch ends the supersedes of
//     the archived block: its targets are reconciled in the same transaction,
//     as DeleteBlock's are (a target no other live block supersedes is
//     knowledge again at once, not a snapshot of an archived block until the
//     daily link cleanup);
//   - the batch locks its blocks in one order with every other path of the
//     scope: a declaration (scope lock, then its source, then its target) in
//     flight while the batch locks the target first and then waits for the
//     source closed a lock cycle that PostgreSQL ended with 40P01.
//
// Every block is written through the store's write functions, every flag
// through guard.RunGuardBatch; the interleaving is held at a statement of the
// production path (a query tracer on the declaration's pool).
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestGuardResolve.*Supersedes|TestGuardResolveBatchAndADeclaration' -count=20 -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// grSearch is the guard.* default search.
var grSearch = guard.Search{EfSearch: 800, ExactMaxCandidates: 400}

// grFixture holds a checked neighbour m on axis 10: a block written with
// grFlaggedVec is flagged needs_review against it by the next guard run.
type grFixture struct {
	pool *pgxpool.Pool
	dsn  string
	set  *blocktype.Set
	m    string
}

func newGRFixture(t *testing.T) *grFixture {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	f := &grFixture{pool: pool, dsn: dsn, set: reg.Snapshot()}
	f.m = f.block(t, "gr-neighbour", gkVec(10, 1, 0, 0))
	f.guard(t)
	return f
}

// grFlaggedVec is at cosine 0.96 to m: needs_review (0.92 ≤ s < 0.98).
func grFlaggedVec() []float32 { return gkVec(10, 0.96, 11, 0.28) }

func (f *grFixture) block(t *testing.T, title string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, f.pool, "gr", title, title+" content", nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	if vec != nil {
		f.embed(t, b.ID, vec)
	}
	return b.ID
}

func (f *grFixture) embed(t *testing.T, id string, vec []float32) {
	t.Helper()
	if err := store.StoreEmbedding(context.Background(), f.pool, id, "gr", vec); err != nil {
		t.Fatalf("embed %s: %v", id, err)
	}
}

func (f *grFixture) declare(t *testing.T, pool *pgxpool.Pool, title, target string) (*store.Block, error) {
	t.Helper()
	return store.UpsertBlockSupersedes(context.Background(), pool, "gr", title, title+" content", nil, nil, "private", true,
		store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: target, APIKeyID: "gr", Types: f.set})
}

func (f *grFixture) guard(t *testing.T) {
	t.Helper()
	if _, err := guard.RunGuardBatch(context.Background(), f.pool, f.set, 50, grSearch); err != nil {
		t.Fatalf("guard run: %v", err)
	}
}

func (f *grFixture) guardStatus(t *testing.T, id string) (status string, archived bool) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(guard_status, ''), is_archived FROM context_blocks WHERE id = $1::uuid`, id).Scan(&status, &archived); err != nil {
		t.Fatalf("read guard status: %v", err)
	}
	return status, archived
}

func (f *grFixture) targetState(t *testing.T, id string) (lifecycle string, by *string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id = $1::uuid`, id).Scan(&lifecycle, &by); err != nil {
		t.Fatalf("read target: %v", err)
	}
	return lifecycle, by
}

func ptrText(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestGuardResolveArchiveReconcilesSupersedesTargets: S declares that it
// supersedes T (T a snapshot of S) and is flagged by the guard; the operator
// archives S. T must be knowledge again, without a pointer.
func TestGuardResolveArchiveReconcilesSupersedesTargets(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			f := newGRFixture(t)
			ctx := context.Background()
			tgt := f.block(t, "gr-target", nil)
			declared, err := f.declare(t, f.pool, "gr-source", tgt)
			if err != nil {
				t.Fatalf("declare: %v", err)
			}
			src := declared.ID
			f.embed(t, src, grFlaggedVec())
			f.guard(t)
			if st, _ := f.guardStatus(t, src); st != "needs_review" {
				t.Fatalf("source guard status = %q, want needs_review", st)
			}
			if l, _ := f.targetState(t, tgt); l != "snapshot" {
				t.Fatalf("target before the resolve = %s, want snapshot", l)
			}
			if mode == "single" {
				if b, err := store.GuardResolve(ctx, f.pool, src, "archive", []string{"private"}); err != nil || b == nil {
					t.Fatalf("guard resolve = (%v, %v)", b, err)
				}
			} else {
				resolved, skipped, err := store.GuardResolveBatch(ctx, f.pool, []string{src}, "archive", []string{"private"})
				if err != nil || len(resolved) != 1 || len(skipped) != 0 {
					t.Fatalf("guard resolve batch = (%v, %v, %v), want one resolved", resolved, skipped, err)
				}
			}
			if _, archived := f.guardStatus(t, src); !archived {
				t.Fatalf("source not archived")
			}
			if l, by := f.targetState(t, tgt); l != "knowledge" || by != nil {
				t.Errorf("target after the guard archive of its superseder = %s superseded_by %s, want knowledge without pointer", l, ptrText(by))
			}
		})
	}
}

// TestGuardResolveBatchAndADeclarationOfOneScope: T sorts before S. A store
// of S declaring that it supersedes T holds the scope's supersedes lock and S
// and is about to lock T when a guard resolve batch over [T, S] starts. The
// batch must not lock T and then wait for S: it waits for the declaration,
// which then runs through, and resolves after it. Neither side may end with
// 40P01; an archive of S then reconciles T.
func TestGuardResolveBatchAndADeclarationOfOneScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, resolution := range []string{"archive", "keep"} {
		t.Run(resolution, func(t *testing.T) {
			f := newGRFixture(t)
			ctx := context.Background()
			// Two plain blocks; the one with the smaller id is the target.
			a, b := f.block(t, "gr-pair-a", nil), f.block(t, "gr-pair-b", nil)
			tgt, src, srcTitle := a, b, "gr-pair-b"
			if tgt > src {
				tgt, src, srcTitle = b, a, "gr-pair-a"
			}
			f.embed(t, src, grFlaggedVec())
			f.guard(t)
			if st, _ := f.guardStatus(t, src); st != "needs_review" {
				t.Fatalf("source guard status = %q, want needs_review", st)
			}

			type result struct {
				resolved []store.Block
				err      error
			}
			batchDone := make(chan result, 1)
			finished := make(chan struct{})
			started := false
			declPool := hookedPool(t, f.dsn, onceAt("SELECT type_name, lifecycle_state = 'chunk'", func(pid uint32) {
				// The declaration holds the scope lock and S; its next
				// statement locks T.
				started = true
				go func() {
					resolved, _, err := store.GuardResolveBatch(ctx, f.pool, []string{tgt, src}, resolution, []string{"private"})
					batchDone <- result{resolved, err}
					close(finished)
				}()
				testdb.WaitBlockedBy(t, f.pool, pid, finished, "the guard resolve batch", "")
			}))
			_, declErr := f.declare(t, declPool, srcTitle, tgt)
			if !started {
				t.Fatalf("the declaration never reached its target lock")
			}
			res := <-batchDone
			for who, err := range map[string]error{"declaration": declErr, "guard resolve batch": res.err} {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
					t.Errorf("%s ended with a deadlock: %v", who, err)
				} else if err != nil {
					t.Errorf("%s: %v", who, err)
				}
			}
			if t.Failed() {
				return
			}
			_, archived := f.guardStatus(t, src)
			l, by := f.targetState(t, tgt)
			switch resolution {
			case "archive":
				if !archived {
					t.Errorf("source not archived by the batch")
				}
				if l != "knowledge" || by != nil {
					t.Errorf("target after the archive of its declared superseder = %s superseded_by %s, want knowledge", l, ptrText(by))
				}
			case "keep":
				if archived || l != "snapshot" || ptrText(by) != src {
					t.Errorf("after keep: source archived=%v, target %s superseded_by %s, want the declaration standing", archived, l, ptrText(by))
				}
			}
		})
	}
}

// TestGuardResolveBatchHoldsOffAContentWrite: the batch locks its rows FOR NO
// KEY UPDATE (no longer FOR UPDATE). A content write of a resolved block —
// which takes no supersedes lock, only the row — must still wait for the
// batch and then write on the resolved row: after an archive it finds no live
// block, after a keep it writes the new content.
func TestGuardResolveBatchHoldsOffAContentWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, resolution := range []string{"archive", "keep"} {
		t.Run(resolution, func(t *testing.T) {
			f := newGRFixture(t)
			ctx := context.Background()
			src := f.block(t, "gr-held", grFlaggedVec())
			f.guard(t)
			if st, _ := f.guardStatus(t, src); st != "needs_review" {
				t.Fatalf("guard status = %q, want needs_review", st)
			}
			type result struct {
				b   *store.Block
				err error
			}
			writeDone := make(chan struct{})
			var write result
			started := false
			batchPool := hookedPool(t, f.dsn, onceAt("'guard_resolution', $3::text", func(pid uint32) {
				started = true
				go func() {
					defer close(writeDone)
					content := "gr-held rewritten"
					b, _, err := store.UpdateBlock(ctx, f.pool, src, store.UpdateBlockData{Content: &content}, []string{"private"})
					write = result{b, err}
				}()
				testdb.WaitBlockedBy(t, f.pool, pid, writeDone, "the content write", "transactionid")
			}))
			resolved, _, err := store.GuardResolveBatch(ctx, batchPool, []string{src}, resolution, []string{"private"})
			if !started {
				t.Fatalf("the batch never reached its update")
			}
			<-writeDone
			if err != nil || len(resolved) != 1 {
				t.Fatalf("guard resolve batch = (%v, %v), want one resolved", resolved, err)
			}
			var content string
			_, archived := f.guardStatus(t, src)
			if err := f.pool.QueryRow(ctx, `SELECT content FROM context_blocks WHERE id = $1::uuid`, src).Scan(&content); err != nil {
				t.Fatal(err)
			}
			switch resolution {
			case "archive":
				if write.err != nil || write.b != nil || !archived || content == "gr-held rewritten" {
					t.Errorf("content write after the archive = (%v, %v), archived=%v content=%q; want not found on the archived block", write.b, write.err, archived, content)
				}
			case "keep":
				if write.err != nil || write.b == nil || archived || content != "gr-held rewritten" {
					t.Errorf("content write after the keep = (%v, %v), archived=%v content=%q; want the write on the kept block", write.b, write.err, archived, content)
				}
			}
		})
	}
}

// TestGuardResolveBatchOfABlockMovedBeforeItsLock: the batch reads the scopes
// of its ids without a lock and then takes their supersedes locks. A block
// that moves to another write scope in between is not in a locked scope when
// the batch locks its rows: the batch runs again, reads the new scope, and
// resolves the block there — not skipped as not found.
func TestGuardResolveBatchOfABlockMovedBeforeItsLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, resolution := range []string{"archive", "keep"} {
		t.Run(resolution, func(t *testing.T) {
			f := newGRFixture(t)
			ctx := context.Background()
			src := f.block(t, "gr-moving", grFlaggedVec())
			f.guard(t)
			if st, _ := f.guardStatus(t, src); st != "needs_review" {
				t.Fatalf("guard status = %q, want needs_review", st)
			}
			writes := []string{"private", "elsewhere"}
			batchPool := hookedPool(t, f.dsn, onceAt("pg_advisory_xact_lock", func(uint32) {
				dest := "elsewhere"
				if _, _, err := store.UpdateBlock(ctx, f.pool, src, store.UpdateBlockData{Scope: &dest}, writes); err != nil {
					t.Errorf("move: %v", err)
				}
			}))
			resolved, skipped, err := store.GuardResolveBatch(ctx, batchPool, []string{src}, resolution, writes)
			if err != nil || len(resolved) != 1 || len(skipped) != 0 {
				t.Fatalf("guard resolve batch of a block moved before its lock = (%v, %v, %v), want it resolved", resolved, skipped, err)
			}
			if resolved[0].Scope != "elsewhere" {
				t.Errorf("resolved in scope %s, want elsewhere", resolved[0].Scope)
			}
		})
	}
}

// TestGuardResolveBatchTakesItsScopeLocksInOneOrder: a batch over blocks of
// two scopes takes the two supersedes locks in the order a block batch
// (UpsertBlocks) takes them, sorted by bytes. A block batch over the same two
// scopes that starts between the resolve's two locks waits for the first one
// and holds nothing; in another order the two would hold one lock each and
// wait for the other's (40P01). The block of the later scope is written first,
// so the scope read returns the scopes out of order.
func TestGuardResolveBatchTakesItsScopeLocksInOneOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, resolution := range []string{"archive", "keep"} {
		t.Run(resolution, func(t *testing.T) {
			f := newGRFixture(t)
			ctx := context.Background()
			mk := func(title, scope string) string {
				b, err := store.UpsertBlock(ctx, f.pool, "gr", title, title+" content", nil, nil, scope, true, store.SensitivityWrite{}, "")
				if err != nil {
					t.Fatalf("seed %s: %v", title, err)
				}
				return b.ID
			}
			zb, ya := mk("gr-order zb", "zb"), mk("gr-order ya", "ya")
			locks := 0
			done := make(chan struct{})
			var batchErr error
			resolvePool := hookedPool(t, f.dsn, func(pid uint32, sql string) {
				if !strings.Contains(sql, "hashtext('ctx:supersedes:'") {
					return
				}
				if locks++; locks != 2 {
					return
				}
				go func() {
					defer close(done)
					_, batchErr = store.UpsertBlocks(ctx, f.pool, []store.BatchUpsert{
						{Category: "gr", Title: "gr-order ya", Content: "ya changed", Scope: "ya"},
						{Category: "gr", Title: "gr-order zb", Content: "zb changed", Scope: "zb"},
					}, "", f.set, nil)
				}()
				testdb.WaitBlockedBy(t, f.pool, pid, done, "the block batch", "advisory")
			})
			_, _, resolveErr := store.GuardResolveBatch(ctx, resolvePool, []string{ya, zb}, resolution, []string{"ya", "zb"})
			if locks < 2 {
				t.Fatalf("the resolve took %d scope locks, want 2", locks)
			}
			<-done
			if resolveErr != nil || batchErr != nil {
				t.Errorf("guard resolve batch = %v, block batch = %v, want both through", resolveErr, batchErr)
			}
		})
	}
}

// TestGuardResolveBatchLocksItsRowsInIdOrder: the batch locks its rows in id
// order whatever plan reads them. Here the higher id comes first in the heap
// (the lower block was rewritten after it) and the rows are read by a
// sequential scan; a second transaction holds the lower block and then locks
// the higher one, the order every id-ordered locker keeps (a reconcile, a
// lock pass). In id order the batch waits for the lower block holding
// nothing; in heap order it holds the higher one and the second
// transaction's lock closes a cycle (40P01).
func TestGuardResolveBatchLocksItsRowsInIdOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newGRFixture(t)
	ctx := context.Background()
	lo, hi := f.block(t, "gr-heap a", nil), f.block(t, "gr-heap b", nil)
	if lo > hi {
		lo, hi = hi, lo
	}
	// A new row version of the lower block, behind the higher one in the heap.
	if _, _, err := store.UpdateBlock(ctx, f.pool, lo, store.UpdateBlockData{Metadata: map[string]any{"gr": "moved in the heap"}}, []string{"private"}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	seqScan := testdb.WithSessionSettings(t, f.pool, map[string]string{"enable_indexscan": "off", "enable_bitmapscan": "off"})

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var holderPid uint32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR NO KEY UPDATE`, lo); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var batchErr error
	go func() {
		defer close(done)
		_, _, batchErr = store.GuardResolveBatch(ctx, seqScan, []string{hi, lo}, "keep", []string{"private"})
	}()
	testdb.WaitBlockedBy(t, f.pool, holderPid, done, "the guard resolve batch", "transactionid")
	_, holderErr := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR NO KEY UPDATE`, hi)
	if holderErr == nil {
		holderErr = holder.Commit(ctx)
	}
	<-done
	if holderErr != nil || batchErr != nil {
		t.Errorf("id-ordered locker = %v, guard resolve batch = %v, want both through", holderErr, batchErr)
	}
}
