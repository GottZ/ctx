//go:build integration

package dream

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestFetchDailyGuardReviewAgesFromTheFlag pins the "oldest entry" line of the
// daily report's guard section. The age of the queue head counts from the time
// the guard flagged the block, not from the block's last write: content
// written a month ago and flagged today has waited zero days for a decision.
// A flagged row without guard_checked_at (older than the key, or touched by a
// repair migration) still counts from its updated_at.
//
// The flagged pair is written through the production path (store.UpsertBlock,
// store.StoreEmbedding) and flagged by guard.RunGuardBatch. Only the age of
// the content is set by hand.
func TestFetchDailyGuardReviewAgesFromTheFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"

	vec := func(aIdx int, aW float32, bIdx int, bW float32) []float32 {
		v := make([]float32, 1024)
		v[aIdx] = aW
		if bW != 0 {
			v[bIdx] = bW
		}
		return v
	}
	write := func(title string, embedding []float32) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "learnings", title, "daily guard age body for "+title, nil, nil,
			scope, false, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("upsert %q: %v", title, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "daily-guard-age-model", embedding); err != nil {
			t.Fatalf("StoreEmbedding %q: %v", title, err)
		}
		return b.ID
	}

	// A pair at cosine 0.96: the guard flags both as needs_review.
	ids := []string{
		write("dga-anchor", vec(0, 1.0, 0, 0)),
		write("dga-peer", vec(0, 0.96, 1, 0.28)),
	}

	now := time.Now().UTC()
	written := now.Add(-30 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET updated_at = $1 WHERE id = ANY($2::uuid[])`, written, ids); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 10, guard.Search{EfSearch: 800, ExactMaxCandidates: 400} /* the guard.* defaults */)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != len(ids) {
		t.Fatalf("processed: want %d, got %d", len(ids), processed)
	}
	for _, id := range ids {
		var status string
		var updatedAt time.Time
		if err := pool.QueryRow(ctx,
			`SELECT guard_status, updated_at FROM context_blocks WHERE id = $1::uuid`, id,
		).Scan(&status, &updatedAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if status != "needs_review" {
			t.Fatalf("block %s: guard_status = %q, want needs_review (the fixture did not reach the flagged state)", id, status)
		}
		if !updatedAt.Equal(written) {
			t.Errorf("block %s: the guard pass moved updated_at from %s to %s; flagging a block is not a write to it",
				id, written.Format(time.RFC3339Nano), updatedAt.Format(time.RFC3339Nano))
		}
	}

	asOf := time.Now().UTC()
	g, err := fetchDailyGuardReview(ctx, pool, scope, asOf)
	if err != nil {
		t.Fatalf("fetchDailyGuardReview: %v", err)
	}
	if g == nil {
		t.Fatal("fetchDailyGuardReview returned nil with two flagged blocks")
	}
	if g.NeedsReview != 2 {
		t.Errorf("needs_review = %d, want 2", g.NeedsReview)
	}
	if g.OldestDays != 0 {
		t.Errorf("OldestDays = %d, want 0: the blocks were flagged just now; %d is the age of their content",
			g.OldestDays, g.OldestDays)
	}

	// A flagged row that carries no guard_checked_at ages from its updated_at.
	// Such a row cannot come from the write path, so it is inserted directly.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, guard_status, updated_at)
		 VALUES ('reference', 'dga-no-key', 'c', $1, 'needs_review', $2)`,
		scope, asOf.Add(-10*24*time.Hour-time.Hour)); err != nil {
		t.Fatalf("insert row without key: %v", err)
	}
	g, err = fetchDailyGuardReview(ctx, pool, scope, asOf)
	if err != nil {
		t.Fatalf("fetchDailyGuardReview (with keyless row): %v", err)
	}
	if g == nil || g.NeedsReview != 3 {
		t.Fatalf("with keyless row: %+v, want needs_review 3", g)
	}
	if g.OldestDays != 10 {
		t.Errorf("OldestDays = %d, want 10 (the keyless row falls back to its updated_at)", g.OldestDays)
	}
}

// dgaFlaggedPair writes a pair of blocks at cosine 0.96 on the axes (axis,
// axis+1) through the production path, with the caller metadata meta, and
// returns their ids; the caller runs the guard.
func dgaFlaggedPair(t *testing.T, scope, prefix string, axis int, meta func() map[string]any, write func(scope, title string, meta map[string]any, vec []float32) string) []string {
	t.Helper()
	vec := func(aIdx int, aW float32, bIdx int, bW float32) []float32 {
		v := make([]float32, 1024)
		v[aIdx] = aW
		if bW != 0 {
			v[bIdx] = bW
		}
		return v
	}
	return []string{
		write(scope, fmt.Sprintf("%s-%02d-a", prefix, axis), meta(), vec(axis, 1.0, 0, 0)),
		write(scope, fmt.Sprintf("%s-%02d-b", prefix, axis), meta(), vec(axis, 0.96, axis+1, 0.28)),
	}
}

