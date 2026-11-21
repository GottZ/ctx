//go:build integration

// Issue #427, #452: PopulateTemporalReview writes the review's rows and the
// version they stand for (context_temporal_reviews) only while the block
// holds the title and content the review read; every other temporal writer
// deletes the version; TemporalReviewDueCol answers due for a block whose
// title and content no version row carries. The statements reach their rows
// by primary keys alone.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestPopulateTemporalReview|TestTemporalReview|TestRefreshTemporal' -count=1 -v
package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i452Due answers TemporalReviewDueCol for the block.
func i452Due(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var due bool
	if err := pool.QueryRow(context.Background(),
		`SELECT `+store.TemporalReviewDueCol+` FROM context_blocks WHERE id = $1`, id).Scan(&due); err != nil {
		t.Fatalf("read due: %v", err)
	}
	return due
}

// i452Versions counts the block's rows of context_temporal_reviews.
func i452Versions(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_temporal_reviews WHERE block_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

// TestTemporalReviewVersionFollowsTitleAndContent pins the due rule: a block
// is due until a review of the title and content it holds is written, stays
// reviewed through a tags write, and is due again once its title or content
// differs — before any afterwork ran. A write of the extraction's rows
// deletes the version.
func TestTemporalReviewVersionFollowsTitleAndContent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	title, content := "Retro notes", "Retro on 2026-02-10."
	b, err := store.UpsertBlock(ctx, pool, "i427", title, content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if !i452Due(t, pool, b.ID) {
		t.Fatalf("a block never reviewed is not due")
	}
	found := []time.Time{i414Day("2026-02-14")}
	for _, tc := range []struct{ name, title, content string }{
		{"other title", "Retro notes (old)", content},
		{"other content", title, "Retro on 2026-02-03."},
	} {
		applied, err := store.PopulateTemporalReview(ctx, pool, b.ID, tc.title, tc.content, found, b.CreatedAt)
		if err != nil || applied {
			t.Errorf("%s: applied=%v err=%v, want not applied", tc.name, applied, err)
		}
		if n := i452Versions(t, pool, b.ID); n != 0 || !i452Due(t, pool, b.ID) {
			t.Errorf("%s: %d version row(s), due=%v; want none and due", tc.name, n, i452Due(t, pool, b.ID))
		}
	}

	if applied, err := store.PopulateTemporalReview(ctx, pool, b.ID, title, content, found, b.CreatedAt); err != nil || !applied {
		t.Fatalf("reviewed version: applied=%v err=%v, want applied", applied, err)
	}
	if i452Due(t, pool, b.ID) {
		t.Fatalf("the reviewed version is due")
	}
	if u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Tags: []string{"retro"}}, []string{"private"}); err != nil || u == nil {
		t.Fatalf("tags update: %v", err)
	}
	if i452Due(t, pool, b.ID) {
		t.Errorf("a tags write made the reviewed version due")
	}

	newer := "Retro moved to 2026-02-17."
	u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &newer}, []string{"private"})
	if err != nil || u == nil {
		t.Fatalf("update: %v", err)
	}
	if !i452Due(t, pool, b.ID) {
		t.Errorf("a content write left the block reviewed before its afterwork ran")
	}
	if applied, err := store.RefreshTemporal(ctx, pool, u.ID, u.Content, u.CreatedAt); err != nil || !applied {
		t.Fatalf("afterwork: applied=%v err=%v", applied, err)
	}
	if n := i452Versions(t, pool, b.ID); n != 0 {
		t.Errorf("the afterwork of a content write left %d version row(s)", n)
	}

	// A block that is gone answers not applied, without an error.
	if applied, err := store.PopulateTemporalReview(ctx, pool, "019d0000-0000-7000-9000-000000042700", title, content, found, b.CreatedAt); err != nil || applied {
		t.Errorf("gone block: applied=%v err=%v, want not applied and no error", applied, err)
	}
}

