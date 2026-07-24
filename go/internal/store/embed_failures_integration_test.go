//go:build integration

// Integration test for the Evokoa-Clean-Room-Plan Achse 04 W04-2 (Re-Embed-
// Migration "Fehler-Memo") — migration 113 (context_embed_failures) plus the
// store-level upsert primitive (RecordEmbedFailure) and the shared pending-
// exclusion predicate (EmbedFailureExcludedPredicate). The scheduler/query
// integration of these primitives (both pick paths + the actual Vorfall-
// 2026-07-10 head-of-line repro) is covered by
// internal/events/backfill_headofline_integration_test.go and
// internal/handler/backfill_synccap_integration_test.go — this file pins the
// DB-level contract those two build on:
//   - the migration's schema (table, two partial-unique indexes, backoff index);
//   - RecordEmbedFailure upserts (attempts++, last_error/last_class replaced,
//     next_attempt_at recomputed) rather than duplicating rows;
//   - the exponential backoff curve (base * 2^(attempts-1), capped);
//   - the oversize class short-circuits straight to 'infinity' regardless of
//     attempts;
//   - EmbedFailureExcludedPredicate actually excludes a backed-off block and
//     stops excluding it once next_attempt_at lapses.
//
// Run: go test -tags=integration ./internal/store/ -run TestEmbedFailures -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestEmbedFailures_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedBlock := func(t *testing.T, title string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('issue', $1, 'body', 'w04-2') RETURNING id::text`, title).Scan(&id); err != nil {
			t.Fatalf("seed block %s: %v", title, err)
		}
		return id
	}

	// (1) Schema: table + three indexes from migration 113 exist.
	t.Run("schema_objects_present", func(t *testing.T) {
		var tableCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables WHERE table_name = 'context_embed_failures'`,
		).Scan(&tableCount); err != nil {
			t.Fatalf("table probe: %v", err)
		}
		if tableCount != 1 {
			t.Fatalf("context_embed_failures table missing")
		}
		for _, idx := range []string{
			"idx_embed_failures_backfill",
			"idx_embed_failures_migration",
			"idx_embed_failures_next_attempt",
		} {
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM pg_indexes WHERE tablename = 'context_embed_failures' AND indexname = $1`,
				idx).Scan(&n); err != nil {
				t.Fatalf("index probe %s: %v", idx, err)
			}
			if n != 1 {
				t.Errorf("index %s missing (want exactly 1, got %d)", idx, n)
			}
		}
	})

	// (2) Upsert semantics: a second failure on the SAME block increments
	// attempts and replaces last_error/last_class instead of inserting a
	// second row (the partial-unique index idx_embed_failures_backfill is
	// what makes this an upsert rather than a constraint violation).
	t.Run("upsert_increments_attempts", func(t *testing.T) {
		id := seedBlock(t, "w04-2-upsert")
		base := time.Minute
		backoffCap := time.Hour

		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureWire, "wire: first failure", base, backoffCap); err != nil {
			t.Fatalf("first RecordEmbedFailure: %v", err)
		}
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureWire, "wire: second failure", base, backoffCap); err != nil {
			t.Fatalf("second RecordEmbedFailure: %v", err)
		}

		var rowCount, attempts int
		var lastError, lastClass string
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_failures WHERE block_id = $1::uuid`, id).Scan(&rowCount); err != nil {
			t.Fatalf("row count: %v", err)
		}
		if rowCount != 1 {
			t.Fatalf("row count = %d, want 1 (upsert, not insert-per-failure)", rowCount)
		}
		if err := pool.QueryRow(ctx,
			`SELECT attempts, last_error, last_class FROM context_embed_failures WHERE block_id = $1::uuid`, id).
			Scan(&attempts, &lastError, &lastClass); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if attempts != 2 {
			t.Errorf("attempts = %d, want 2", attempts)
		}
		if lastError != "wire: second failure" {
			t.Errorf("last_error = %q, want the SECOND failure's message", lastError)
		}
		if lastClass != "wire" {
			t.Errorf("last_class = %q, want %q", lastClass, "wire")
		}
	})

	// (3) Exponential backoff: base*2^(attempts-1), capped. Three failures
	// with base=1s produce next_attempt_at deltas of roughly 1s, 2s, 4s —
	// asserted as monotonically increasing (not exact, to stay robust
	// against DB round-trip jitter) plus an explicit cap probe.
	t.Run("exponential_backoff_capped", func(t *testing.T) {
		id := seedBlock(t, "w04-2-backoff")
		base := time.Second
		backoffCap := 3 * time.Second // small cap so attempt 3 (nominal 4s) clamps

		readDelta := func() time.Duration {
			var secs float64
			if err := pool.QueryRow(ctx,
				`SELECT extract(epoch FROM (next_attempt_at - now())) FROM context_embed_failures WHERE block_id = $1::uuid`, id).
				Scan(&secs); err != nil {
				t.Fatalf("read next_attempt_at delta: %v", err)
			}
			return time.Duration(secs * float64(time.Second))
		}

		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureWire, "wire: attempt 1", base, backoffCap); err != nil {
			t.Fatalf("attempt 1: %v", err)
		}
		d1 := readDelta()
		if d1 < 500*time.Millisecond || d1 > 1500*time.Millisecond {
			t.Errorf("attempt 1 delta = %v, want ~1s (base*2^0)", d1)
		}

		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureWire, "wire: attempt 2", base, backoffCap); err != nil {
			t.Fatalf("attempt 2: %v", err)
		}
		d2 := readDelta()
		if d2 < 1500*time.Millisecond || d2 > 2500*time.Millisecond {
			t.Errorf("attempt 2 delta = %v, want ~2s (base*2^1)", d2)
		}

		// Attempt 3 would nominally be base*2^2=4s, above the 3s cap.
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureWire, "wire: attempt 3", base, backoffCap); err != nil {
			t.Fatalf("attempt 3: %v", err)
		}
		d3 := readDelta()
		if d3 < 2500*time.Millisecond || d3 > 3500*time.Millisecond {
			t.Errorf("attempt 3 delta = %v, want ~3s (capped, not ~4s)", d3)
		}
	})

	// (4) Oversize short-circuits to 'infinity' regardless of attempts —
	// never a blind 24h-in-slow-motion retry.
	t.Run("oversize_is_infinity", func(t *testing.T) {
		id := seedBlock(t, "w04-2-oversize")
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureOversize, "oversize: too big", time.Minute, time.Hour); err != nil {
			t.Fatalf("RecordEmbedFailure: %v", err)
		}
		var isInf bool
		if err := pool.QueryRow(ctx,
			`SELECT next_attempt_at = 'infinity' FROM context_embed_failures WHERE block_id = $1::uuid`, id).
			Scan(&isInf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if !isInf {
			t.Errorf("next_attempt_at is not infinity for an oversize memo")
		}
	})

	// (5) EmbedFailureExcludedPredicate: a block with an outstanding
	// backoff is excluded from the pending-pick shape; a block whose
	// backoff already lapsed is NOT excluded (the memo row alone doesn't
	// block forever — only a FUTURE next_attempt_at does).
	t.Run("predicate_excludes_only_while_backed_off", func(t *testing.T) {
		backedOff := seedBlock(t, "w04-2-pred-backed-off")
		lapsed := seedBlock(t, "w04-2-pred-lapsed")
		clean := seedBlock(t, "w04-2-pred-clean")

		if err := store.RecordEmbedFailure(ctx, pool, backedOff, store.EmbedFailureWire, "wire: still backed off", time.Hour, 24*time.Hour); err != nil {
			t.Fatalf("seed backed-off memo: %v", err)
		}
		// Simulate a memo whose backoff already lapsed (a block that will
		// legitimately be retried on the very next pick).
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
			 VALUES ($1, NULL, 1, 'wire: lapsed', 'wire', now() - interval '1 hour')`, lapsed); err != nil {
			t.Fatalf("seed lapsed memo: %v", err)
		}

		rows, err := pool.Query(ctx,
			`SELECT id::text FROM context_blocks
			 WHERE embedding IS NULL AND NOT is_archived`+store.EmbedFailureExcludedPredicate+`
			   AND id::text = ANY($1)
			 ORDER BY created_at ASC`,
			[]string{backedOff, lapsed, clean})
		if err != nil {
			t.Fatalf("predicate query: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}

		gotSet := map[string]bool{}
		for _, id := range got {
			gotSet[id] = true
		}
		if gotSet[backedOff] {
			t.Errorf("backed-off block was NOT excluded (got %v)", got)
		}
		if !gotSet[lapsed] {
			t.Errorf("lapsed-backoff block was excluded, want visible again (got %v)", got)
		}
		if !gotSet[clean] {
			t.Errorf("clean pending block was excluded, want visible (got %v)", got)
		}
	})
}
