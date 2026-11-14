//go:build integration

package dream

import (
	"context"
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

	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 10)
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
