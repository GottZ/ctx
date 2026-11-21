//go:build integration

// Issue #427: MarkTemporalReviewed records the dream's temporal review only
// for the title and content it read, as the block's updated_at, and reaches
// the row by its primary key alone.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestMarkTemporalReviewed|TestTemporalReview' -count=1 -v
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

// i427Stamp reads the block's stamp and updated_at.
func i427Stamp(t *testing.T, pool *pgxpool.Pool, id string) (*time.Time, time.Time) {
	t.Helper()
	var stamp *time.Time
	var updated time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT dream_temporal_validated_at, updated_at FROM context_blocks WHERE id = $1`, id).
		Scan(&stamp, &updated); err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	return stamp, updated
}

func TestMarkTemporalReviewedOnlyForTheReviewedVersion(t *testing.T) {
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

	for _, tc := range []struct{ name, title, content string }{
		{"other title", "Retro notes (old)", content},
		{"other content", title, "Retro on 2026-02-03."},
	} {
		stamped, err := store.MarkTemporalReviewed(ctx, pool, b.ID, tc.title, tc.content)
		if err != nil || stamped {
			t.Errorf("%s: stamped=%v err=%v, want false", tc.name, stamped, err)
		}
		if stamp, _ := i427Stamp(t, pool, b.ID); stamp != nil {
			t.Errorf("%s: the stamp was written: %v", tc.name, *stamp)
		}
	}

	stamped, err := store.MarkTemporalReviewed(ctx, pool, b.ID, title, content)
	if err != nil || !stamped {
		t.Fatalf("reviewed version: stamped=%v err=%v, want true", stamped, err)
	}
	stamp, updated := i427Stamp(t, pool, b.ID)
	if stamp == nil || !stamp.Equal(updated) {
		t.Fatalf("stamp = %v, want the block's updated_at %v", stamp, updated)
	}

	// A later version leaves the earlier stamp as it is.
	newer := "Retro moved to 2026-02-17."
	if u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &newer}, []string{"private"}); err != nil || u == nil {
		t.Fatalf("update: %v", err)
	}
	if stamped, err := store.MarkTemporalReviewed(ctx, pool, b.ID, title, content); err != nil || stamped {
		t.Errorf("stale version: stamped=%v err=%v, want false", stamped, err)
	}
	if after, updatedAfter := i427Stamp(t, pool, b.ID); after == nil || !after.Equal(*stamp) || !updatedAfter.After(*after) {
		t.Errorf("stamp after the stale mark = %v (updated_at %v), want %v and the block due", after, updatedAfter, *stamp)
	}

	// A block that is gone answers false, without an error.
	if stamped, err := store.MarkTemporalReviewed(ctx, pool, "019d0000-0000-7000-9000-000000042700", title, content); err != nil || stamped {
		t.Errorf("gone block: stamped=%v err=%v, want false and no error", stamped, err)
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

func TestTemporalReviewStampPlanUsesPrimaryKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	i427PlanCorpus(t, pool)
	text, conds := i427GenericPlan(t, pool, store.TemporalReviewStampSQLForTest)
	t.Logf("generic plan:\n%s", text)
	if !strings.Contains(text, "Index Scan using context_blocks_pkey on context_blocks") ||
		!slices.Equal(conds, []string{"Index Cond: (id = $1)"}) {
		t.Errorf("the temporal review stamp must reach the row by its primary key only, plan:\n%s", text)
	}
}
