//go:build integration

// A guard pass records its decision on the block (guard_status, the guard_*
// metadata keys). This file pins what that does to the block's updated_at
// against a real database:
//
//   - marking a block as checked (clean, needs_review) leaves updated_at where
//     the last write put it;
//   - an auto-archive moves it, because the block leaves every default read.
//
// The blocks are written through the production path (store.UpsertBlock, then
// store.StoreEmbedding the way the embed worker writes the vector), so the
// timestamps under test are the ones a real write produces, not values a
// fixture chose.
//
// Run:
//
//	go test -tags=integration -p 1 ./internal/guard/ -run TestRunGuardBatch_UpdatedAt -count=1 -v

package guard_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// writeGuardBlock creates a block through store.UpsertBlock and gives it a
// vector through store.StoreEmbedding.
func writeGuardBlock(t *testing.T, pool *pgxpool.Pool, title string, embedding []float32) string {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "learnings", title, "guard updated_at body for "+title, nil, nil,
		"private", false, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("upsert %q: %v", title, err)
	}
	if err := store.StoreEmbedding(ctx, pool, b.ID, "guard-updated-at-model", embedding); err != nil {
		t.Fatalf("StoreEmbedding %q: %v", title, err)
	}
	return b.ID
}

// blockTimes reads the two timestamps of a block.
func blockTimes(t *testing.T, pool *pgxpool.Pool, id string) (createdAt, updatedAt time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT created_at, updated_at FROM context_blocks WHERE id = $1::uuid`, id,
	).Scan(&createdAt, &updatedAt); err != nil {
		t.Fatalf("read timestamps %s: %v", id, err)
	}
	return createdAt, updatedAt
}

func TestRunGuardBatch_UpdatedAt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Three groups on disjoint axes, so no group is a neighbour of another:
	// a lone block (clean), a pair at cosine 0.96 (both needs_review), and a
	// pair of identical vectors (the older one is auto-archived, the younger
	// one then has no live neighbour and is clean).
	ids := map[string]string{
		"clean":       writeGuardBlock(t, pool, "upd-clean", unitVec1024(0, 1.0)),
		"review-a":    writeGuardBlock(t, pool, "upd-review-a", unitVec1024(100, 1.0)),
		"review-b":    writeGuardBlock(t, pool, "upd-review-b", blendedVec1024(100, 101, 0.96, 0.28)),
		"dup-older":   writeGuardBlock(t, pool, "upd-dup-older", unitVec1024(200, 1.0)),
		"dup-younger": writeGuardBlock(t, pool, "upd-dup-younger", unitVec1024(200, 1.0)),
	}

	before := map[string]time.Time{}
	for name, id := range ids {
		_, before[name] = blockTimes(t, pool, id)
	}

	// The guard runs in its own transaction, so its now() is later than every
	// write above. A short pause keeps the two apart on a coarse clock too.
	time.Sleep(20 * time.Millisecond)

	processed, err := guard.RunGuardBatch(ctx, pool, gitSet(), 10)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != len(ids) {
		t.Fatalf("processed: want %d, got %d", len(ids), processed)
	}

	wantStatus := map[string]string{
		"clean":       "clean",
		"review-a":    "needs_review",
		"review-b":    "needs_review",
		"dup-older":   "archived_dup",
		"dup-younger": "clean",
	}
	for name, id := range ids {
		s := readGuardState(t, pool, id)
		if s.GuardStatus != wantStatus[name] {
			t.Fatalf("%s: guard_status = %q, want %q (the fixture did not reach the branch under test)",
				name, s.GuardStatus, wantStatus[name])
		}
		if !s.MetaCheckedSet {
			t.Errorf("%s: metadata.guard_checked_at not set", name)
		}
		_, after := blockTimes(t, pool, id)

		if name == "dup-older" {
			if !s.IsArchived {
				t.Errorf("%s: not archived", name)
			}
			if !after.After(before[name]) {
				t.Errorf("%s: auto-archive left updated_at at %s; archiving changes what readers see and moves it",
					name, after.Format(time.RFC3339Nano))
			}
			continue
		}
		if s.IsArchived {
			t.Errorf("%s: wrongly archived", name)
		}
		if !after.Equal(before[name]) {
			t.Errorf("%s: the guard pass moved updated_at from %s to %s; recording a %s decision is not a write to the block",
				name, before[name].Format(time.RFC3339Nano), after.Format(time.RFC3339Nano), wantStatus[name])
		}
	}

	// The effect a reader sees: blocks checked in one batch must not collapse
	// onto one shared timestamp. The writes above happened one after another,
	// so their updated_at values are distinct and stay distinct.
	seen := map[time.Time]string{}
	for _, name := range []string{"clean", "review-a", "review-b", "dup-younger"} {
		_, after := blockTimes(t, pool, ids[name])
		if other, dup := seen[after]; dup {
			t.Errorf("%s and %s share updated_at %s after one guard batch", name, other, after.Format(time.RFC3339Nano))
		}
		seen[after] = name
	}
}
