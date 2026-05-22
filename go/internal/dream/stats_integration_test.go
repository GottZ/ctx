//go:build integration

package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

// insertStatsTestBlock writes a context_blocks row with explicit is_meta,
// dream_checked_at and dream_cooldown_until — exposes the Stats vs PickBlock
// asymmetry that caused phantom-pending counts pre-fix (S38).
func insertStatsTestBlock(t *testing.T, pool *pgxpool.Pool, id, category string, isMeta bool, checkedAt, cooldownUntil *time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.01
	}
	vec := pgvec.NewVector(embedding)

	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, embedding, is_meta, dream_checked_at, dream_cooldown_until, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, 'private', $5, $6, $7, $8, now(), now())`,
		id, category, "stats-test-"+id[len(id)-4:], "stats-test-content", vec, isMeta, checkedAt, cooldownUntil,
	)
	if err != nil {
		t.Fatalf("insert dream block %s: %v", id, err)
	}
}

// TestStats_PendingRecheckExcludesMeta verifies that the pending_recheck counter
// only counts blocks that PickBlock would actually pick — meta blocks were
// previously counted (via category!='index') but invisible to PickBlock
// (which filters NOT is_meta). 8 phantom-pending observed in production
// 2026-05-22 (Session 38). Fix: align Stats subquery to NOT is_meta.
func TestStats_PendingRecheckExcludesMeta(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	past := time.Now().Add(-24 * time.Hour)
	expired := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(24 * time.Hour)

	// A: knowledge block, NOT is_meta, cooldown expired → should be pending.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000000a", "learnings", false, &past, &expired)
	// B: meta block, IS is_meta=true, cooldown expired → was phantom-pending pre-fix.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000000b", "learnings", true, &past, &expired)
	// C: index block, IS is_meta=true (index implies meta in production) → must not count.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000000c", "index", true, &past, &expired)
	// D: knowledge block, cooldown NOT expired → not pending yet.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000000d", "learnings", false, &past, &future)

	_, _, _, pendingRecheck, err := dream.Stats(ctx, pool, []string{"private"})
	if err != nil {
		t.Fatalf("dream.Stats: %v", err)
	}

	// Only block A satisfies all PickBlock-equivalent filters.
	if pendingRecheck != 1 {
		t.Errorf("pending_recheck = %d, want 1 (only block A is dream-pickable)", pendingRecheck)
	}
}
