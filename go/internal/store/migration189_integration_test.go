//go:build integration

// Migration 189 (issue #452): context_temporal_reviews is created, the
// unread partial index idx_blocks_dream_temporal_pending is dropped, and the
// stamps of dream_temporal_validated_at are taken over as reviews of the
// blocks' current title and content — the stamp equal to updated_at, or more
// than 15 minutes after it — while a stamp up to 15 minutes after updated_at
// (a write during the review it closed can be the block's last), a stamp
// whose last logged review failed, a stamp earlier than updated_at and no
// stamp leave the block due. Above the inline threshold — by a bounded
// count, not the planner's estimate — nothing is taken over.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestMigration189 -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i452Stamps writes one block per case on schema 188 and gives it the stamp
// of the case (an older binary wrote the column; no write path of this one
// does): name → block id.
func i452Stamps(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := context.Background()
	cases := map[string]string{
		"no stamp":                 `NULL`,
		"stamp = updated_at":       `updated_at`,
		"stamp 5 min after":        `updated_at + interval '5 minutes'`,
		"stamp 1 h after":          `updated_at + interval '1 hour'`,
		"stamp before updated_at":  `updated_at - interval '1 hour'`,
		"last review failed":       `updated_at`,
		"earlier review failed":    `updated_at + interval '1 hour'`,
	}
	ids := map[string]string{}
	i := 0
	for name, stamp := range cases {
		i++
		b, err := store.UpsertBlock(ctx, pool, "i452", fmt.Sprintf("Migration case %d", i), "Case on 2026-03-0"+fmt.Sprint(i)+".",
			nil, nil, "private", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("%s: store: %v", name, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET dream_temporal_validated_at = `+stamp+` WHERE id = $1`, b.ID); err != nil {
			t.Fatalf("%s: stamp: %v", name, err)
		}
		ids[name] = b.ID
	}
	// The LLM log of the older binary's reviews: one dream-temporal row per
	// call, error set when the call or its parse failed.
	for name, calls := range map[string][]*string{
		"stamp = updated_at":    {nil},
		"last review failed":    {nil, i452Str("parse: unexpected end of JSON input")},
		"earlier review failed": {i452Str("i452: backend down"), nil},
	} {
		for i, failed := range calls {
			if _, err := pool.Exec(ctx, `INSERT INTO context_llm_log (pipeline, model, host, error, block_ids, created_at)
				VALUES ('dream-temporal', 'm-i452', 'h-i452', $1, ARRAY[$2::uuid], now() - interval '1 hour' + $3 * interval '1 minute')`,
				failed, ids[name], i); err != nil {
				t.Fatalf("%s: log: %v", name, err)
			}
		}
	}
	return ids
}

func i452Str(s string) *string { return &s }

func i452IndexExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind = 'i'`, name).Scan(&n); err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	return n > 0
}

func TestMigration189TakesOverTheStampsOutsideTheReviewWindow(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 188)
	ctx := context.Background()
	if !i452IndexExists(t, pool, "idx_blocks_dream_temporal_pending") {
		t.Fatalf("schema 188 has no idx_blocks_dream_temporal_pending")
	}
	ids := i452Stamps(t, pool)
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate to 189: %v", err)
	}
	if i452IndexExists(t, pool, "idx_blocks_dream_temporal_pending") {
		t.Errorf("idx_blocks_dream_temporal_pending survived migration 189")
	}
	want := map[string]bool{
		"no stamp":                false,
		"stamp = updated_at":      true,
		"stamp 5 min after":       false,
		"stamp 1 h after":         true,
		"stamp before updated_at": false,
		"last review failed":      false,
		"earlier review failed":   true,
	}
	for name, inherited := range want {
		if due := i452Due(t, pool, ids[name]); due == inherited {
			t.Errorf("%s: due=%v, want taken over=%v", name, due, inherited)
		}
	}

	// The foreign key, added after the take-over, cascades a block's delete.
	var del string
	if err := pool.QueryRow(ctx, `SELECT confdeltype::text FROM pg_constraint
		WHERE conname = 'context_temporal_reviews_block_id_fkey'
		  AND conrelid = 'context_temporal_reviews'::regclass
		  AND confrelid = 'context_blocks'::regclass`).Scan(&del); err != nil || del != "c" {
		t.Errorf("foreign key of context_temporal_reviews: confdeltype=%q err=%v, want an ON DELETE CASCADE key", del, err)
	}

	// Running the file again changes nothing.
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_temporal_reviews`).Scan(&n); err != nil || n != 3 {
		t.Errorf("version rows after a second run: %d (err %v), want 3", n, err)
	}
}

func TestMigration189AboveTheThresholdTakesOverNothing(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 188)
	ctx := context.Background()
	ids := i452Stamps(t, pool)
	// More rows than the inline threshold (100 000), and an estimate that
	// lags far behind them, as between two ANALYZE runs: the file must count.
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope)
		SELECT 'i452', 'Migration bulk ' || g, 'Bulk ' || g, 'private' FROM generate_series(1, 100000) g`); err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE pg_class SET reltuples = 10 WHERE oid = 'context_blocks'::regclass`); err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate to 189: %v", err)
	}
	for name, id := range ids {
		if !i452Due(t, pool, id) {
			t.Errorf("%s: taken over above the threshold", name)
		}
	}
	if i452IndexExists(t, pool, "idx_blocks_dream_temporal_pending") {
		t.Errorf("idx_blocks_dream_temporal_pending survived migration 189")
	}
}
