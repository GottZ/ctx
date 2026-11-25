//go:build integration

// Issue #555, point 5: a block batch reads an update item's target in scope
// A and takes the locks of A and C (another item writes into C). The target
// moves A → B before the batch's lock pass and B → C during it: the lock
// pass looks for it in A and C and misses it (its snapshot still has it in
// B), and the diagnosis of the miss, a statement later, finds it in C — a
// scope the batch holds. Before, the diagnosis read that as "no live block"
// and the batch answered ErrBlockNotFound for a block that is live and
// writable; now the batch runs again and writes it.
package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// stagedHold holds, in turn, the first statement each of its matchers
// accepts: stage i is held until release[i] is closed; held[i] is closed
// once the statement stands there.
type stagedHold struct {
	mu      sync.Mutex
	next    int
	match   []func(sql string) bool
	held    []chan struct{}
	release []chan struct{}
}

func newStagedHold(match ...func(sql string) bool) *stagedHold {
	h := &stagedHold{match: match}
	for range match {
		h.held = append(h.held, make(chan struct{}))
		h.release = append(h.release, make(chan struct{}))
	}
	return h
}

func (h *stagedHold) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	h.mu.Lock()
	i := h.next
	hit := i < len(h.match) && h.match[i](d.SQL)
	if hit {
		h.next++
	}
	h.mu.Unlock()
	if hit {
		close(h.held[i])
		<-h.release[i]
	}
	return ctx
}

func (h *stagedHold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (h *stagedHold) wait(t *testing.T, i int, done <-chan error) {
	t.Helper()
	select {
	case <-h.held[i]:
	case err := <-done:
		t.Fatalf("the batch ended (%v) before stage %d", err, i)
	case <-time.After(testdb.CommitHoldBound):
		t.Fatalf("the batch did not reach stage %d", i)
	}
}

func TestBatchUpdateTargetMovedTwice(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	scopes := []string{"twice-a", "twice-b", "twice-c"}
	target := f.block(t, scopes[0], "moved twice")

	h := newStagedHold(
		// 0: the batch has read the target's scope and takes its first scope lock.
		func(sql string) bool { return strings.Contains(sql, "pg_advisory_xact_lock(hashtext('ctx:supersedes:'") },
		// 1: the lock pass's statement for the update item.
		func(sql string) bool {
			return strings.Contains(sql, "FROM context_blocks") && strings.Contains(sql, "FOR NO KEY UPDATE") && strings.Contains(sql, "id = $")
		},
		// 2: the diagnosis of its miss.
		func(sql string) bool { return strings.Contains(sql, "SELECT scope, ") && strings.Contains(sql, "updated_at FROM context_blocks") },
	)
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = h
	bp, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bp.Close)

	title := "renamed by the batch"
	done := make(chan error, 1)
	var res []store.BatchResult
	go func() {
		var err error
		res, err = store.UpsertBlocks(ctx, bp, []store.BatchUpsert{
			{Update: &store.BatchUpdate{ID: target, Data: store.UpdateBlockData{Title: &title}, WriteScopes: scopes}},
			{Category: "learnings", Title: "written into c", Content: "the other item", Scope: scopes[2], ScopeExplicit: true},
		}, "", f.set, nil)
		done <- err
	}()

	h.wait(t, 0, done)
	toB, toC := scopes[1], scopes[2]
	if _, _, err := store.UpdateBlock(ctx, f.pool, target, store.UpdateBlockData{Scope: &toB}, scopes); err != nil {
		t.Fatalf("move a → b: %v", err)
	}
	close(h.release[0])

	h.wait(t, 1, done)
	// The second move holds its commit: the batch's lock pass still sees the
	// target in b.
	mp, mh := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool { return strings.HasPrefix(strings.TrimSpace(sql), "UPDATE context_blocks") })
	moved := make(chan error, 1)
	go func() {
		_, _, err := store.UpdateBlock(ctx, mp, target, store.UpdateBlockData{Scope: &toC}, scopes)
		moved <- err
	}()
	mh.Wait(t)
	close(h.release[1])

	h.wait(t, 2, done)
	mh.Release()
	if err := <-moved; err != nil {
		t.Fatalf("move b → c: %v", err)
	}
	close(h.release[2])

	err = <-done
	var scope, got string
	if qerr := f.pool.QueryRow(ctx, `SELECT scope, title FROM context_blocks WHERE id = $1::uuid`, target).Scan(&scope, &got); qerr != nil {
		t.Fatal(qerr)
	}
	t.Logf("batch: %v; the target is in %s titled %q", err, scope, got)
	if err != nil {
		t.Fatalf("a batch whose update target moved twice answered %v for a live block in a scope it holds — want it run again and written", err)
	}
	if got != title || res[0].Block == nil {
		t.Errorf("the update item was not written: title %q, result %+v", got, res[0])
	}
}
