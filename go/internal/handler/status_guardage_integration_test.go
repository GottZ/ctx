//go:build integration

// guard_review.oldest_updated_at is the aging signal of the review queue: how
// long has its head been waiting for a decision. This file pins where that
// time comes from, end to end, with the guard that produces it:
//
//   - a guard pass that flags a block leaves the block's updated_at alone, and
//     the section still reports the time of the FLAG, not the age of the
//     content;
//   - a later write that leaves the content alone (tags) moves updated_at and
//     keeps the flag, and the queue's age stays at the flag;
//   - a flagged row that carries no guard_checked_at (rows older than the key,
//     repair migrations) or an unreadable one falls back to updated_at, and one
//     such row never takes the section down for everybody else;
//   - guard_checked_at is metadata a caller can write. A value that cannot be
//     a flag time of the row (infinite, before the row existed, in the future)
//     falls back to updated_at too.
//
// Run:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestGuardReviewAging -count=1 -v

package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// agingVec builds a 1024-dim vector with up to two components set.
func agingVec(aIdx int, aW float32, bIdx int, bW float32) []float32 {
	v := make([]float32, 1024)
	v[aIdx] = aW
	if bW != 0 {
		v[bIdx] = bW
	}
	return v
}

// writeAgingBlock creates a block through the production write path and gives
// it a vector the way the embed worker does.
func writeAgingBlock(t *testing.T, pool *pgxpool.Pool, scope, title string, embedding []float32) string {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "learnings", title, "guard aging body for "+title, nil, nil,
		scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("upsert %q: %v", title, err)
	}
	if err := store.StoreEmbedding(ctx, pool, b.ID, "guard-aging-model", embedding); err != nil {
		t.Fatalf("StoreEmbedding %q: %v", title, err)
	}
	return b.ID
}

// TestGuardReviewAgingFollowsTheFlag: content written long ago, flagged by the
// guard just now. The block keeps its updated_at, and the queue's age starts
// at the flag.
func TestGuardReviewAgingFollowsTheFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "ga-flag"
	seedGuardScope(t, pool, scope, nil)

	// A pair at cosine 0.96: the guard flags both as needs_review.
	ids := []string{
		writeAgingBlock(t, pool, scope, "ga-anchor", agingVec(0, 1.0, 0, 0)),
		writeAgingBlock(t, pool, scope, "ga-peer", agingVec(0, 0.96, 1, 0.28)),
	}

	// The content is old: move the last write a long way back. Only the
	// timestamp is set by hand; the rows themselves came from the write path.
	written := time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET updated_at = $1 WHERE id = ANY($2::uuid[])`, written, ids); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	passStart := time.Now().UTC().Truncate(time.Second)
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 10)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != len(ids) {
		t.Fatalf("processed: want %d, got %d", len(ids), processed)
	}

	var oldestFlag time.Time
	for _, id := range ids {
		var status, checkedAt string
		var updatedAt time.Time
		if err := pool.QueryRow(ctx,
			`SELECT guard_status, COALESCE(metadata->>'guard_checked_at', ''), updated_at
			   FROM context_blocks WHERE id = $1::uuid`, id).Scan(&status, &checkedAt, &updatedAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if status != "needs_review" {
			t.Fatalf("block %s: guard_status = %q, want needs_review (the fixture did not reach the flagged state)", id, status)
		}
		if !updatedAt.Equal(written) {
			t.Errorf("block %s: the guard pass moved updated_at from %s to %s; flagging a block is not a write to it",
				id, written.Format(time.RFC3339), updatedAt.Format(time.RFC3339Nano))
		}
		flag, err := time.Parse(time.RFC3339, checkedAt)
		if err != nil {
			t.Fatalf("block %s: guard_checked_at %q is not RFC 3339: %v", id, checkedAt, err)
		}
		if oldestFlag.IsZero() || flag.Before(oldestFlag) {
			oldestFlag = flag
		}
	}

	gen := buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil")
	}
	for name, row := range map[string]*guardReviewStatus{"scope": gen.forScope(scope), "global": gen.globalRow()} {
		if row == nil {
			t.Fatalf("%s row missing", name)
		}
		if row.NeedsReview != 2 {
			t.Errorf("%s row: needs_review = %d, want 2", name, row.NeedsReview)
		}
		if row.OldestUpdatedAt == nil {
			t.Fatalf("%s row: oldest_updated_at is null with two flagged blocks", name)
		}
		got := *row.OldestUpdatedAt
		if got.Before(passStart) {
			t.Errorf("%s row: oldest_updated_at = %s is before the guard pass (%s); it reports the age of the content, not the time in the queue",
				name, got.Format(time.RFC3339Nano), passStart.Format(time.RFC3339))
		}
		if !got.Equal(oldestFlag) {
			t.Errorf("%s row: oldest_updated_at = %s, want the oldest guard_checked_at %s",
				name, got.Format(time.RFC3339Nano), oldestFlag.Format(time.RFC3339))
		}
	}

	// A write that leaves the content alone keeps the flag and moves
	// updated_at past it. The queue's age must not restart with that write.
	for _, id := range ids {
		if _, _, err := store.UpdateBlock(ctx, pool, id,
			store.UpdateBlockData{Tags: []string{"ga-retagged"}}, []string{scope}); err != nil {
			t.Fatalf("retag %s: %v", id, err)
		}
		var checkedAt string
		var updatedAt time.Time
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(metadata->>'guard_checked_at', ''), updated_at
			   FROM context_blocks WHERE id = $1::uuid`, id).Scan(&checkedAt, &updatedAt); err != nil {
			t.Fatalf("read %s after retag: %v", id, err)
		}
		if checkedAt == "" || !updatedAt.After(written) {
			t.Fatalf("block %s after retag: guard_checked_at %q, updated_at %s (the fixture did not reach a flagged row with a later write)",
				id, checkedAt, updatedAt.Format(time.RFC3339Nano))
		}
	}
	gen = buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil after the retag")
	}
	for name, row := range map[string]*guardReviewStatus{"scope": gen.forScope(scope), "global": gen.globalRow()} {
		if row == nil || row.OldestUpdatedAt == nil {
			t.Fatalf("%s row after retag: no oldest_updated_at (row %+v)", name, row)
		}
		if !row.OldestUpdatedAt.Equal(oldestFlag) {
			t.Errorf("%s row after retag: oldest_updated_at = %s, want the flag %s; a tags write restarted the queue's age",
				name, row.OldestUpdatedAt.Format(time.RFC3339Nano), oldestFlag.Format(time.RFC3339))
		}
	}
}

