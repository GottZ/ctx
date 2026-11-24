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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillReleaseNeverWaitsForAHeldMemo(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	relNoWait(t, func(*testing.T, context.Context, *pgxpool.Pool) (string, store.BackfillRelease) {
		return "private", store.BackfillRelease{Class: store.EmbedFailureOversize, AllTenants: true}
	})
}

// TestBackfillReleaseNeverWaitsForAHeldMemoOfATenant: the same for a
// tenant-admin's release, whose page reads the memos from the tenant's own
// scopes.
func TestBackfillReleaseNeverWaitsForAHeldMemoOfATenant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	relNoWait(t, func(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, store.BackfillRelease) {
		tn, err := store.CreateTenant(ctx, pool, "rel-nw", "rel-nw")
		if err != nil {
			t.Fatal(err)
		}
		const scope = "rel-nw:data"
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
			t.Fatal(err)
		}
		return scope, store.BackfillRelease{Class: store.EmbedFailureOversize, TenantID: tn.ID, Hold: time.Hour}
	})
}

// relNoWait parks two issue blocks of the reader's scope, lets a content
// writer hold the first one's memo, and releases the class as the reader:
// the release must take the free memo and finish while the writer's
// transaction is open.
func relNoWait(t *testing.T, reader func(*testing.T, context.Context, *pgxpool.Pool) (string, store.BackfillRelease)) {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scope, rel := reader(t, ctx, pool)
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
	// The zero reader (neither every tenant nor one) reaches nothing.
	if res, err := store.ReleaseBackfillMemos(ctx, pool, store.BackfillRelease{Class: store.EmbedFailureOversize}); err != nil || res.Counted != 0 {
		t.Fatalf("zero reader: %+v %v, want counted 0", res, err)
	}
	dry, err := store.ReleaseBackfillMemos(ctx, pool, rel)
	if err != nil || dry.Counted != 2 {
		t.Fatalf("dry run: %+v %v, want counted 2", dry, err)
	}

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
		// Both memos count: the writer's delete of the held one is not
		// committed.
		confirmed := rel
		confirmed.Confirm, confirmed.AsOf = &dry.Counted, &dry.AsOf
		res, err := store.ReleaseBackfillMemos(ctx, pool, confirmed)
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
	// What still keeps a block out of the pick: nothing (the writer's
	// invalidation deleted one memo, the release took the other — deleted
	// for a server-admin, due for a tenant-admin).
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_failures WHERE block_id = ANY($1::uuid[]) AND next_attempt_at > now()`,
		[]string{held, free}).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d memos still park their block, want 0 (the writer's invalidation and the release took one each)", left)
	}
}

// TestBackfillReleaseHoldWithoutACapStaysInRange: with
// embed_backfill.backoff_cap = 0 (no cap) the hold of a tenant reader's
// repeat is the doubling alone. A memo of many attempts under a long
// release_hold must count as held, not fail the statement: Hold * 2^30
// seconds is past what make_interval takes (22008).
func TestBackfillReleaseHoldWithoutACapStaysInRange(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, "rel-cap0", "rel-cap0")
	if err != nil {
		t.Fatal(err)
	}
	const scope = "rel-cap0:data"
	if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := i236Tx(t, pool, func(tx pgx.Tx) error {
		b, err := store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: scope, Title: "rel236 cap0", Content: "body"}, []string{scope})
		if err == nil {
			id = b.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureOversize,
		store.NormalizeEmbedError(store.EmbedFailureOversize, store.OversizeEstimateMessage(9, 1)), time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	// A block that failed on the wire many times before it was parked keeps
	// its attempts on the memo row.
	if _, err := pool.Exec(ctx, `UPDATE context_embed_failures SET attempts = 40 WHERE block_id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []store.BackfillRelease{
		{Class: store.EmbedFailureOversize, TenantID: tn.ID, Hold: 3 * time.Hour},
		{BlockID: id, TenantID: tn.ID, Hold: 3 * time.Hour},
		{Class: store.EmbedFailureOversize, TenantID: tn.ID, Hold: 3 * time.Hour, HoldCap: 1 << 62},
	} {
		res, err := store.ReleaseBackfillMemos(ctx, pool, rel)
		if err != nil {
			t.Fatalf("dry run without a cap (%+v): %v", rel, err)
		}
		if res.Counted != 0 || res.Held != 1 || res.HeldUntil == nil {
			t.Errorf("dry run without a cap (%+v): %+v, want counted 0, held 1 with an end", rel, res)
		}
	}
}
