//go:build integration

// Issue #466 point 8: the afterwork of a same-content write keeps the rows of
// the dream's review only when the review recorded the extraction the write
// would write — the same times, for the same version of the block. A date a
// later extraction no longer yields does not stay on a reviewed block, and a
// version row without the extraction it was written with (one of migration
// 189's take-over, or of an older binary) covers nothing.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestRefreshTemporal.*Extraction|TestMigration190' -count=1 -v
package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i466Content yields the dates i466Kept and i466Dropped; i466Found is a date
// the LLM found beside them.
const i466Content = "Plan on 2026-03-02, follow-up on 2026-04-13."

var (
	i466Kept    = i414Day("2026-03-02")
	i466Dropped = i414Day("2026-04-13")
	i466Found   = i414Day("2026-05-11")
)

// i466WithoutDropped is an extraction that no longer yields i466Dropped, as a
// later release's could (#437 dropped years outside 2020-2099).
func i466WithoutDropped(content string) []time.Time {
	return slices.DeleteFunc(store.ExtractDates(content), func(d time.Time) bool { return d.Equal(i466Dropped) })
}

// i466Reviewed stores a block of i466Content and records a review of it with
// the extraction in force and i466Found.
func i466Reviewed(t *testing.T, pool *pgxpool.Pool, title string) *store.Block {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "i466", title, i466Content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	i466Review(t, pool, b, title)
	return b
}

// i466Review records a review of b under title: the dates of the content, as
// the dream's phase 1 extracts them (store.ExtractDates), and i466Found.
func i466Review(t *testing.T, pool *pgxpool.Pool, b *store.Block, title string) {
	t.Helper()
	found := append(store.ExtractDates(b.Content), i466Found)
	if applied, err := store.PopulateTemporalReview(context.Background(), pool, b.ID, title, b.Content, found, b.CreatedAt); err != nil || !applied {
		t.Fatalf("review: applied=%v err=%v", applied, err)
	}
}

// i466Refreshed runs the afterwork of a same-content write and asserts the
// rows it leaves and whether the block is due.
func i466Refreshed(t *testing.T, pool *pgxpool.Pool, b *store.Block, wantDue bool, want ...time.Time) {
	t.Helper()
	if applied, err := store.RefreshTemporal(context.Background(), pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("same-content afterwork: applied=%v err=%v", applied, err)
	}
	if got, w := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, want...); !slices.Equal(got, w) {
		t.Errorf("source times = %v, want %v", got, w)
	}
	if due := i452Due(t, pool, b.ID); due != wantDue {
		t.Errorf("due = %v, want %v", due, wantDue)
	}
}

// TestRefreshTemporalRewritesAReviewOfAnotherExtraction: the review ran
// while the extraction yielded both dates; the extraction in force now yields
// one of them for the same content. The afterwork of a same-content write
// writes what the extraction yields now and deletes the version, so the
// dropped date goes and the block is due for a review that finds the LLM's
// dates again. Before, the check only asked whether the rows held every date
// of the extraction (a superset): the dropped date stayed, and the block was
// not due.
func TestRefreshTemporalRewritesAReviewOfAnotherExtraction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	b := i466Reviewed(t, pool, "Plan")

	i466Refreshed(t, pool, b, false, i466Kept, i466Dropped, i466Found)

	defer store.SetTemporalExtractionForTest(i466WithoutDropped)()
	i466Refreshed(t, pool, b, true, i466Kept)
}