// TestRefreshTemporalKeepsTheRowsOfTheReviewOfItsContent is point 3 of issue
// #452 at the store: RefreshTemporal of the content the block holds, after
// the review of that title and content, writes nothing and keeps the
// review's findings; a content written away and back (X, Y, X) is due again
// and gets the extraction's rows; rows that lack a date of the content are
// rewritten.
func TestRefreshTemporalKeepsTheRowsOfTheReviewOfItsContent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	title, content := "Budget notes", "Budget on 2026-05-04."
	b, err := store.UpsertBlock(ctx, pool, "i452", title, content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}
	own := []time.Time{i414Day("2026-05-04")}
	found := append(slices.Clone(own), i414Day("2026-05-11"))
	review := func() {
		t.Helper()
		if applied, err := store.PopulateTemporalReview(ctx, pool, b.ID, title, content, found, b.CreatedAt); err != nil || !applied {
			t.Fatalf("review: applied=%v err=%v", applied, err)
		}
	}
	review()

	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("same content: applied=%v err=%v", applied, err)
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, found...); !slices.Equal(got, want) {
		t.Errorf("same content: source times = %v, want the review's %v", got, want)
	}
	if i452Due(t, pool, b.ID) {
		t.Errorf("same content: the block is due")
	}

	// Rows that lack a date of the content (an extraction that changed since
	// the review) are rewritten, and the block is due again.
	if _, err := pool.Exec(ctx, `DELETE FROM context_temporal WHERE block_id = $1 AND source_time = $2`, b.ID, own[0]); err != nil {
		t.Fatalf("drop a date: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("rows short of a date: applied=%v err=%v", applied, err)
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, own...); !slices.Equal(got, want) {
		t.Errorf("rows short of a date: source times = %v, want the extraction's %v", got, want)
	}
	if !i452Due(t, pool, b.ID) {
		t.Errorf("rows short of a date: the rewritten block is not due")
	}

	// X, Y, X: the content returns, the rows of Y's afterwork stand for
	// neither, and the block is due.
	review()
	other := "Budget cancelled."
	for _, c := range []string{other, content} {
		u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &c}, []string{"private"})
		if err != nil || u == nil {
			t.Fatalf("update: %v", err)
		}
		if applied, err := store.RefreshTemporal(ctx, pool, u.ID, u.Content, u.CreatedAt); err != nil || !applied {
			t.Fatalf("afterwork of %q: applied=%v err=%v", c, applied, err)
		}
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, own...); !slices.Equal(got, want) {
		t.Errorf("X, Y, X: source times = %v, want the extraction's %v", got, want)
	}
	if !i452Due(t, pool, b.ID) {
		t.Errorf("X, Y, X: the block is not due, its review's findings are gone")
	}
}

// i427GenericPlan EXPLAINs sql as EXPLAIN (GENERIC_PLAN) and returns the plan
// lines and its index conditions.
func i427GenericPlan(t *testing.T, pool *pgxpool.Pool, sql string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// The simple protocol sends the statement with its placeholders unbound.
	res, err := conn.Conn().PgConn().Exec(ctx, `EXPLAIN (GENERIC_PLAN) `+sql).ReadAll()
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan, conds []string
	for _, r := range res[0].Rows {
		l := string(r[0])
		plan = append(plan, l)
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Index Cond:") {
			conds = append(conds, l)
		}
	}
	return strings.Join(plan, "\n"), conds
}

// i427PlanCorpus writes 2000 blocks in one statement and analyses them: half
// share one title and one content (one content_hash, the case of identical
// contents), half have titles of their own, the shape under which the trigram
// index on title served `title = $n` in the classify UPDATE (#433).
func i427PlanCorpus(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 SELECT 'test_i427_plan',
		        CASE WHEN g % 2 = 0 THEN 'Shared review title ' || g ELSE 'Notes on topic ' || md5(g::text) END,
		        CASE WHEN g % 2 = 0 THEN 'i427 shared content' ELSE 'i427 ' || g END,
		        'private'
		   FROM generate_series(1, 2000) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// TestPopulateTemporalReviewNeedsTheReviewedTitle: the dream's LLM findings
// land only while the block holds the title and content the review read; a
// replaced title drops them, an update of tags keeps them.
func TestPopulateTemporalReviewNeedsTheReviewedTitle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	title, content := "Review on 2026-03-15", "Notes of the review."
	b, err := store.UpsertBlock(ctx, pool, "i427", title, content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}
	base := i414Want(b.CreatedAt)

	// Tags only: the review read nothing that changed.
	if u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Tags: []string{"retro"}}, []string{"private"}); err != nil || u == nil {
		t.Fatalf("tags update: %v", err)
	}
	found := []time.Time{i414Day("2026-03-15")}
	if applied, err := store.PopulateTemporalReview(ctx, pool, b.ID, title, content, found, b.CreatedAt); err != nil || !applied {
		t.Fatalf("tags-only: applied=%v err=%v, want applied", applied, err)
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, found...); !slices.Equal(got, want) {
		t.Fatalf("source times = %v, want %v", got, want)
	}
	// The rows of the content's own dates again (phase 1's writer; the
	// afterwork of a same-content write keeps a review's rows).
	if applied, err := store.PopulateTemporal(ctx, pool, b.ID, content, store.ExtractDates(content), b.CreatedAt); err != nil || !applied {
		t.Fatalf("reset rows: applied=%v err=%v", applied, err)
	}

	// The title is replaced: the date read off the old one stays out.
	newTitle := "Review on 2026-07-20"
	if u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Title: &newTitle}, []string{"private"}); err != nil || u == nil {
		t.Fatalf("title update: %v", err)
	}
	if applied, err := store.PopulateTemporalReview(ctx, pool, b.ID, title, content, found, b.CreatedAt); err != nil || applied {
		t.Errorf("replaced title: applied=%v err=%v, want not applied", applied, err)
	}
	if got := i414Sources(t, pool, b.ID); !slices.Equal(got, base) {
		t.Errorf("source times = %v, want %v (the rows of the block's own content)", got, base)
	}
}