// TestGuardReviewAgingFallsBackToUpdatedAt: flagged rows the guard's stamp
// cannot be read from, the way they exist in a database that predates the key
// or was hand-repaired. They are inserted directly: the guard itself always
// writes RFC 3339, and a row older than the key has no write path any more.
func TestGuardReviewAgingFallsBackToUpdatedAt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Every time is counted back from now: a stamp is only taken when it lies
	// between the row's creation and now. created is before all of them.
	base := time.Now().UTC().Truncate(time.Second)
	created := base.Add(-365 * 24 * time.Hour)
	insert := func(scope, title, metadata string, updatedAt time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, guard_status, metadata, created_at, updated_at)
			 VALUES ('reference', $1, 'c', $2, 'needs_review', $3::jsonb, $4, $5)`,
			title, scope, metadata, created, updatedAt); err != nil {
			t.Fatalf("insert %s: %v", title, err)
		}
	}

	noKey := base.Add(-90 * 24 * time.Hour)
	unreadable := base.Add(-60 * 24 * time.Hour)
	stamped := base.Add(-30 * 24 * time.Hour)
	stampedMeta := `{"guard_checked_at": "` + stamped.Format(time.RFC3339) + `"}`

	seedGuardScope(t, pool, "ga-nokey", nil)
	insert("ga-nokey", "ga-nokey-block", `{}`, noKey)

	seedGuardScope(t, pool, "ga-unreadable", nil)
	insert("ga-unreadable", "ga-unreadable-block", `{"guard_checked_at": "not a timestamp"}`, unreadable)
	insert("ga-unreadable", "ga-unreadable-empty", `{"guard_checked_at": ""}`, unreadable.Add(time.Hour))

	// A stamped row whose last write is OLDER than its flag: the stamp wins.
	seedGuardScope(t, pool, "ga-stamped", nil)
	insert("ga-stamped", "ga-stamped-block", stampedMeta, noKey.Add(-24*time.Hour))

	// A stamped row whose last write is NEWER than its flag (a tags or
	// metadata write on a flagged block): the stamp still wins.
	seedGuardScope(t, pool, "ga-rewritten", nil)
	insert("ga-rewritten", "ga-rewritten-block", stampedMeta, base.Add(-24*time.Hour))

	gen := buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil: one unreadable guard_checked_at took the whole section down")
	}
	for scope, want := range map[string]time.Time{
		"ga-nokey":      noKey,
		"ga-unreadable": unreadable,
		"ga-stamped":    stamped,
		"ga-rewritten":  stamped,
	} {
		row := gen.forScope(scope)
		if row == nil || row.OldestUpdatedAt == nil {
			t.Errorf("scope %s: no oldest_updated_at (row %+v)", scope, row)
			continue
		}
		if !row.OldestUpdatedAt.Equal(want) {
			t.Errorf("scope %s: oldest_updated_at = %s, want %s", scope,
				row.OldestUpdatedAt.Format(time.RFC3339Nano), want.Format(time.RFC3339))
		}
	}
	// The grand total takes the minimum over the same per-row time.
	if g := gen.globalRow(); g == nil || g.OldestUpdatedAt == nil || !g.OldestUpdatedAt.Equal(noKey) {
		t.Errorf("global oldest_updated_at = %+v, want %s", g, noKey.Format(time.RFC3339))
	}
}

// TestGuardReviewAgingIgnoresAStampOutsideTheRowsLifetime: guard_checked_at is
// a metadata key, and an update that leaves the content alone stores the
// caller's metadata document as sent. A caller can therefore put any text
// there. A value that cannot be the flag time of the row must not reach the
// aggregate: an infinite one does not scan and would take the section away
// from every scope, a finite one far in the past would set the age of the
// whole queue, grand total included.
//
// The rows are flagged by the guard and then rewritten through
// store.UpdateBlock, the call the update surfaces end in.
func TestGuardReviewAgingIgnoresAStampOutsideTheRowsLifetime(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "ga-forged"
	seedGuardScope(t, pool, scope, nil)

	// Two pairs at cosine 0.96 on disjoint axes: four needs_review blocks.
	ids := []string{
		writeAgingBlock(t, pool, scope, "ga-forged-a", agingVec(0, 1.0, 0, 0)),
		writeAgingBlock(t, pool, scope, "ga-forged-b", agingVec(0, 0.96, 1, 0.28)),
		writeAgingBlock(t, pool, scope, "ga-forged-c", agingVec(2, 1.0, 0, 0)),
		writeAgingBlock(t, pool, scope, "ga-forged-d", agingVec(2, 0.96, 3, 0.28)),
	}
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 10)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != len(ids) {
		t.Fatalf("processed: want %d, got %d", len(ids), processed)
	}

	forged := []string{"-infinity", "infinity", "1900-01-01T00:00:00Z", "2999-01-01T00:00:00Z"}
	var oldestWrite time.Time
	for i, id := range ids {
		if _, _, err := store.UpdateBlock(ctx, pool, id,
			store.UpdateBlockData{Metadata: map[string]any{"guard_checked_at": forged[i]}}, []string{scope}); err != nil {
			t.Fatalf("update %s: %v", id, err)
		}
		var status, checkedAt string
		var updatedAt time.Time
		if err := pool.QueryRow(ctx,
			`SELECT guard_status, COALESCE(metadata->>'guard_checked_at', ''), updated_at
			   FROM context_blocks WHERE id = $1::uuid`, id).Scan(&status, &checkedAt, &updatedAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if status != "needs_review" || checkedAt != forged[i] {
			t.Fatalf("block %s: guard_status %q, guard_checked_at %q, want needs_review and %q (the fixture did not reach a flagged row with a caller-written stamp; if the update path now refuses the key, write it with SQL here)",
				id, status, checkedAt, forged[i])
		}
		if oldestWrite.IsZero() || updatedAt.Before(oldestWrite) {
			oldestWrite = updatedAt
		}
	}

	gen := buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil: a caller-written guard_checked_at took the section down for every scope")
	}
	for name, row := range map[string]*guardReviewStatus{"scope": gen.forScope(scope), "global": gen.globalRow()} {
		if row == nil {
			t.Fatalf("%s row missing", name)
		}
		if row.NeedsReview != len(ids) {
			t.Errorf("%s row: needs_review = %d, want %d", name, row.NeedsReview, len(ids))
		}
		if row.OldestUpdatedAt == nil {
			t.Fatalf("%s row: oldest_updated_at is null with %d flagged blocks", name, len(ids))
		}
		if !row.OldestUpdatedAt.Equal(oldestWrite) {
			t.Errorf("%s row: oldest_updated_at = %s, want %s (the oldest updated_at; none of the stamps can be a flag time of its row)",
				name, row.OldestUpdatedAt.Format(time.RFC3339Nano), oldestWrite.Format(time.RFC3339Nano))
		}
	}
}
