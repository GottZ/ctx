//go:build integration

package store_test

// Issue #266: the stats numbers come from context_block_counts, maintained by
// statement-level triggers on context_blocks. Gleichheit is the gate: after
// EVERY step of a mixed load over the production write paths the counter
// table equals a GROUP BY over context_blocks, and the four reading functions
// answer exactly what the per-call counts they replaced answer (the old
// statements are kept below as the reference).

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
)

// refStats is GetStats' pre-#266 statement (blocks.go, root 1e99b71c).
func refStats(t *testing.T, pool *pgxpool.Pool, scopes []string) store.Stats {
	t.Helper()
	var s store.Stats
	if err := pool.QueryRow(context.Background(),
		`SELECT
			(SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived),
			(SELECT count(DISTINCT category)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived),
			(SELECT min(created_at) FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived),
			(SELECT max(created_at) FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived),
			(SELECT pg_size_pretty(pg_total_relation_size('context_blocks')))`, scopes,
	).Scan(&s.TotalBlocks, &s.TotalCategories, &s.OldestEntry, &s.NewestEntry, &s.DBSize); err != nil {
		t.Fatalf("ref stats: %v", err)
	}
	return s
}

// refGuardStats is GetGuardStats' pre-#266 statement.
func refGuardStats(t *testing.T, pool *pgxpool.Pool, scopes []string) store.GuardStats {
	t.Helper()
	var gs store.GuardStats
	if err := pool.QueryRow(context.Background(),
		`SELECT
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status = 'active'), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status = 'clean'), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status = 'needs_review'), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status = 'near_duplicate'), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status IS NULL), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND guard_status = 'archived_dup'), 0),
			COALESCE((SELECT count(*)::int FROM context_write_log), 0),
			(SELECT dirty_since FROM context_guard_state WHERE id = true),
			(SELECT last_guard_at FROM context_guard_state WHERE id = true),
			COALESCE((SELECT pending_count FROM context_guard_state WHERE id = true), 0)`, scopes,
	).Scan(&gs.TotalBlocks, &gs.Active, &gs.Clean, &gs.NeedsReview, &gs.NearDuplicate,
		&gs.Unchecked, &gs.ArchivedDups, &gs.WriteLogEntries,
		&gs.DirtySince, &gs.LastGuardAt, &gs.PendingCount); err != nil {
		t.Fatalf("ref guard stats: %v", err)
	}
	return gs
}

