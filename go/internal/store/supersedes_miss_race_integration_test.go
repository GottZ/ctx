//go:build integration

// Issue #284, point 5: the lock pass of an archive or a scope move
// (lockSupersedesSource) reads the block's scope within the caller's write
// scopes and takes that scope's locks; a block outside them locks nothing.
// The caller's own statement runs after it, in a statement of its own (READ
// COMMITTED, a fresh snapshot): a block that a second writer moved into a
// write scope in between, and declared there to supersede another block, is
// archived or moved by that statement although the lock pass found no row and
// no target. The reconcile then works on the empty target list, and the target
// stays a snapshot that points at an archived block (until the daily link
// cleanup) or at a block of another scope (for good: the move's sweep deleted
// the link, so no cleanup finds it).
//
// The interleaving is held at a statement of the production path: a query
// tracer on the caller's own pool runs the second writer's calls — the
// production ones, committed — right before the caller's statement is sent.
// Every block, move and declaration goes through the store's write functions.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestSupersedesLockPassMiss' -count=20 -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// beforeStatement is a pgx tracer that calls before on the caller's goroutine
// right before each statement of its pool is sent, with the backend pid of
// the connection. Whatever before does runs between two statements of the
// caller's transaction, which stays open, with every lock it took, while it
// runs.
type beforeStatement struct {
	before func(pid uint32, sql string)
}

func (b *beforeStatement) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	b.before(conn.PgConn().PID(), d.SQL)
	return ctx
}

