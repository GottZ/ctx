//go:build integration

// Issue #491: the pull's locked decision takes the block row and then the
// mapping row; the push's section takes the entity's advisory lock, holds it
// across the wire write and then takes the block row (FOR NO KEY UPDATE) and the
// mapping row in its advance; the local writes take the block row alone
// (UpdateIssueBlock FOR UPDATE, UpdateBlock FOR NO KEY UPDATE). No path
// takes the mapping row before the block row, and the advisory lock is only
// tried, so no cycle can form. This runs all of them against one issue and
// its comment from several goroutines on a pool of four connections — the CI
// runner's pool on its two cores — and requires every call to finish, none
// with an error (a deadlock would surface as SQLSTATE 40P01, a wait cycle
// through the pool as the deadline).
//
//	taskset -c 0-1 go test -tags=integration ./internal/forge/ -run TestForgeLocksUnderConcurrency -count=3 -v
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i491ConcurrencyRounds is the number of calls each worker makes.
const i491ConcurrencyRounds = 150

func TestForgeLocksUnderConcurrency(t *testing.T) {
	setupPool, dsn := testdb.SetupTestDBWithDSN(t)
	proj := enablePush(seedIFProject(t, setupPool, "i491locks"))
	issueID, commentID := i452Pulled(t, setupPool, proj, true)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var mu sync.Mutex
	var failures []string
	fail := func(worker string, round int, err error) {
		var pgErr *pgconn.PgError
		kind := ""
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			kind = " (deadlock)"
		}
		mu.Lock()
		failures = append(failures, fmt.Sprintf("%s round %d%s: %v", worker, round, kind, err))
		mu.Unlock()
	}

	edit := func(id string, u store.IssueUpdate) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := store.UpdateIssueBlock(ctx, tx, id, u, nil, []string{proj.Scope}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	workers := map[string]func(round int) error{
		"local issue edit": func(round int) error {
			if round%2 == 0 {
				return edit(issueID, store.IssueUpdate{Title: str(fmt.Sprintf("Crane %d", round))})
			}
			return edit(issueID, store.IssueUpdate{Content: str(fmt.Sprintf("local body %d", round))})
		},
		"local comment edit": func(round int) error {
			return edit(commentID, store.IssueUpdate{Content: str(fmt.Sprintf("local comment %d", round))})
		},
		"UpdateBlock": func(round int) error {
			_, _, err := store.UpdateBlock(ctx, pool, issueID, store.UpdateBlockData{Tags: []string{fmt.Sprintf("t%d", round%3)}}, []string{proj.Scope})
			return err
		},
	}
	for _, inst := range []string{"A", "B"} {
		inst := inst
		workers["pull "+inst] = func(round int) error {
			a := applierWithRegistry(pool)
			title := "Crane"
			if round%3 == 0 {
				title = fmt.Sprintf("Crane %d", round)
			}
			if _, err := a.ApplyIssues(ctx, proj, []IssueRemote{{Number: 5, Title: title, Body: fmt.Sprintf("forge body %s%d", inst, round%4),
				State: "open", UpdatedAt: time.Unix(int64(10+round), 0).UTC()}}); err != nil {
				return err
			}
			_, err := a.ApplyComments(ctx, proj, []CommentRemote{{ID: 700, IssueNumber: 5, Body: fmt.Sprintf("forge comment %d", round%4),
				UpdatedAt: time.Unix(int64(10+round), 0).UTC()}})
			return err
		}
		workers["push "+inst] = func(int) error {
			_, err := pusherWithRegistry(pool).PushProject(ctx, proj, newPushSpy(), RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
			return err
		}
	}

	var wg sync.WaitGroup
	for name, w := range workers {
		wg.Add(1)
		go func(name string, w func(int) error) {
			defer wg.Done()
			for round := 0; round < i491ConcurrencyRounds; round++ {
				if err := w(round); err != nil {
					fail(name, round, err)
					return
				}
			}
		}(name, w)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatalf("the workers did not finish within the deadline: a wait cycle (lock or pool)")
	}
	if len(failures) > 0 {
		t.Fatalf("%d worker(s) failed:\n%s", len(failures), strings.Join(failures, "\n"))
	}
}