// refCategories is ListCategories' pre-#266 statement, as a map (the order of
// equal counts was never defined).
func refCategories(t *testing.T, pool *pgxpool.Pool, scopes []string) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT category, count(*)::int FROM context_blocks
		  WHERE scope = ANY($1::text[]) AND NOT is_archived GROUP BY category`, scopes)
	if err != nil {
		t.Fatalf("ref categories: %v", err)
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			t.Fatalf("ref categories scan: %v", err)
		}
		m[c] = n
	}
	return m
}

// counterRows renders a key → n listing; truth=true counts context_blocks,
// truth=false reads the counter table (zero rows are no information).
func counterRows(t *testing.T, pool *pgxpool.Pool, truth bool) []string {
	t.Helper()
	q := `SELECT scope, category, is_archived, COALESCE(guard_status, '<null>'), sum(n)::bigint
	        FROM (SELECT scope, category, is_archived, guard_status, n FROM context_block_counts
	              UNION ALL
	              SELECT scope, category, is_archived, guard_status, n FROM context_block_count_deltas) k
	       GROUP BY 1, 2, 3, 4 HAVING sum(n) <> 0 ORDER BY 1, 2, 3, 4`
	if truth {
		q = `SELECT scope::text, category::text, is_archived, COALESCE(guard_status::text, '<null>'), count(*)
		       FROM context_blocks GROUP BY 1, 2, 3, 4 ORDER BY 1, 2, 3, 4`
	}
	rows, err := pool.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("counter rows (truth=%v): %v", truth, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s, c, g string
		var a bool
		var n int64
		if err := rows.Scan(&s, &c, &a, &g, &n); err != nil {
			t.Fatalf("counter rows scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%v|%s=%d", s, c, a, g, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("counter rows: %v", err)
	}
	return out
}

// assertCountParity is the gate of one step.
func assertCountParity(t *testing.T, pool *pgxpool.Pool, step string, windows [][]string) {
	t.Helper()
	ctx := context.Background()
	if got, want := counterRows(t, pool, false), counterRows(t, pool, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: counter table drifted\n got: %v\nwant: %v", step, got, want)
	}
	for _, w := range windows {
		ref := refStats(t, pool, w)
		got, err := store.GetStats(ctx, pool, w)
		if err != nil {
			t.Fatalf("%s %v: GetStats: %v", step, w, err)
		}
		if got.TotalBlocks != ref.TotalBlocks || got.TotalCategories != ref.TotalCategories ||
			!reflect.DeepEqual(got.OldestEntry, ref.OldestEntry) || !reflect.DeepEqual(got.NewestEntry, ref.NewestEntry) ||
			got.DBSize != ref.DBSize {
			t.Fatalf("%s %v: GetStats = %+v, want %+v", step, w, *got, ref)
		}
		refG := refGuardStats(t, pool, w)
		gotG, err := store.GetGuardStats(ctx, pool, w)
		if err != nil {
			t.Fatalf("%s %v: GetGuardStats: %v", step, w, err)
		}
		if !reflect.DeepEqual(*gotG, refG) {
			t.Fatalf("%s %v: GetGuardStats = %+v, want %+v", step, w, *gotG, refG)
		}
		refC := refCategories(t, pool, w)
		gotC, err := store.ListCategories(ctx, pool, w)
		if err != nil {
			t.Fatalf("%s %v: ListCategories: %v", step, w, err)
		}
		m := map[string]int{}
		for i, c := range gotC {
			m[c.Category] = c.Count
			if i > 0 && gotC[i-1].Count < c.Count {
				t.Fatalf("%s %v: ListCategories not count-desc: %+v", step, w, gotC)
			}
		}
		if len(m) != len(gotC) || !reflect.DeepEqual(m, refC) {
			t.Fatalf("%s %v: ListCategories = %+v, want %v", step, w, gotC, refC)
		}
		n, ok, err := store.ActiveCategoryCount(ctx, pool, w, 5*time.Second)
		if err != nil || !ok || n != ref.TotalCategories {
			t.Fatalf("%s %v: ActiveCategoryCount = (%d, %v, %v), want %d", step, w, n, ok, err, ref.TotalCategories)
		}
		n, ok, err = store.ActiveBlockCount(ctx, pool, w, 5*time.Second)
		if err != nil || !ok || n != ref.TotalBlocks {
			t.Fatalf("%s %v: ActiveBlockCount = (%d, %v, %v), want %d", step, w, n, ok, err, ref.TotalBlocks)
		}
	}
}

// unit returns a 1024-dim vector with cos(v, e0) = c in the (e0, e_axis) plane.
func unit(axis int, c float64) []float32 {
	v := make([]float32, 1024)
	v[0] = float32(c)
	v[axis] = float32(math.Sqrt(1 - c*c))
	return v
}

func TestBlockCounts_ParityOverWritePaths(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	sA := f.scope(t, "cnt-a")
	sB := f.scope(t, "cnt-b")
	sC := f.scope(t, "cnt-c")
	all := []string{sA, sB, sC, spBystander}
	windows := [][]string{{sA}, {sB}, {sA, sB}, all, {"cnt-nowhere"}, {sA, sA}}
	// Every step is checked twice: with the delta rows of the step still
	// pending, and after a fold moved them into the counter rows.
	check := func(step string) {
		t.Helper()
		assertCountParity(t, pool, step, windows)
		if _, err := store.FoldBlockCountDeltas(ctx, pool); err != nil {
			t.Fatalf("%s: fold: %v", step, err)
		}
		var pending int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_block_count_deltas`).Scan(&pending); err != nil || pending != 0 {
			t.Fatalf("%s: %d delta rows left after the fold (%v)", step, pending, err)
		}
		assertCountParity(t, pool, step+" (folded)", windows)
	}
	up := func(scope, cat, title, content string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, cat, title, content, nil, nil, scope, true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("upsert %s/%s: %v", scope, title, err)
		}
		return b.ID
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	check("empty")

	// Store: inserts across scopes and categories.
	ids := map[string]string{}
	for i, c := range []struct{ scope, cat string }{
		{sA, "alpha"}, {sA, "alpha"}, {sA, "alpha"}, {sA, "beta"}, {sA, "beta"}, {sA, "gamma"},
		{sB, "alpha"}, {sB, "delta"}, {spBystander, "alpha"},
	} {
		title := fmt.Sprintf("t%d", i)
		ids[title] = up(c.scope, c.cat, title, "body "+title)
	}
	check("store")

	// Store on an existing (category, title, scope): the upsert update path.
	up(sA, "alpha", "t0", "body t0 changed")
	check("store-update")

	// Update: category change, scope change, content-only change.
	beta, toB, body := "beta", sB, "new body"
	writes := []string{sA, sB, sC}
	if _, _, err := store.UpdateBlock(ctx, pool, ids["t1"], store.UpdateBlockData{Category: &beta}, writes); err != nil {
		t.Fatalf("update category: %v", err)
	}
	check("update-category")
	if _, _, err := store.UpdateBlock(ctx, pool, ids["t5"], store.UpdateBlockData{Scope: &toB}, writes); err != nil {
		t.Fatalf("update scope: %v", err)
	}
	check("update-scope")
	if _, _, err := store.UpdateBlock(ctx, pool, ids["t2"], store.UpdateBlockData{Content: &body}, writes); err != nil {
		t.Fatalf("update content: %v", err)
	}
	check("update-content")

	// Archive (manage delete) and an unarchive by SQL (no store verb exists).
	if _, err := store.DeleteBlock(ctx, pool, ids["t3"], writes); err != nil {
		t.Fatalf("archive: %v", err)
	}
	check("archive")
	exec(`UPDATE context_blocks SET is_archived = false WHERE id = $1::uuid`, ids["t3"])
	check("unarchive")
	exec(`UPDATE context_blocks SET guard_status = NULL WHERE id = $1::uuid`, ids["t4"])
	check("guard-status-null")

	// Guard: an identical pair (near_duplicate → auto-archive as archived_dup),
	// a close pair (needs_review) and an orthogonal block (clean).
	set := blocktype.NewRegistry().Snapshot()
	emb := func(id string, v []float32) {
		t.Helper()
		if err := store.StoreEmbedding(ctx, pool, id, "test-embed", v); err != nil {
			t.Fatalf("embedding: %v", err)
		}
	}
	g1 := up(sC, "guard", "dup-a", "duplicate body one")
	g2 := up(sC, "guard", "dup-b", "duplicate body two")
	g3 := up(sC, "guard", "close-a", "close body one")
	g4 := up(sC, "guard", "close-b", "close body two")
	g5 := up(sC, "guard", "far", "far away body")
	emb(g1, unit(1, 1))
	emb(g2, unit(1, 1))
	emb(g3, unit(2, 0.5))
	emb(g4, unit(3, 0.5))
	g4v := unit(2, 0.5)
	g4v[3] = 0.25
	emb(g4, g4v)
	emb(g5, unit(500, 0))
	if _, err := guard.RunGuardBatch(ctx, pool, set, 50); err != nil {
		t.Fatalf("guard batch: %v", err)
	}
	var statuses []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(COALESCE(guard_status, '<null>') || ':' || is_archived ORDER BY title)
	                                FROM context_blocks WHERE scope = $1`, sC).Scan(&statuses); err != nil {
		t.Fatal(err)
	}
	t.Logf("guard statuses in %s (by title): %v", sC, statuses)
	check("guard-batch")

	// Guard resolve: keep and archive.
	if _, err := store.GuardResolve(ctx, pool, g3, "keep", writes); err != nil {
		t.Fatalf("guard resolve keep: %v", err)
	}
	check("guard-resolve-keep")
	if _, err := store.GuardResolve(ctx, pool, g5, "archive", writes); err != nil {
		t.Fatalf("guard resolve archive: %v", err)
	}
	check("guard-resolve-archive")

	// Ingest: chunks of one source, then a changed re-ingest of a slot.
	var src string
	if err := pool.QueryRow(ctx, `INSERT INTO context_sources (file_path, file_hash, scope)
	                               VALUES ('cnt.md', 'cnt', $1) RETURNING id::text`, sA).Scan(&src); err != nil {
		t.Fatalf("source: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.InsertChunk(ctx, pool, src, i, "ingest", fmt.Sprintf("cnt.md #%d", i),
			fmt.Sprintf("chunk %d", i), nil, nil, sA); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	check("ingest")
	if _, err := store.InsertChunk(ctx, pool, src, 1, "ingest", "cnt.md #1 renamed", "chunk 1 changed", nil, nil, sA); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	check("re-ingest")

	// Bulk statements: one INSERT of 500 rows, one UPDATE moving 100 of them
	// to another scope and category, one UPDATE that changes no key.
	exec(`INSERT INTO context_blocks (category, title, content, scope, guard_status)
	      SELECT CASE WHEN g % 3 = 0 THEN 'bulk-x' ELSE 'bulk-y' END, 'bulk ' || g, 'bulk body ' || g, $1,
	             CASE WHEN g % 5 = 0 THEN NULL WHEN g % 7 = 0 THEN 'clean' ELSE 'active' END
	        FROM generate_series(1, 500) g`, sC)
	check("bulk-insert")
	exec(`UPDATE context_blocks SET scope = $2, category = 'bulk-moved'
	       WHERE id IN (SELECT id FROM context_blocks WHERE scope = $1 AND category LIKE 'bulk-%' ORDER BY title LIMIT 100)`, sC, sA)
	check("bulk-move")
	// An update that changes no key writes no delta row (HAVING sum <> 0).
	var pendingBefore, pendingAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_block_count_deltas`).Scan(&pendingBefore); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE context_blocks SET metadata = metadata || '{"cnt": 1}'::jsonb WHERE scope = $1`, sC)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_block_count_deltas`).Scan(&pendingAfter); err != nil {
		t.Fatal(err)
	}
	if pendingAfter != pendingBefore {
		t.Fatalf("an update without a key change wrote %d delta rows", pendingAfter-pendingBefore)
	}
	check("bulk-no-key-change")

	// Parent cascade: deleting a parent removes its child through
	// fk_blocks_parent ON DELETE CASCADE (an RI statement, not ours).
	parent := up(sB, "tree", "parent", "parent body")
	child := up(sB, "tree", "child", "child body")
	exec(`UPDATE context_blocks SET parent_id = $1::uuid WHERE id = $2::uuid`, parent, child)
	exec(`DELETE FROM context_blocks WHERE id = $1::uuid`, parent)
	check("cascade-delete")

	// Purge of a scope (the manage purge path, batched deletes).
	q := f.admin()
	q.Scope = sC
	dry, err := store.PurgeScope(ctx, pool, q)
	if err != nil {
		t.Fatalf("purge dry: %v", err)
	}
	q.ConfirmBlocks = confirm(dry.Counted.Blocks)
	if _, err := store.PurgeScope(ctx, pool, q); err != nil {
		t.Fatalf("purge: %v", err)
	}
	check("purge")

	// Tenant delete: the prune drains every scope of tenant A.
	if err := store.PruneTenant(ctx, pool, f.tenantA, store.APIStamp(f.adminKeyID, "req-cnt")); err != nil {
		t.Fatalf("prune tenant: %v", err)
	}
	check("tenant-delete")

	// TRUNCATE has its own trigger; it clears pending deltas too.
	up(spBystander, "alpha", "pre-truncate", "pending delta")
	exec(`TRUNCATE context_blocks CASCADE`)
	check("truncate")
	var rowsLeft int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_block_counts) + (SELECT count(*) FROM context_block_count_deltas)`).Scan(&rowsLeft); err != nil || rowsLeft != 0 {
		t.Fatalf("counter and delta rows after TRUNCATE = %d (%v), want 0", rowsLeft, err)
	}
}