func TestTemporalReviewGuardPlanUsesPrimaryKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	i427PlanCorpus(t, pool)
	text, conds := i427GenericPlan(t, pool, store.TemporalReviewGuardSQLForTest)
	t.Logf("generic plan:\n%s", text)
	if !strings.Contains(text, "Index Scan using context_blocks_pkey on context_blocks") ||
		!slices.Equal(conds, []string{"Index Cond: (id = $1)"}) {
		t.Errorf("the temporal review guard must reach the row by its primary key only, plan:\n%s", text)
	}
}

// TestTemporalReviewCoversPlanUsesPrimaryKeys: RefreshTemporal's check reads
// the block, its version row and its temporal rows by their keys, whatever
// the size of the tables.
func TestTemporalReviewCoversPlanUsesPrimaryKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	i427PlanCorpus(t, pool)
	text, _ := i427GenericPlan(t, pool, store.TemporalReviewCoversSQLForTest)
	t.Logf("generic plan:\n%s", text)
	for _, want := range []string{"context_blocks_pkey", "context_temporal_reviews_pkey"} {
		if !strings.Contains(text, want) {
			t.Errorf("the check must read %s, plan:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Seq Scan") {
		t.Errorf("the check scans a table, plan:\n%s", text)
	}
}

// TestRefreshTemporalWaitsForAReviewInFlight (CodeRabbit on #464): the
// afterwork of a same-content write runs while a review of that content
// holds the block's temporal turn, uncommitted (held at its COMMIT). The
// afterwork's check of the review's rows must come after the turn, so it
// sees the review once committed and leaves its rows and version standing.
// Before, the check ran before the transaction: it saw no review, and the
// afterwork replaced the review's rows and deleted its version.
func TestRefreshTemporalWaitsForAReviewInFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	title, content := "Budget notes", "Budget on 2026-05-04."
	b, err := store.UpsertBlock(ctx, pool, "i452", title, content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}
	found := []time.Time{i414Day("2026-05-04"), i414Day("2026-05-11")}

	holdPool, hold := testdb.HoldCommitPool(t, dsn, func(sql string) bool {
		return strings.HasPrefix(strings.TrimSpace(sql), "INSERT INTO context_temporal_reviews")
	})
	reviewed := make(chan error, 1)
	go func() {
		applied, err := store.PopulateTemporalReview(ctx, holdPool, b.ID, title, content, found, b.CreatedAt)
		if err == nil && !applied {
			err = errNotApplied
		}
		reviewed <- err
	}()
	pid := hold.Wait(t)

	refreshed := make(chan error, 1)
	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		_, err := store.RefreshTemporal(ctx, pool, b.ID, content, b.CreatedAt)
		refreshed <- err
	}()
	testdb.WaitBlockedBy(t, pool, pid, waiting, "the afterwork", "advisory")
	hold.Release()
	if err := <-reviewed; err != nil {
		t.Fatalf("review: %v", err)
	}
	if err := <-refreshed; err != nil {
		t.Fatalf("afterwork: %v", err)
	}

	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, found...); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want the review's %v", got, want)
	}
	if i452Due(t, pool, b.ID) {
		t.Errorf("the afterwork deleted the version of the review that committed before its write")
	}
}