func (b *beforeStatement) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// hookedPool is a one-connection pool on the test database whose statements
// pass before first.
func hookedPool(t *testing.T, dsn string, before func(pid uint32, sql string)) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = &beforeStatement{before: before}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("hooked pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// missFixture: T in scope "in", S in scope "out" (outside the caller's write
// scopes). enter moves S into "in" and declares there that S supersedes T —
// two production writes, each committed — so T becomes a snapshot of S.
type missFixture struct {
	pool     *pgxpool.Pool
	dsn      string
	set      *blocktype.Set
	src, tgt string
	srcTitle string
}

func newMissFixture(t *testing.T) *missFixture {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	f := &missFixture{pool: pool, dsn: dsn, set: reg.Snapshot(), srcTitle: "miss-src"}
	tgt, err := store.UpsertBlock(ctx, pool, "race", "miss-tgt", "miss-tgt content", nil, nil, "in", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}
	src, err := store.UpsertBlock(ctx, pool, "race", f.srcTitle, f.srcTitle+" content", nil, nil, "out", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	f.src, f.tgt = src.ID, tgt.ID
	return f
}

// enter is the second writer: S moves from "out" into "in", then a store of
// S's identity in "in" declares that it supersedes T.
func (f *missFixture) enter(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	in := "in"
	if _, _, err := store.UpdateBlock(ctx, f.pool, f.src, store.UpdateBlockData{Scope: &in}, []string{"out", "in"}); err != nil {
		t.Errorf("move the source in: %v", err)
		return
	}
	if _, err := store.UpsertBlockSupersedes(ctx, f.pool, "race", f.srcTitle, f.srcTitle+" content", nil, nil, "in", true,
		store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: f.tgt, APIKeyID: "race", Types: f.set}); err != nil {
		t.Errorf("declare in scope in: %v", err)
	}
	if l := lifecycleOf(t, f.pool, f.tgt); l != "snapshot" {
		t.Errorf("the declaration left the target %s, want snapshot", l)
	}
}

// leave moves S back out of the write scope (the second writer's other half,
// for the bound test).
func (f *missFixture) leave(t *testing.T) {
	t.Helper()
	out := "out"
	if _, _, err := store.UpdateBlock(context.Background(), f.pool, f.src, store.UpdateBlockData{Scope: &out}, []string{"out", "in"}); err != nil {
		t.Errorf("move the source out: %v", err)
	}
}

func lifecycleOf(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var l string
	if err := pool.QueryRow(context.Background(), `SELECT lifecycle_state FROM context_blocks WHERE id = $1::uuid`, id).Scan(&l); err != nil {
		t.Fatalf("read lifecycle: %v", err)
	}
	return l
}

// assertTargetReleased: after the caller's commit the target is knowledge
// again, without a pointer — no snapshot of a block that is archived or in
// another scope.
func (f *missFixture) assertTargetReleased(t *testing.T, op string) {
	t.Helper()
	var lifecycle string
	var by *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id = $1::uuid`, f.tgt).Scan(&lifecycle, &by); err != nil {
		t.Fatalf("read target: %v", err)
	}
	if lifecycle != "knowledge" || by != nil {
		ptr := "<nil>"
		if by != nil {
			ptr = *by
		}
		t.Errorf("target after the %s of its superseder = %s superseded_by %s, want knowledge without pointer", op, lifecycle, ptr)
	}
}

// onceAt runs fn before the first statement that contains part, with the
// pid of the connection that sends it.
func onceAt(part string, fn func(pid uint32)) func(uint32, string) {
	var once sync.Once
	return func(pid uint32, sql string) {
		if strings.Contains(sql, part) {
			once.Do(func() { fn(pid) })
		}
	}
}

// TestSupersedesLockPassMissArchive: DeleteBlock with write scopes [in] while
// S is in "out"; before its UPDATE, S enters "in" and supersedes T there.
func TestSupersedesLockPassMissArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMissFixture(t)
	actor := hookedPool(t, f.dsn, onceAt("UPDATE context_blocks SET is_archived = true", func(uint32) { f.enter(t) }))
	b, err := store.DeleteBlock(context.Background(), actor, f.src, []string{"in"})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if b == nil {
		t.Fatalf("archive found no block, want the source archived in scope in")
	}
	f.assertTargetReleased(t, "archive")
}

// TestSupersedesLockPassMissScopeMove: UpdateBlock moves S to "dest" with
// write scopes [in, dest] while S is in "out"; before its UPDATE, S enters
// "in" and supersedes T there. The move's sweep deletes S→T (T stays behind).
func TestSupersedesLockPassMissScopeMove(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMissFixture(t)
	actor := hookedPool(t, f.dsn, onceAt("UPDATE context_blocks SET", func(uint32) { f.enter(t) }))
	dest := "dest"
	b, _, err := store.UpdateBlock(context.Background(), actor, f.src, store.UpdateBlockData{Scope: &dest}, []string{"in", dest})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if b == nil || b.Scope != dest {
		t.Fatalf("move = %+v, want the source moved to %s", b, dest)
	}
	f.assertTargetReleased(t, "scope move")
}

// TestSupersedesLockPassMissGuardArchive: the guard's single-id resolve
// 'archive' is an archive like DeleteBlock and takes the same lock pass.
func TestSupersedesLockPassMissGuardArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMissFixture(t)
	actor := hookedPool(t, f.dsn, onceAt("guard_status = 'archived_dup'", func(uint32) { f.enter(t) }))
	b, err := store.GuardResolve(context.Background(), actor, f.src, "archive", []string{"in"})
	if err != nil {
		t.Fatalf("guard resolve archive: %v", err)
	}
	if b == nil {
		t.Fatalf("guard resolve found no block, want the source archived in scope in")
	}
	f.assertTargetReleased(t, "guard archive")
}

// TestSupersedesLockPassMissRerunsAtMostThreeTimes: a second writer that moves
// S out of the write scope before every lock pass and back in before every
// archive statement makes every attempt miss. The archive runs its
// transaction at most three times, then answers the error with nothing
// written: S stays live where the last move left it.
func TestSupersedesLockPassMissRerunsAtMostThreeTimes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMissFixture(t)
	var mu sync.Mutex
	scopeReads, archives := 0, 0
	actor := hookedPool(t, f.dsn, func(_ uint32, sql string) {
		mu.Lock()
		defer mu.Unlock()
		if scopeReads > 5 {
			return // an archive without a bound would go on; this one lets it through
		}
		switch {
		case strings.Contains(sql, "SELECT scope FROM context_blocks WHERE id = $1::uuid AND scope = ANY($2::text[]) AND NOT is_archived"):
			scopeReads++
			if scopeReads > 1 {
				f.leave(t) // the previous attempt left S in "in"
			}
		case strings.Contains(sql, "UPDATE context_blocks SET is_archived = true"):
			archives++
			f.enter(t)
		}
	})
	b, err := store.DeleteBlock(context.Background(), actor, f.src, []string{"in"})
	if !errors.Is(err, store.ErrSupersedesSourceChangedForTest) {
		t.Fatalf("archive after %d misses = (%v, %v), want the source-changed error", scopeReads, b, err)
	}
	if scopeReads != 3 || archives != 3 {
		t.Errorf("the archive ran %d lock passes and %d archive statements, want 3 and 3", scopeReads, archives)
	}
	var archived bool
	var scope string
	if err := f.pool.QueryRow(context.Background(), `SELECT is_archived, scope FROM context_blocks WHERE id = $1::uuid`, f.src).Scan(&archived, &scope); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if archived || scope != "in" {
		t.Errorf("source after the refused archive: archived=%v scope=%s, want live in scope in", archived, scope)
	}
	if l := lifecycleOf(t, f.pool, f.tgt); l != "snapshot" {
		t.Errorf("target after the refused archive = %s, want the snapshot the last declaration left", l)
	}
}
