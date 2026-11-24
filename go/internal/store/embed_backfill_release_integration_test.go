//go:build integration

// Issue #236, point one: ReleaseBackfillMemos takes memo rows only, and none
// it would have to wait for. A content writer holds a block's memo row from
// its invalidation to its commit (InvalidateEmbeddingTx), and a batch writer
// holds it while it goes on to the next block: a release that waited for
// such a row while holding the memo rows of its page could close a wait
// cycle with a writer that wants one of them next. The release must skip a
// held memo and finish while the writer's transaction is open.
//
//	go test -tags=integration ./internal/store/ -run TestBackfillRelease -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillReleaseNeverWaitsForAHeldMemo(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"
	writable := []string{scope}
	ids := make([]string, 2)
	for i, title := range []string{"rel236 held", "rel236 free"} {
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			b, err := store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: scope, Title: title, Content: "body"}, writable)
			if err == nil {
				ids[i] = b.ID
			}
			return err
		}); err != nil {
			t.Fatalf("issue %s: %v", title, err)
		}
		// The arm's own park of a block (pre-wire estimate).
		if err := store.RecordEmbedFailure(ctx, pool, ids[i], store.EmbedFailureOversize,
			store.NormalizeEmbedError(store.EmbedFailureOversize, store.OversizeEstimateMessage(9, 1)), time.Minute, time.Hour); err != nil {
			t.Fatalf("park %s: %v", title, err)
		}
	}
	held, free := ids[0], ids[1]

	// The writer changes the first block's body: its invalidation releases
	// the memo and holds the memo row to the commit.
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	body := "body, edited"
	if _, err := store.UpdateIssueBlock(ctx, writer, held, store.IssueUpdate{Content: &body}, nil, writable); err != nil {
		t.Fatalf("writer: %v", err)
	}

	type result struct {
		res store.BackfillReleaseResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		counted := int64(2) // both memos, the held one not yet deleted for others
		res, err := store.ReleaseBackfillMemos(ctx, pool, store.BackfillRelease{
			Class: store.EmbedFailureOversize, AllTenants: true, Confirm: &counted,
		})
		done <- result{res, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("the release waited for a memo row a content writer holds (still running after 5 s)")
		if err := writer.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		got = <-done
	}
	if got.err != nil {
		t.Fatalf("release: %v", got.err)
	}
	if got.res.Counted != 2 || got.res.Released != 1 || got.res.Remaining != 1 {
		t.Errorf("release while the writer holds one memo: %+v, want counted 2, released 1 (the free one), remaining 1", got.res)
	}
	if err := writer.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("writer commit: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_failures WHERE block_id = ANY($1::uuid[])`,
		[]string{held, free}).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d memos left, want 0 (the writer's invalidation and the release took one each)", left)
	}
}
