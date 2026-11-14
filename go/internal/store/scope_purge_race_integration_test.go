//go:build integration

// Two review findings of the scope purge (#202, PR #254), each pinned on the
// production write paths:
//
//   - a child committed below a block of the batch while the batch waits for
//     its row lock must be part of the set the batch checks — the confirmed
//     count, the scope and the as-of bound — and not go along unseen with the
//     ON DELETE CASCADE of fk_blocks_parent;
//   - a derivative hanging below a matched block must not go with it: the
//     cascade would take it, so the run refuses and the dry run counts it.
//
//	go test -tags=integration ./internal/store/ -run 'TestScopePurge_(ChildCommitted|DerivedChild)' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// spIssueWithComment writes an issue tagged "import" and one comment under it.
func spIssueWithComment(t *testing.T, f *spFixture, scope string) (issueID, commentID string) {
	t.Helper()
	if err := pgxdb.Write(f.ctx, f.pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		iss, err := store.InsertIssueBlock(f.ctx, tx, store.IssueFields{Scope: scope, Title: "imported issue", Content: "x", Tags: []string{"import"}}, []string{scope})
		if err != nil {
			return err
		}
		c, err := store.InsertCommentBlock(f.ctx, tx, iss.ID, store.CommentFields{Author: "a", Content: "c"}, []string{scope})
		if err != nil {
			return err
		}
		issueID, commentID = iss.ID, c.ID
		return nil
	}); err != nil {
		t.Fatalf("issue + comment: %v", err)
	}
	return issueID, commentID
}

func (f *spFixture) exists(t *testing.T, id string) bool {
	return f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, id) == 1
}

func TestScopePurge_ChildCommittedWhileTheBatchWaits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	s := f.scope(t, "race")
	root, child := spIssueWithComment(t, f, s)

	q := f.admin()
	q.Scope = s
	q.Filter = store.PurgeFilter{Tags: []string{"import"}}
	dry, err := store.PurgeScope(f.ctx, f.pool, q)
	if err != nil || dry.Counted.Blocks != 2 {
		t.Fatalf("dry run = (%+v, %v), want blocks 2 (issue + comment)", dry, err)
	}

	// T2 writes a grandchild below the comment and holds its transaction open:
	// the insert holds the comment's row (FOR SHARE + the FK's KEY SHARE).
	t2, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = t2.Rollback(context.Background()) }()
	grand, err := store.InsertCommentBlock(f.ctx, t2, child, store.CommentFields{Author: "late", Content: "g"}, []string{s})
	if err != nil {
		t.Fatalf("grandchild in T2: %v", err)
	}

	type outcome struct {
		res *store.PurgeResult
		err error
	}
	done := make(chan outcome, 1)
	q.ConfirmBlocks = confirm(dry.Counted.Blocks)
	go func() {
		res, err := store.PurgeScope(f.ctx, f.pool, q)
		done <- outcome{res, err}
	}()

	// The batch must be waiting for the comment's row lock before T2 commits.
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case o := <-done:
			t.Fatalf("purge finished before it waited on T2: %+v, %v", o.res, o.err)
		default:
		}
		var n int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'
			    AND query LIKE '%FOR UPDATE%' AND query LIKE '%parent_id%'`).Scan(&n); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the purge batch to wait on T2")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := t2.Commit(f.ctx); err != nil {
		t.Fatalf("commit T2: %v", err)
	}

	o := <-done
	if !errors.Is(o.err, store.ErrPurgeConfirmMismatch) {
		t.Fatalf("purge after a grandchild committed under the batch = (%+v, %v), want ErrPurgeConfirmMismatch (3 rows would go, 2 confirmed)", o.res, o.err)
	}
	for name, id := range map[string]string{"issue": root, "comment": child, "grandchild": grand.ID} {
		if !f.exists(t, id) {
			t.Errorf("the %s was deleted by a refused run", name)
		}
	}
}

func TestScopePurge_DerivedChildRefusesTheRun(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	s := f.scope(t, "derived-child")
	issue, _ := spIssueWithComment(t, f, s)
	drv := f.derivative(t, s, "derivative-below-the-issue", nil)
	if err := pgxdb.Write(f.ctx, f.pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		return store.PutBlockParent(f.ctx, tx, drv, issue, []string{s})
	}); err != nil {
		t.Fatalf("hang the derivative below the issue: %v", err)
	}

	q := f.admin()
	q.Scope = s
	q.Filter = store.PurgeFilter{Tags: []string{"import"}}
	dry, err := store.PurgeScope(f.ctx, f.pool, q)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Counted.DerivedChildren != 1 {
		t.Fatalf("derived_children = %d, want 1 (counted %+v)", dry.Counted.DerivedChildren, dry.Counted)
	}
	q.ConfirmBlocks = confirm(dry.Counted.Blocks)
	if _, err := store.PurgeScope(f.ctx, f.pool, q); !errors.Is(err, store.ErrPurgeDerivedChild) {
		t.Fatalf("run with a derivative below a matched block = %v, want ErrPurgeDerivedChild", err)
	}
	if !f.exists(t, issue) || !f.exists(t, drv) {
		t.Fatal("a refused run deleted the issue or the derivative")
	}
}
