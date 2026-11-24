//go:build integration

// Issue #547 point 6: the parent cycle check of a block batch and of
// /api/store with a parent (store.LinkBlockParent) walks the parent's
// ancestors; the writing transaction holds the supersedes locks of the
// scopes it writes, which serialize the parent writes of those scopes only.
// A chain that crosses scopes — a scope move keeps parent_id — let two link
// writes in different scopes each pass the walk and close a cycle together:
// a1 under a2 in scope A (a2's parent b1 lives in B) and b1 under b2 in
// scope B (b2's parent a1 lives in A).
//
// TestParentCycleAcrossScopes holds the first write at its COMMIT and runs
// the second: the second must not close the cycle. With CTX_C547_ROUNDS it
// runs that many rounds, and with CTX_C547_FREE set the two writes start
// together without a hold point; after the rounds no block of the database
// may be its own ancestor. TestParentWritersUnderOneParent measures the
// throughput of eight parallel writers under one parent (CTX_C547_SECONDS).
//
//	CTX_C547_ROUNDS=200 go test -tags=integration -p 1 ./internal/store/ -run 'TestParentCycleAcrossScopes' -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// c547Link hangs the block (category learnings, title, scope) under parent
// through the block batch, the path of /api/store with parent_id.
func (f *p547Fixture) c547Link(pool *pgxpool.Pool, scope, title, parent string) error {
	_, err := store.UpsertBlocks(context.Background(), pool, []store.BatchUpsert{{
		Category: "learnings", Title: title, Content: title + " content", Scope: scope, ScopeExplicit: true, ParentID: parent,
	}}, "", f.set, nil)
	return err
}

// c547Moved writes title under parent in parent's scope and moves it to
// scope: a block whose parent lives in another scope.
func (f *p547Fixture) c547Moved(t *testing.T, parentScope, scope, title, parent string) string {
	t.Helper()
	if err := f.c547Link(f.pool, parentScope, title, parent); err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM context_blocks WHERE title = $1 AND NOT is_archived`, title).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateBlock(context.Background(), f.pool, id, store.UpdateBlockData{Scope: &scope}, []string{parentScope, scope}); err != nil {
		t.Fatalf("move %s: %v", title, err)
	}
	return id
}

// c547Cycles counts the blocks of the database that are their own ancestor.
func c547Cycles(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `WITH RECURSIVE up(start, id, depth) AS (
		SELECT id, parent_id, 1 FROM context_blocks WHERE parent_id IS NOT NULL
		UNION ALL
		SELECT up.start, b.parent_id, up.depth + 1 FROM up JOIN context_blocks b ON b.id = up.id
		 WHERE b.parent_id IS NOT NULL AND up.id <> up.start AND up.depth < 64)
		SELECT count(DISTINCT start)::int FROM up WHERE id = start`).Scan(&n); err != nil {
		t.Fatalf("cycle count: %v", err)
	}
	return n
}

func c547Env(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func TestParentCycleAcrossScopes(t *testing.T) {
	f := newP547Fixture(t)
	rounds := c547Env("CTX_C547_ROUNDS", 1)
	free := os.Getenv("CTX_C547_FREE") != ""
	var refused, both int
	for r := 0; r < rounds; r++ {
		p := fmt.Sprintf("c547-%d", r)
		if free {
			p = "f" + p
		}
		a, b := p+"-a", p+"-b"
		a1 := f.block(t, a, p+" a1")
		b1 := f.block(t, b, p+" b1")
		a2 := f.c547Moved(t, b, a, p+" a2", b1) // in A, its parent b1 in B
		b2 := f.c547Moved(t, a, b, p+" b2", a1) // in B, its parent a1 in A

		var w1Err, w2Err error
		if free {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); w1Err = f.c547Link(f.pool, a, p+" a1", a2) }()
			go func() { defer wg.Done(); w2Err = f.c547Link(f.pool, b, p+" b1", b2) }()
			wg.Wait()
		} else {
			w1Pool, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool {
				return strings.Contains(sql, "UPDATE context_blocks SET parent_id")
			})
			w1Done := make(chan struct{})
			go func() { defer close(w1Done); w1Err = f.c547Link(w1Pool, a, p+" a1", a2) }()
			pid := hold.Wait(t)
			w2Done := make(chan struct{})
			go func() { defer close(w2Done); w2Err = f.c547Link(f.pool, b, p+" b1", b2) }()
			p547WaitOn(t, f.pool, pid, w2Done) // the second waits for the first, or (base) is through
			hold.Release()
			<-w1Done
			<-w2Done
			w1Pool.Close()
		}
		for _, err := range []error{w1Err, w2Err} {
			if err != nil && !errors.Is(err, store.ErrLinkScopeViolation) {
				t.Fatalf("round %d: link write: %v", r, err)
			}
		}
		switch {
		case w1Err == nil && w2Err == nil:
			both++
		case w1Err != nil || w2Err != nil:
			refused++
		}
	}
	cycles := c547Cycles(t, f.pool)
	t.Logf("%d crossed rounds (free %v): both writes through %d, one refused %d; blocks on a cycle: %d", rounds, free, both, refused, cycles)
	if cycles != 0 {
		t.Errorf("%d blocks are their own ancestor after %d crossed rounds, want none", cycles, rounds)
	}
}

// TestParentWritersUnderOneParent writes children under one parent from eight
// goroutines for CTX_C547_SECONDS (default 2) and logs the writes per second
// (the price #550 declared: 182–350/s). It pins only that every write goes
// through.
func TestParentWritersUnderOneParent(t *testing.T) {
	f := newP547Fixture(t)
	parent := f.block(t, "c547-tp", "c547 parent")
	secs := c547Env("CTX_C547_SECONDS", 2)
	var n, fails atomic.Int64
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				if err := f.c547Link(f.pool, "c547-tp", fmt.Sprintf("c547 child %d-%d", w, i), parent); err != nil {
					fails.Add(1)
					t.Errorf("writer %d: %v", w, err)
					return
				}
				n.Add(1)
			}
		}(w)
	}
	wg.Wait()
	t.Logf("8 parallel parent writers under one parent: %d writes in %d s = %.0f/s, %d failed", n.Load(), secs, float64(n.Load())/float64(secs), fails.Load())
}