// TestRefreshTemporalTakesNoVersionWithoutExtractionAsCovered: a version row
// that carries no extraction (taken over by migration 189, or written by the
// review of a binary before migration 190) does not say which extraction its
// rows hold, so it covers no same-content write: the afterwork writes the
// extraction's rows and deletes it, and the block is reviewed again.
func TestRefreshTemporalTakesNoVersionWithoutExtractionAsCovered(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	b := i466Reviewed(t, pool, "Plan")
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_temporal_reviews SET extraction = NULL WHERE block_id = $1`, b.ID); err != nil {
		t.Fatalf("clear the extraction: %v", err)
	}
	if i452Due(t, pool, b.ID) {
		t.Fatalf("a version row without extraction made the block due; the pick reads the version only")
	}
	i466Refreshed(t, pool, b, true, i466Kept, i466Dropped)
}

// TestRefreshTemporalTakesNoExtractionOfAnotherVersion: the extraction a row
// carries is bound to the version it was written with. A review of the
// binary before migration 190 rewrites an existing row's version and leaves
// its extraction as the earlier review wrote it; that extraction, written for
// another title, covers no write of the current one, even where its times
// equal the extraction in force. Without the binding the afterwork here took
// the review of the current title — made while the extraction still yielded
// the dropped date — as covering, and the date stayed.
func TestRefreshTemporalTakesNoExtractionOfAnotherVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	restore := store.SetTemporalExtractionForTest(i466WithoutDropped)
	b := i466Reviewed(t, pool, "Plan")
	restore()
	var earlier []byte
	if err := pool.QueryRow(ctx, `SELECT extraction FROM context_temporal_reviews WHERE block_id = $1`, b.ID).Scan(&earlier); err != nil || earlier == nil {
		t.Fatalf("read the extraction: %x %v", earlier, err)
	}

	title := "Plan, renamed"
	u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Title: &title}, []string{"private"})
	if err != nil || u == nil {
		t.Fatalf("rename: %v", err)
	}
	i466Review(t, pool, u, title)
	if _, err := pool.Exec(ctx, `UPDATE context_temporal_reviews SET extraction = $2 WHERE block_id = $1`, b.ID, earlier); err != nil {
		t.Fatalf("leave the earlier extraction: %v", err)
	}

	defer store.SetTemporalExtractionForTest(i466WithoutDropped)()
	i466Refreshed(t, pool, u, true, i466Kept)
}

// TestMigration190AddsTheExtractionWithoutTouchingARow: migration 190 adds
// context_temporal_reviews.extraction as a nullable column with no default —
// a catalog change, no row rewritten: the version rows of schema 189 keep
// their version, carry no extraction, and their blocks stay reviewed for the
// pick.
func TestMigration190AddsTheExtractionWithoutTouchingARow(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 189)
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "i466", "Plan", i466Content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_temporal_reviews (block_id, version)
		SELECT id, digest(content_hash || title, 'sha256') FROM context_blocks WHERE id = $1`, b.ID); err != nil {
		t.Fatalf("version row of schema 189: %v", err)
	}
	var before string
	if err := pool.QueryRow(ctx, `SELECT ctid::text FROM context_temporal_reviews WHERE block_id = $1`, b.ID).Scan(&before); err != nil {
		t.Fatalf("ctid: %v", err)
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate to 190: %v", err)
	}
	var (
		after      string
		extraction []byte
		nullable   string
		def        *string
	)
	if err := pool.QueryRow(ctx, `SELECT r.ctid::text, r.extraction, c.is_nullable, c.column_default
		  FROM context_temporal_reviews r, information_schema.columns c
		 WHERE r.block_id = $1 AND c.table_name = 'context_temporal_reviews' AND c.column_name = 'extraction'`, b.ID).
		Scan(&after, &extraction, &nullable, &def); err != nil {
		t.Fatalf("read after 190: %v", err)
	}
	if after != before || extraction != nil {
		t.Errorf("version row after 190: ctid %s (was %s), extraction %x, want the row untouched and no extraction", after, before, extraction)
	}
	if nullable != "YES" || def != nil {
		t.Errorf("extraction column: nullable=%s default=%v, want nullable without a default", nullable, def)
	}
	if i452Due(t, pool, b.ID) {
		t.Errorf("the block of a schema-189 version row is due after 190")
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
}