// TestFetchDailyGuardReviewFutureStampInAScopeOfItsOwn: the upper bound of the
// flag time (issue #238 §2). The only flagged blocks of a scope carry a stamp
// in the future — a value a caller could write before the write path kept the
// key to the guard; it is written with SQL, as no write path produces it any
// more. The report's oldest-entry age counts from their updated_at, and is
// never negative.
func TestFetchDailyGuardReviewFutureStampInAScopeOfItsOwn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "dga-future"
	write := func(scope, title string, meta map[string]any, vec []float32) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "learnings", title, "daily guard age body for "+title, nil, meta,
			scope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("upsert %q: %v", title, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "daily-guard-age-model", vec); err != nil {
			t.Fatalf("StoreEmbedding %q: %v", title, err)
		}
		return b.ID
	}
	ids := dgaFlaggedPair(t, scope, "dga-future", 0, func() map[string]any { return nil }, write)
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 10, guard.Search{EfSearch: 800, ExactMaxCandidates: 400})
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != len(ids) {
		t.Fatalf("processed: want %d, got %d", len(ids), processed)
	}
	// The content was written three days ago; the stamp lies in 2999.
	written := time.Now().UTC().Add(-3*24*time.Hour - time.Hour)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET updated_at = $1, created_at = $1,
		        metadata = jsonb_set(metadata, '{guard_checked_at}', '"2999-01-01T00:00:00Z"')
		  WHERE id = ANY($2::uuid[]) AND guard_status = 'needs_review'`, written, ids); err != nil {
		t.Fatalf("forge: %v", err)
	}
	g, err := fetchDailyGuardReview(ctx, pool, scope, time.Now().UTC())
	if err != nil {
		t.Fatalf("fetchDailyGuardReview: %v", err)
	}
	if g == nil || g.NeedsReview != 2 {
		t.Fatalf("fetchDailyGuardReview = %+v, want needs_review 2", g)
	}
	if g.OldestDays != 3 {
		t.Errorf("OldestDays = %d, want 3 (counted from updated_at; a stamp in the future cannot be a flag time)", g.OldestDays)
	}
}

// TestDailyGuardReviewReadsTheStampOncePerRow pins the cost of the flag time
// on the daily report's read (issue #238 §1) in buffers: a flagged row whose
// metadata lives out of line costs the read what ONE read of the key costs.
// The same three-statement comparison as the /api/status rollup's pin
// (handler, TestGuardReviewRollupReadsTheStampOncePerRow), for the custom and
// the generic plan, at both deployment values of random_page_cost.
func TestDailyGuardReviewReadsTheStampOncePerRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope, pairs = "dga-toast", 20
	write := func(scope, title string, meta map[string]any, vec []float32) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "learnings", title, "toasted daily body "+title, nil, meta,
			scope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("upsert %q: %v", title, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "daily-guard-age-model", vec); err != nil {
			t.Fatalf("StoreEmbedding %q: %v", title, err)
		}
		return b.ID
	}
	blob := func() map[string]any {
		raw := make([]byte, 6000)
		if _, err := rand.Read(raw); err != nil {
			t.Fatalf("random: %v", err)
		}
		return map[string]any{"blob": base64.StdEncoding.EncodeToString(raw)}
	}
	for i := 0; i < pairs; i++ {
		dgaFlaggedPair(t, scope, "dga-toast", 2*i, blob, write)
	}
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 4*pairs+10, guard.Search{EfSearch: 800, ExactMaxCandidates: 400})
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != 2*pairs {
		t.Fatalf("processed %d, want %d", processed, 2*pairs)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	const flagged = ` FROM context_blocks WHERE scope = $1 AND NOT is_archived
		  AND guard_status IN ('needs_review', 'near_duplicate', 'possible_duplicate')`
	const counts = `(count(*) FILTER (WHERE guard_status = 'needs_review'))::int,
		(count(*) FILTER (WHERE guard_status = 'near_duplicate'))::int,
		(count(*) FILTER (WHERE guard_status = 'possible_duplicate'))::int`
	none := `SELECT ` + counts + `, min(updated_at)` + flagged
	one := `SELECT ` + counts + `, min(updated_at), count(metadata->>'guard_checked_at')` + flagged
	rowsOutOfLine := int64(2 * pairs)
	for _, rpc := range testdb.RandomPageCosts {
		p := testdb.WithRandomPageCost(t, pool, rpc)
		for _, mode := range []string{"custom", "generic"} {
			explain := testdb.ExplainCustom
			if mode == "generic" {
				explain = testdb.ExplainGeneric
			}
			bNone := explain(t, p, none, scope).Buffers
			bOne := explain(t, p, one, scope).Buffers
			bDaily := explain(t, p, dailyGuardReviewSQL, scope).Buffers
			t.Logf("random_page_cost %s, %s plan: buffers none %d, one read %d, daily %d (%d rows out of line)",
				rpc, mode, bNone, bOne, bDaily, rowsOutOfLine)
			if bOne-bNone < rowsOutOfLine {
				t.Fatalf("random_page_cost %s, %s plan: one read of the key costs %d buffers over none for %d rows; the fixture's metadata is not out of line",
					rpc, mode, bOne-bNone, rowsOutOfLine)
			}
			if bDaily-bNone > bOne-bNone+rowsOutOfLine/4 {
				t.Errorf("random_page_cost %s, %s plan: the daily read spends %d buffers on the flag time, one read of the key per row costs %d: the key is read more than once per row",
					rpc, mode, bDaily-bNone, bOne-bNone)
			}
		}
	}
}
