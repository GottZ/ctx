//go:build integration

package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

// insertStatsTestBlock writes a context_blocks row with explicit type_name,
// dream_checked_at and dream_cooldown_until — exposes the Stats vs PickBlock
// asymmetry that caused phantom-pending counts pre-fix (S38). Since WF T8,
// eligibility is the dream.linkable type allowlist (the retired is_meta
// column's generalization) — the fixture sets type_name directly.
func insertStatsTestBlock(t *testing.T, pool *pgxpool.Pool, id, category, typeName string, checkedAt, cooldownUntil *time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.01
	}
	vec := pgvec.NewVector(embedding)

	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name, dream_checked_at, dream_cooldown_until, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, 'private', $5, $6, $7, $8, now(), now())`,
		id, category, "stats-test-"+id[len(id)-4:], "stats-test-content", vec, typeName, checkedAt, cooldownUntil,
	)
	if err != nil {
		t.Fatalf("insert dream block %s: %v", id, err)
	}
}

// statsLinkable boots the registry from the migrated test DB (M072 seeds)
// and returns its DreamLinkableTypes — the same resolution path production
// uses, never a test-local list.
func statsLinkable(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	return reg.Snapshot().DreamLinkableTypes()
}

// TestStats_PendingRecheckExcludesNonLinkable verifies that the
// pending_recheck counter only counts blocks that PickBlock would actually
// pick — system-meta blocks were phantom-pending pre-S38 (8 observed in
// production 2026-05-22); since WF T8 the shared dreamEligibleWhere fragment
// makes Stats and PickBlock structurally identical.
func TestStats_PendingRecheckExcludesNonLinkable(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	past := time.Now().Add(-24 * time.Hour)
	expired := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(24 * time.Hour)

	// A: knowledge block, cooldown expired → should be pending.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000000a", "learnings", "knowledge", &past, &expired)
	// B: system-meta block (dream.linkable=false seed), cooldown expired →
	// was phantom-pending pre-S38.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000000b", "learnings", "system-meta", &past, &expired)
	// C: index block, system-meta type (index implies meta in production) → must not count.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000000c", "index", "system-meta", &past, &expired)
	// D: knowledge block, cooldown NOT expired → not pending yet.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000000d", "learnings", "knowledge", &past, &future)

	_, _, _, pendingRecheck, err := dream.Stats(ctx, pool, []string{"private"}, statsLinkable(t, pool))
	if err != nil {
		t.Fatalf("dream.Stats: %v", err)
	}

	// Only block A satisfies all PickBlock-equivalent filters.
	if pendingRecheck != 1 {
		t.Errorf("pending_recheck = %d, want 1 (only block A is dream-pickable)", pendingRecheck)
	}
}

// TestStatsQueueDepth_LinkableFalseTypeExcluded pins the WF T8 gate: a
// REGISTERED type with dream.linkable=false (≠ system-meta) is out of every
// dream counter AND out of PickBlock — pre-T8 only is_meta sieved, so such a
// type WAS counted and picked. With the seed defaults the counts stay
// identical to the pre-T8 NOT-is_meta world (prod before-counts in the wave
// return: 819/819 across Stats total/checked and the QueueDepth CTE).
func TestStatsQueueDepth_LinkableFalseTypeExcluded(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, config)
		 VALUES ('wf-nolink', '_global', '{"v":1,"dream":{"linkable":false}}'::jsonb)`); err != nil {
		t.Fatalf("register type: %v", err)
	}

	past := time.Now().Add(-24 * time.Hour)
	expired := time.Now().Add(-1 * time.Hour)
	// A: knowledge, pickable now. B: wf-nolink, would be pickable if the type sieve failed.
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000001a", "learnings", "knowledge", &past, &expired)
	insertStatsTestBlock(t, pool, "019e9999-0000-7000-9000-00000000001b", "learnings", "wf-nolink", &past, &expired)

	linkable := statsLinkable(t, pool) // boots AFTER the insert — wf-nolink is registered but not linkable

	total, checked, _, pending, err := dream.Stats(ctx, pool, []string{"private"}, linkable)
	if err != nil {
		t.Fatalf("dream.Stats: %v", err)
	}
	if total != 1 || checked != 1 || pending != 1 {
		t.Errorf("Stats = total %d, checked %d, pending %d; want 1/1/1 (wf-nolink excluded)", total, checked, pending)
	}

	q, err := dream.QueueDepth(ctx, pool, []string{"private"}, linkable)
	if err != nil {
		t.Fatalf("dream.QueueDepth: %v", err)
	}
	if q.PickableNow != 1 {
		t.Errorf("QueueDepth.PickableNow = %d, want 1 (wf-nolink excluded)", q.PickableNow)
	}

	// PickBlock itself: only A may ever be claimed. nil scopes = no scope
	// conjunct (T12) — this probe is about the type allowlist, not tenant scope.
	block, err := dream.PickBlock(ctx, pool, linkable, nil, dream.PickClaimTTL(nil))
	if err != nil {
		t.Fatalf("PickBlock: %v", err)
	}
	if block == nil {
		t.Fatal("PickBlock = nil, want block A")
	}
	if block.ID != "019e9999-0000-7000-9000-00000000001a" {
		t.Errorf("PickBlock picked %s, want block A (wf-nolink must never be picked)", block.ID)
	}
	// Second pick: A is claimed (transient cooldown), B must NOT surface.
	block2, err := dream.PickBlock(ctx, pool, linkable, nil, dream.PickClaimTTL(nil))
	if err != nil {
		t.Fatalf("PickBlock #2: %v", err)
	}
	if block2 != nil {
		t.Errorf("PickBlock #2 = %s, want nil (wf-nolink must never be picked)", block2.ID)
	}
}
