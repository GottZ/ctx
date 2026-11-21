//go:build integration

// Migration 191 (issue #399): ctx_migration_rows(regclass, bigint) is the
// size a migration with a row threshold compares against it, instead of the
// copied pg_class.reltuples query of 115, 140 and 171-187. The function
// estimates the largest of the stored estimate, the stored density applied to
// the current size of the heap and n_live_tup, summed over every partition,
// inheritance child and TimescaleDB chunk, and lets the estimate decide only
// "out of band": an estimate at or above the threshold is the answer; below
// it, and where no estimate can be trusted (never analysed or truncated,
// reltuples 0, fewer than 32 pages, no VACUUM or ANALYZE on record in the
// cumulative statistics), it counts, stopping at the threshold.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestMigration191 -count=1 -v
//
// The measurement of the issue at full scale (not a gate; about 1 GB of
// heap in the test container):
//
//	CTX_M191_MEASURE=1 go test -tags=integration ./internal/store/ -run TestMigration191MeasureIssue399 -count=1 -v -timeout 30m
package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m191File = "191_migration_rows.sql"

func m191Exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// m191Rows is the size the function returns for rel at threshold.
func m191Rows(t *testing.T, pool *pgxpool.Pool, rel string, threshold int64) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT ctx_migration_rows($1::regclass, $2)`, rel, threshold).Scan(&n); err != nil {
		t.Fatalf("ctx_migration_rows(%s, %d): %v", rel, threshold, err)
	}
	return n
}

// m191Old is the size decision 171-187 copy: pg_class.reltuples, and only
// for a table never analysed (reltuples < 0) a count stopped at the
// threshold.
func m191Old(t *testing.T, pool *pgxpool.Pool, rel string, threshold int64) int64 {
	t.Helper()
	ctx := context.Background()
	var est float64
	if err := pool.QueryRow(ctx, `SELECT reltuples FROM pg_class WHERE oid = $1::regclass`, rel).Scan(&est); err != nil {
		t.Fatal(err)
	}
	if est >= 0 {
		return int64(est)
	}
	var n int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 FROM %s LIMIT %d) t`, rel, threshold)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// m191Count is the exact row count of rel (children included).
func m191Count(t *testing.T, pool *pgxpool.Pool, rel string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+rel).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// m191Stats logs what the catalog stores for rel and returns reltuples and
// relpages.
func m191Stats(t *testing.T, pool *pgxpool.Pool, label, rel string) (float64, int32) {
	t.Helper()
	var rt float64
	var rp int32
	var size int64
	if err := pool.QueryRow(context.Background(), `SELECT reltuples, relpages, pg_relation_size(oid) FROM pg_class WHERE oid = $1::regclass`, rel).Scan(&rt, &rp, &size); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s reltuples=%.0f relpages=%d size_pages=%d", label, rel, rt, rp, size/8192)
	return rt, rp
}

// m191Within fails unless got lies in [lo*want, hi*want].
func m191Within(t *testing.T, label string, got, want int64, lo, hi float64) {
	t.Helper()
	if float64(got) < lo*float64(want) || float64(got) > hi*float64(want) {
		t.Errorf("%s: ctx_migration_rows = %d, want within [%.2f, %.2f] x %d", label, got, lo, hi, want)
	}
}

// m191Table creates a table of rows of about 160 bytes with autovacuum off
// (the issue's setting: no ANALYZE runs unless the test says so).
func m191Table(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	m191Exec(t, pool, `CREATE TABLE `+name+` (id bigint, pad text) WITH (autovacuum_enabled = false)`)
}

func m191Fill(t *testing.T, pool *pgxpool.Pool, name string, from, to int) {
	t.Helper()
	m191ExecFlushed(t, pool, `INSERT INTO `+name+` SELECT g, repeat('x', 120) || g FROM generate_series($1::int, $2::int) g`, from, to)
}

// m191ExecFlushed runs sql on one connection and has that backend flush its
// pending cumulative statistics before the call returns (the flush runs when
// the backend goes idle, before it reports ready), so n_live_tup holds the
// write when the next statement reads it.
func m191ExecFlushed(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if _, err := c.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}
}

// m191Live is n_live_tup of rel, the cumulative statistics' live-row count.
func m191Live(t *testing.T, pool *pgxpool.Pool, rel string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT pg_stat_get_live_tuples($1::regclass)`, rel).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// m191ResetCounters drops the cumulative statistics of rel, as crash
// recovery drops them for every table: n_live_tup reads 0 until the next
// write, VACUUM or ANALYZE.
func m191ResetCounters(t *testing.T, pool *pgxpool.Pool, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		m191Exec(t, pool, `SELECT pg_stat_reset_single_table_counters($1::regclass)`, rel)
		if n := m191Live(t, pool, rel); n != 0 {
			t.Fatalf("%s: n_live_tup %d after the reset, want 0", rel, n)
		}
	}
}

// m191SQLState runs sql and returns the SQLSTATE it fails with ("" on
// success).
func m191SQLState(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) (string, string) {
	t.Helper()
	var n *int64
	err := pool.QueryRow(context.Background(), sql, args...).Scan(&n)
	if err == nil {
		return "", ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.Message
	}
	t.Fatalf("%s: %v", sql, err)
	return "", ""
}

func TestMigration191RowsFunction(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)

	t.Run("never analysed: a count stopped at the threshold", func(t *testing.T) {
		m191Table(t, pool, "m191_never")
		m191Fill(t, pool, "m191_never", 1, 1500)
		if rt, _ := m191Stats(t, pool, "never analysed", "m191_never"); rt >= 0 {
			t.Fatalf("reltuples = %.0f, want -1 (never analysed)", rt)
		}
		if got := m191Rows(t, pool, "m191_never", 1000); got != 1000 {
			t.Errorf("threshold 1000: got %d, want 1000 (the count stops at the threshold)", got)
		}
		if got := m191Rows(t, pool, "m191_never", 2000); got != 1500 {
			t.Errorf("threshold 2000: got %d, want 1500 (every row counted below the threshold)", got)
		}
	})

	t.Run("exactly at the threshold", func(t *testing.T) {
		m191Table(t, pool, "m191_edge")
		m191Fill(t, pool, "m191_edge", 1, 1000)
		if got := m191Rows(t, pool, "m191_edge", 1000); got != 1000 {
			t.Errorf("1000 rows at threshold 1000: got %d, want 1000 (at the threshold: out of band)", got)
		}
		if got := m191Rows(t, pool, "m191_edge", 1001); got != 1000 {
			t.Errorf("1000 rows at threshold 1001: got %d, want 1000 (below: inline)", got)
		}
	})

	t.Run("empty table", func(t *testing.T) {
		m191Table(t, pool, "m191_empty")
		if got := m191Rows(t, pool, "m191_empty", 1000); got != 0 {
			t.Errorf("never analysed: got %d, want 0", got)
		}
		m191Exec(t, pool, `ANALYZE m191_empty`)
		m191Stats(t, pool, "analysed empty", "m191_empty")
		if got := m191Rows(t, pool, "m191_empty", 1000); got != 0 {
			t.Errorf("analysed: got %d, want 0", got)
		}
	})

	t.Run("analysed empty, filled since (relpages 0)", func(t *testing.T) {
		m191Table(t, pool, "m191_zero_pages")
		m191Exec(t, pool, `ANALYZE m191_zero_pages`)
		m191Fill(t, pool, "m191_zero_pages", 1, 1500)
		rt, rp := m191Stats(t, pool, "analysed empty, filled", "m191_zero_pages")
		if rt != 0 || rp != 0 {
			t.Fatalf("reltuples %.0f relpages %d, want 0 and 0", rt, rp)
		}
		if old := m191Old(t, pool, "m191_zero_pages", 1000); old != 0 {
			t.Errorf("the copied query: got %d, want 0 (it builds inline)", old)
		}
		if got := m191Rows(t, pool, "m191_zero_pages", 1000); got != 1000 {
			t.Errorf("got %d, want 1000 (no stored density: a count)", got)
		}
	})

	t.Run("an estimate without pages (restored statistics on a heap filled since)", func(t *testing.T) {
		m191Table(t, pool, "m191_nopages")
		m191Fill(t, pool, "m191_nopages", 1, 1500)
		m191Exec(t, pool, `ANALYZE m191_nopages`)
		m191Exec(t, pool, `UPDATE pg_class SET reltuples = 1500, relpages = 0 WHERE oid = 'm191_nopages'::regclass`)
		if got := m191Rows(t, pool, "m191_nopages", 1000); got != 1000 {
			t.Errorf("got %d, want 1000 (no pages to read a density from: a count)", got)
		}
	})

	t.Run("analysed with every row deleted, filled since (reltuples 0)", func(t *testing.T) {
		m191Table(t, pool, "m191_dead")
		m191Fill(t, pool, "m191_dead", 1, 2000)
		m191ExecFlushed(t, pool, `DELETE FROM m191_dead`)
		m191Exec(t, pool, `ANALYZE m191_dead`)
		m191Fill(t, pool, "m191_dead", 1, 3000)
		rt, rp := m191Stats(t, pool, "deleted, analysed, filled", "m191_dead")
		if rt != 0 || rp == 0 {
			t.Fatalf("reltuples %.0f relpages %d, want 0 and more than 0", rt, rp)
		}
		if old := m191Old(t, pool, "m191_dead", 1000); old != 0 {
			t.Errorf("the copied query: got %d, want 0 (it builds inline)", old)
		}
		if got := m191Rows(t, pool, "m191_dead", 1000); got != 1000 {
			t.Errorf("got %d, want 1000 (a stored density of 0 rows per page: a count)", got)
		}
	})

	t.Run("grown since ANALYZE", func(t *testing.T) {
		m191Table(t, pool, "m191_grown")
		m191Fill(t, pool, "m191_grown", 1, 2000)
		m191Exec(t, pool, `ANALYZE m191_grown`)
		m191Fill(t, pool, "m191_grown", 2001, 60000)
		rt, _ := m191Stats(t, pool, "analysed at 2000, grown to 60000", "m191_grown")
		if rt != 2000 {
			t.Fatalf("reltuples %.0f, want 2000", rt)
		}
		if old := m191Old(t, pool, "m191_grown", 10000); old != 2000 {
			t.Errorf("the copied query: got %d, want 2000", old)
		}
		got := m191Rows(t, pool, "m191_grown", 10000)
		t.Logf("ctx_migration_rows = %d for 60000 rows (n_live_tup %d)", got, m191Live(t, pool, "m191_grown"))
		if got != 60000 {
			t.Errorf("counters intact: got %d, want 60000 (n_live_tup)", got)
		}
		// Crash recovery drops the cumulative statistics, the record of the
		// ANALYZE with them: a count.
		m191ResetCounters(t, pool, "m191_grown")
		if got := m191Rows(t, pool, "m191_grown", 10000); got != 10000 {
			t.Errorf("counters dropped: got %d, want 10000 (a count stopped at the threshold)", got)
		}
	})

	t.Run("refilled into the space a VACUUM freed: the live-row counter", func(t *testing.T) {
		// The heap does not grow, so neither the estimate nor the density
		// times the size can see the rows; the cumulative statistics count
		// every insert since the VACUUM.
		m191Table(t, pool, "m191_refill")
		m191Fill(t, pool, "m191_refill", 1, 20000)
		m191ExecFlushed(t, pool, `DELETE FROM m191_refill WHERE id % 10 <> 0`)
		m191ExecFlushed(t, pool, `VACUUM ANALYZE m191_refill`)
		_, rp0 := m191Stats(t, pool, "deleted 9 of 10, vacuumed", "m191_refill")
		m191Fill(t, pool, "m191_refill", 20001, 38000)
		rt, rp := m191Stats(t, pool, "refilled to 20000 rows", "m191_refill")
		var pages int64
		if err := pool.QueryRow(ctx, `SELECT pg_relation_size('m191_refill') / 8192`).Scan(&pages); err != nil {
			t.Fatal(err)
		}
		if rt != 2000 || pages > int64(rp0)+int64(rp0)/10 {
			t.Fatalf("reltuples %.0f, heap %d pages (relpages %d); want 2000 and the heap within 10%% of %d", rt, pages, rp, rp0)
		}
		if n := m191Live(t, pool, "m191_refill"); n != 20000 {
			t.Fatalf("n_live_tup %d, want 20000", n)
		}
		if got := m191Rows(t, pool, "m191_refill", 10000); got != 20000 {
			t.Errorf("got %d, want 20000 (n_live_tup, above the threshold, uncounted; the estimate and the density read about 2000)", got)
		}
		// The counters dropped: nothing left that sees the refill but a count.
		m191ResetCounters(t, pool, "m191_refill")
		if got := m191Rows(t, pool, "m191_refill", 10000); got != 10000 {
			t.Errorf("counters dropped: got %d, want 10000 (a count stopped at the threshold)", got)
		}
	})

	t.Run("analysed at fewer than 32 pages, grown since: a count", func(t *testing.T) {
		// Ten rows on one page: a stored density of 10 rows per page where
		// a full page holds about 48.
		m191Table(t, pool, "m191_thin")
		m191Fill(t, pool, "m191_thin", 1, 10)
		m191Exec(t, pool, `ANALYZE m191_thin`)
		m191Fill(t, pool, "m191_thin", 11, 60000)
		m191Stats(t, pool, "analysed at 10 rows, grown to 60000", "m191_thin")
		if got := m191Rows(t, pool, "m191_thin", 20000); got != 20000 {
			t.Errorf("got %d, want 20000 (a count stopped at the threshold)", got)
		}
	})

	t.Run("after TRUNCATE", func(t *testing.T) {
		m191Table(t, pool, "m191_trunc")
		m191Fill(t, pool, "m191_trunc", 1, 2000)
		m191Exec(t, pool, `ANALYZE m191_trunc`)
		m191Exec(t, pool, `TRUNCATE m191_trunc`)
		m191Stats(t, pool, "truncated", "m191_trunc")
		if got := m191Rows(t, pool, "m191_trunc", 1000); got != 0 {
			t.Errorf("truncated: got %d, want 0", got)
		}
		m191Fill(t, pool, "m191_trunc", 1, 1500)
		if got := m191Rows(t, pool, "m191_trunc", 1000); got != 1000 {
			t.Errorf("truncated and filled again: got %d, want 1000 (reltuples -1 after TRUNCATE: a count)", got)
		}
	})

	t.Run("bloated since ANALYZE: the estimate says above (conservative)", func(t *testing.T) {
		m191Table(t, pool, "m191_bloat")
		m191Fill(t, pool, "m191_bloat", 1, 2000)
		m191Exec(t, pool, `ANALYZE m191_bloat`)
		m191Exec(t, pool, `UPDATE m191_bloat SET pad = pad || 'u'`)
		m191Exec(t, pool, `UPDATE m191_bloat SET pad = pad || 'v'`)
		m191Stats(t, pool, "updated twice since ANALYZE", "m191_bloat")
		got := m191Rows(t, pool, "m191_bloat", 3000)
		t.Logf("ctx_migration_rows = %d for 2000 live rows at threshold 3000", got)
		if got < 3000 {
			t.Errorf("got %d, want at least 3000: dead row versions grew the heap, the density times the size reads them, and the estimate decides above uncounted", got)
		}
		// Far above the estimate the rows are counted: 2000.
		if got := m191Rows(t, pool, "m191_bloat", 1000000); got != 2000 {
			t.Errorf("threshold 1000000: got %d, want 2000 (below the threshold: the exact count)", got)
		}
	})

	t.Run("an estimate above both other measures: the estimate", func(t *testing.T) {
		// Relation statistics restored onto a packed heap (as
		// pg_restore_relation_stats writes them): twice the rows on twice the
		// pages, so the stored density times the size and n_live_tup read
		// 2000 and only reltuples reads 4000.
		m191Table(t, pool, "m191_dense")
		m191Fill(t, pool, "m191_dense", 1, 2000)
		m191Exec(t, pool, `ANALYZE m191_dense`)
		_, rp := m191Stats(t, pool, "analysed", "m191_dense")
		m191Exec(t, pool, `UPDATE pg_class SET reltuples = 4000, relpages = $1 WHERE oid = 'm191_dense'::regclass`, 2*rp)
		if got := m191Rows(t, pool, "m191_dense", 3000); got != 4000 {
			t.Errorf("got %d, want 4000 (reltuples, larger than the density times the size and n_live_tup, above the threshold)", got)
		}
	})

	t.Run("an estimate a fraction below the threshold rounds up", func(t *testing.T) {
		// 999.5 rows by the estimate at threshold 1000: up, to the threshold
		// (out of band), not down to a count.
		m191Table(t, pool, "m191_round")
		m191Fill(t, pool, "m191_round", 1, 2000)
		m191ExecFlushed(t, pool, `DELETE FROM m191_round WHERE id % 4 <> 0`)
		m191ExecFlushed(t, pool, `VACUUM ANALYZE m191_round`)
		_, rp := m191Stats(t, pool, "500 rows left, vacuumed", "m191_round")
		m191Exec(t, pool, `UPDATE pg_class SET reltuples = 999.5, relpages = $1 WHERE oid = 'm191_round'::regclass`, rp)
		if got := m191Rows(t, pool, "m191_round", 1000); got != 1000 {
			t.Errorf("got %d, want 1000 (the estimate 999.5 rounded up, at the threshold)", got)
		}
	})

	t.Run("a coarse density that claims above is counted (fewer than 32 pages)", func(t *testing.T) {
		// Five pages of short rows analysed (about 180 rows a page), then
		// wide rows (about 4 a page): the stored density times the size
		// claims some 90 000 rows for 2 900.
		m191Exec(t, pool, `CREATE TABLE m191_coarse (id bigint, pad text) WITH (autovacuum_enabled = false)`)
		m191ExecFlushed(t, pool, `INSERT INTO m191_coarse SELECT g, 'x' FROM generate_series(1, 900) g`)
		m191Exec(t, pool, `ANALYZE m191_coarse`)
		m191ExecFlushed(t, pool, `INSERT INTO m191_coarse SELECT g, repeat('w', 1800) FROM generate_series(901, 2900) g`)
		_, rp := m191Stats(t, pool, "analysed on short rows, grown with wide ones", "m191_coarse")
		if rp < 2 || rp >= 32 {
			t.Fatalf("relpages %d, want 2 to 31 (a density from a few pages)", rp)
		}
		if got := m191Rows(t, pool, "m191_coarse", 10000); got != 2900 {
			t.Errorf("got %d, want 2900 (a density from fewer than 32 pages decides nothing: the exact count)", got)
		}
	})

	t.Run("vacuumed, never analysed: a record, the estimate stands", func(t *testing.T) {
		m191Table(t, pool, "m191_vaconly")
		m191Fill(t, pool, "m191_vaconly", 1, 20000)
		m191ExecFlushed(t, pool, `VACUUM m191_vaconly`)
		rt, _ := m191Stats(t, pool, "vacuumed only", "m191_vaconly")
		if rt != 20000 {
			t.Fatalf("reltuples %.0f, want 20000 after VACUUM", rt)
		}
		if got := m191Rows(t, pool, "m191_vaconly", 10000); got != 20000 {
			t.Errorf("got %d, want 20000 (the estimate, above the threshold; a VACUUM is a record)", got)
		}
	})

	t.Run("a child whose counters are lost while its parent's stand: a count", func(t *testing.T) {
		// The record of a VACUUM or ANALYZE is read per relation of the
		// tree: the child, bloated since its ANALYZE, claims about 60 000
		// rows by density times size, and only its counters say otherwise.
		m191Table(t, pool, "m191_lostp")
		m191Exec(t, pool, `CREATE TABLE m191_lostc () INHERITS (m191_lostp) WITH (autovacuum_enabled = false)`)
		// The parent holds 2000 rows on more than 32 pages, so nothing of its
		// own sends the function to a count.
		m191Fill(t, pool, "m191_lostp", 1, 2000)
		m191Fill(t, pool, "m191_lostc", 1, 20000)
		m191Exec(t, pool, `ANALYZE m191_lostp`)
		m191Exec(t, pool, `ANALYZE m191_lostc`)
		m191ExecFlushed(t, pool, `UPDATE m191_lostc SET pad = pad || 'u'`)
		m191ExecFlushed(t, pool, `UPDATE m191_lostc SET pad = pad || 'v'`)
		m191ResetCounters(t, pool, "m191_lostc")
		if _, rp := m191Stats(t, pool, "parent", "m191_lostp"); rp < 32 {
			t.Fatalf("parent relpages %d, want 32 or more", rp)
		}
		m191Stats(t, pool, "child, bloated, counters lost", "m191_lostc")
		if got := m191Rows(t, pool, "m191_lostp", 50000); got != 22000 {
			t.Errorf("got %d, want 22000 (the child has no record: the exact count; its estimate alone reads about 60 000)", got)
		}
	})

	t.Run("a child under two parents of the tree is measured once", func(t *testing.T) {
		m191Table(t, pool, "m191_mi_a")
		m191Exec(t, pool, `CREATE TABLE m191_mi_b () INHERITS (m191_mi_a) WITH (autovacuum_enabled = false)`)
		m191Exec(t, pool, `CREATE TABLE m191_mi_c () INHERITS (m191_mi_a, m191_mi_b) WITH (autovacuum_enabled = false)`)
		m191Fill(t, pool, "m191_mi_c", 1, 20000)
		for _, rel := range []string{"m191_mi_a", "m191_mi_b", "m191_mi_c"} {
			m191Exec(t, pool, `ANALYZE `+rel)
		}
		if got := m191Rows(t, pool, "m191_mi_a", 30000); got != 20000 {
			t.Errorf("got %d, want 20000 (one child, counted below the threshold; twice it would read 40 000)", got)
		}
	})

	t.Run("TOAST: the main fork carries the density", func(t *testing.T) {
		// Values of about 2.6 KB stored out of line uncompressed: the main
		// heap holds a pointer per row.
		m191Exec(t, pool, `CREATE TABLE m191_toast (id bigint, body text) WITH (autovacuum_enabled = false)`)
		m191Exec(t, pool, `ALTER TABLE m191_toast ALTER COLUMN body SET STORAGE EXTERNAL`)
		fill := func(from, to int) {
			m191ExecFlushed(t, pool, `INSERT INTO m191_toast SELECT g, (SELECT string_agg(md5(g::text || ':' || i), '') FROM generate_series(1, 80) i) FROM generate_series($1::int, $2::int) g`, from, to)
		}
		fill(1, 6000)
		m191Exec(t, pool, `ANALYZE m191_toast`)
		fill(6001, 18000)
		m191Stats(t, pool, "toasted rows, analysed at 6000, grown to 18000", "m191_toast")
		var toast int64
		if err := pool.QueryRow(ctx, `SELECT pg_relation_size(reltoastrelid) FROM pg_class WHERE oid = 'm191_toast'::regclass`).Scan(&toast); err != nil {
			t.Fatal(err)
		}
		if toast == 0 {
			t.Fatal("the rows were not moved to TOAST; the case tests nothing")
		}
		got := m191Rows(t, pool, "m191_toast", 10000)
		t.Logf("ctx_migration_rows = %d for 18000 rows, TOAST %d pages", got, toast/8192)
		m191Within(t, "toast", got, 18000, 0.95, 1.10)
	})

	t.Run("partitioned table, two levels", func(t *testing.T) {
		m191Exec(t, pool, `CREATE TABLE m191_part (id bigint, pad text) PARTITION BY RANGE (id)`)
		m191Exec(t, pool, `CREATE TABLE m191_part_a PARTITION OF m191_part FOR VALUES FROM (0) TO (1000000) WITH (autovacuum_enabled = false)`)
		m191Exec(t, pool, `CREATE TABLE m191_part_b PARTITION OF m191_part FOR VALUES FROM (1000000) TO (3000000) PARTITION BY RANGE (id)`)
		m191Exec(t, pool, `CREATE TABLE m191_part_b1 PARTITION OF m191_part_b FOR VALUES FROM (1000000) TO (2000000) WITH (autovacuum_enabled = false)`)
		m191Exec(t, pool, `CREATE TABLE m191_part_b2 PARTITION OF m191_part_b FOR VALUES FROM (2000000) TO (3000000) WITH (autovacuum_enabled = false)`)
		m191Fill(t, pool, "m191_part", 1, 2000)
		m191Fill(t, pool, "m191_part", 1000001, 1002000)
		m191Fill(t, pool, "m191_part", 2000001, 2002000)
		m191Exec(t, pool, `ANALYZE m191_part`)
		m191Fill(t, pool, "m191_part", 2002001, 2030000)
		m191Stats(t, pool, "parent", "m191_part")
		m191Stats(t, pool, "grown leaf", "m191_part_b2")
		got := m191Rows(t, pool, "m191_part", 10000)
		want := m191Count(t, pool, "m191_part")
		t.Logf("ctx_migration_rows = %d, the copied query %d, rows %d", got, m191Old(t, pool, "m191_part", 10000), want)
		m191Within(t, "partitioned", got, want, 0.95, 1.10)
	})

	t.Run("inheritance parent with rows of its own", func(t *testing.T) {
		m191Table(t, pool, "m191_inh")
		m191Exec(t, pool, `CREATE TABLE m191_inh_child () INHERITS (m191_inh) WITH (autovacuum_enabled = false)`)
		m191Fill(t, pool, "m191_inh", 1, 2000)
		m191Fill(t, pool, "m191_inh_child", 1, 2000)
		m191Exec(t, pool, `ANALYZE m191_inh`)
		m191Exec(t, pool, `ANALYZE m191_inh_child`)
		m191Fill(t, pool, "m191_inh_child", 2001, 20000)
		got := m191Rows(t, pool, "m191_inh", 10000)
		m191Within(t, "inheritance", got, 22000, 0.95, 1.10)
	})

	t.Run("TimescaleDB hypertable: the chunks carry the numbers", func(t *testing.T) {
		m191Exec(t, pool, `CREATE TABLE m191_hyper (ts timestamptz NOT NULL, id bigint, pad text)`)
		m191Exec(t, pool, `SELECT create_hypertable('m191_hyper', 'ts', chunk_time_interval => interval '1 day')`)
		m191Exec(t, pool, `ALTER TABLE m191_hyper SET (autovacuum_enabled = false)`)
		hyperFill := func(day0, days, perDay int) {
			m191ExecFlushed(t, pool, `INSERT INTO m191_hyper SELECT timestamptz '2026-01-01' + ($1::int + g % $2::int) * interval '1 day' + g * interval '1 ms', g, repeat('y', 120) || g
				FROM generate_series(1, $3::int) g`, day0, days, days*perDay)
		}
		hyperFill(0, 3, 2000)
		m191Exec(t, pool, `ANALYZE m191_hyper`)
		rt, _ := m191Stats(t, pool, "hypertable parent, analysed", "m191_hyper")
		if rt > 0 {
			t.Fatalf("hypertable parent reltuples %.0f, want 0 or -1 (the chunks carry the numbers)", rt)
		}
		var chunks, analysed int
		if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE c.reltuples > 0 AND c.relpages >= 32)
			  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'm191_hyper'::regclass`).Scan(&chunks, &analysed); err != nil {
			t.Fatal(err)
		}
		if chunks != 3 || analysed != 3 {
			t.Fatalf("%d chunks, %d of them with a density to trust; want 3 and 3", chunks, analysed)
		}
		// Thresholds below the rows: a count would return the threshold.
		if got := m191Rows(t, pool, "m191_hyper", 1000); got < 5700 || got > 6600 {
			t.Errorf("analysed: got %d, want about 6000 (the chunks' measures, not a count)", got)
		}
		// Grown inside the analysed chunks.
		hyperFill(0, 3, 6000)
		want := m191Count(t, pool, "m191_hyper")
		got := m191Rows(t, pool, "m191_hyper", 10000)
		t.Logf("grown chunks: ctx_migration_rows = %d, the copied query %d, rows %d", got, m191Old(t, pool, "m191_hyper", 10000), want)
		m191Within(t, "hypertable grown", got, want, 0.95, 1.10)
		if old := m191Old(t, pool, "m191_hyper", 10000); old != 0 {
			t.Errorf("the copied query on the parent: got %d, want 0 (it builds inline)", old)
		}
		// A new chunk, never analysed: a count through the parent.
		hyperFill(10, 1, 2000)
		if got := m191Rows(t, pool, "m191_hyper", 10000); got != 10000 {
			t.Errorf("with a chunk never analysed: got %d, want 10000 (a count stopped at the threshold)", got)
		}
	})

	t.Run("TimescaleDB hypertable with compressed chunks: a count", func(t *testing.T) {
		m191Exec(t, pool, `CREATE TABLE m191_comp (ts timestamptz NOT NULL, id bigint, pad text)`)
		m191Exec(t, pool, `SELECT create_hypertable('m191_comp', 'ts', chunk_time_interval => interval '1 day')`)
		m191Exec(t, pool, `ALTER TABLE m191_comp SET (autovacuum_enabled = false)`)
		m191Exec(t, pool, `INSERT INTO m191_comp SELECT timestamptz '2026-01-01' + (g % 3) * interval '1 day' + g * interval '1 ms', g, repeat('z', 120) || g FROM generate_series(1, 6000) g`)
		m191Exec(t, pool, `ANALYZE m191_comp`)
		m191Exec(t, pool, `ALTER TABLE m191_comp SET (timescaledb.compress)`)
		m191Exec(t, pool, `SELECT compress_chunk(c) FROM show_chunks('m191_comp') c`)
		m191Exec(t, pool, `ANALYZE m191_comp`)
		var inTree int64
		if err := pool.QueryRow(ctx, `SELECT coalesce(sum(pg_relation_size(inhrelid)), 0) FROM pg_inherits WHERE inhparent = 'm191_comp'::regclass`).Scan(&inTree); err != nil {
			t.Fatal(err)
		}
		t.Logf("compressed: the chunks under the parent hold %d bytes", inTree)
		if got := m191Rows(t, pool, "m191_comp", 100000); got != 6000 {
			t.Errorf("got %d, want 6000 (rows in compressed chunks are outside the inheritance tree: a count)", got)
		}
		if got := m191Rows(t, pool, "m191_comp", 1000); got != 1000 {
			t.Errorf("threshold 1000: got %d, want 1000", got)
		}
	})

	t.Run("the shipped hypertables and the tables of the threshold migrations", func(t *testing.T) {
		rows, err := pool.Query(ctx, `SELECT format('%I.%I', hypertable_schema, hypertable_name) FROM timescaledb_information.hypertables WHERE hypertable_schema = 'public' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		var hyper []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			hyper = append(hyper, n)
		}
		rows.Close()
		t.Logf("hypertables in public: %v", hyper)
		for _, tbl := range []string{"context_blocks", "context_write_log", "context_embed_failures", "context_access_tokens",
			"context_web_sessions", "context_block_grants", "context_oauth_codes", "context_chat_sessions"} {
			for _, h := range hyper {
				if h == "public."+tbl {
					t.Errorf("%s is a hypertable", tbl)
				}
			}
		}
		if got := m191Rows(t, pool, "public.context_llm_log", 500000); got != 0 {
			t.Errorf("context_llm_log, empty: got %d, want 0", got)
		}
	})

	t.Run("a table in another schema", func(t *testing.T) {
		m191Exec(t, pool, `CREATE SCHEMA m191_other`)
		m191Exec(t, pool, `CREATE TABLE m191_other.m191_same (id bigint, pad text) WITH (autovacuum_enabled = false)`)
		m191Table(t, pool, "public.m191_same")
		m191Fill(t, pool, "m191_other.m191_same", 1, 1500)
		m191Fill(t, pool, "public.m191_same", 1, 10)
		if got := m191Rows(t, pool, "m191_other.m191_same", 1000); got != 1000 {
			t.Errorf("m191_other.m191_same: got %d, want 1000", got)
		}
		if got := m191Rows(t, pool, "public.m191_same", 1000); got != 10 {
			t.Errorf("public.m191_same: got %d, want 10", got)
		}
		// The count names the relation by its regclass text: quoted.
		m191Exec(t, pool, `CREATE TABLE m191_other."M191 Odd; Name" (id bigint) WITH (autovacuum_enabled = false)`)
		m191Exec(t, pool, `INSERT INTO m191_other."M191 Odd; Name" SELECT g FROM generate_series(1, 1500) g`)
		if got := m191Rows(t, pool, `m191_other."M191 Odd; Name"`, 1000); got != 1000 {
			t.Errorf(`m191_other."M191 Odd; Name": got %d, want 1000`, got)
		}
	})

	t.Run("a missing relation fails", func(t *testing.T) {
		if code, msg := m191SQLState(t, pool, `SELECT ctx_migration_rows('public.m191_missing', 10)`); code != "42P01" {
			t.Errorf("unknown name: SQLSTATE %q (%s), want 42P01", code, msg)
		}
		m191Table(t, pool, "m191_gone")
		var oid uint32
		if err := pool.QueryRow(ctx, `SELECT 'm191_gone'::regclass::oid`).Scan(&oid); err != nil {
			t.Fatal(err)
		}
		m191Exec(t, pool, `DROP TABLE m191_gone`)
		code, msg := m191SQLState(t, pool, `SELECT ctx_migration_rows($1::oid::regclass, 10)`, oid)
		if code != "42P01" {
			t.Errorf("dropped oid: SQLSTATE %q (%s), want 42P01", code, msg)
		}
		t.Logf("dropped oid: %s", msg)
	})

	t.Run("not a table fails", func(t *testing.T) {
		m191Table(t, pool, "m191_base")
		m191Exec(t, pool, `CREATE VIEW m191_view AS SELECT * FROM m191_base`)
		m191Exec(t, pool, `CREATE INDEX m191_base_idx ON m191_base (id)`)
		m191Exec(t, pool, `CREATE SEQUENCE m191_seq`)
		m191Exec(t, pool, `CREATE MATERIALIZED VIEW m191_mat AS SELECT * FROM m191_base`)
		for _, rel := range []string{"m191_view", "m191_base_idx", "m191_seq", "m191_mat"} {
			code, msg := m191SQLState(t, pool, `SELECT ctx_migration_rows($1::regclass, 10)`, rel)
			if code != "42809" {
				t.Errorf("%s: SQLSTATE %q (%s), want 42809", rel, code, msg)
			}
		}
	})

	t.Run("a missing or non-positive threshold fails", func(t *testing.T) {
		m191Table(t, pool, "m191_thr")
		for _, q := range []string{
			`SELECT ctx_migration_rows('m191_thr', NULL)`,
			`SELECT ctx_migration_rows('m191_thr', 0)`,
			`SELECT ctx_migration_rows('m191_thr', -5)`,
			`SELECT ctx_migration_rows(NULL, 10)`,
		} {
			if code, msg := m191SQLState(t, pool, q); code != "22023" {
				t.Errorf("%s: SQLSTATE %q (%s), want 22023", q, code, msg)
			}
		}
	})
}

// TestMigration191EstimateBelowIsCounted: an estimate below the threshold
// decides nothing, the rows are counted. VACUUM sets reltuples and
// n_live_tup together by extrapolation (vac_estimate_reltuples): the pages
// it skips as all-visible count at the stored density of the whole table,
// so after a mass delete beside an all-visible region all three measures
// carry the same error. Each case read below the threshold on a table above
// it (the review of #484 found them); a count says above.
func TestMigration191EstimateBelowIsCounted(t *testing.T) {
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	check := func(label, rel string, threshold int64) {
		t.Helper()
		truth := m191Count(t, pool, rel)
		rt, rp := m191Stats(t, pool, label, rel)
		live := m191Live(t, pool, rel)
		got := m191Rows(t, pool, rel, threshold)
		t.Logf("%s: rows %d, reltuples %.0f, relpages %d, n_live_tup %d, ctx_migration_rows %d at threshold %d", label, truth, rt, rp, live, got, threshold)
		if truth < threshold {
			t.Fatalf("%s: %d rows, below the threshold %d; the case tests nothing", label, truth, threshold)
		}
		if got < threshold {
			t.Errorf("%s: ctx_migration_rows = %d for %d rows at threshold %d: the file would build inline above its threshold", label, got, truth, threshold)
		}
	}
	// 100 000 rows, all-visible; every row beyond the first 10 000 but one
	// in a thousand deleted; ANALYZE (10 090, right), then VACUUM, which
	// skips the 10 000 rows' pages and extrapolates over them (about 1 100).
	for _, freeze := range []bool{true, false} {
		rel := "m191_p1_nofreeze"
		if freeze {
			rel = "m191_p1"
		}
		m191Table(t, pool, rel)
		m191Fill(t, pool, rel, 1, 100000)
		if freeze {
			m191ExecFlushed(t, pool, `VACUUM (FREEZE) `+rel)
		} else {
			m191ExecFlushed(t, pool, `VACUUM `+rel)
		}
		m191ExecFlushed(t, pool, `DELETE FROM `+rel+` WHERE id > 10000 AND id % 1000 <> 0`)
		m191ExecFlushed(t, pool, `ANALYZE `+rel)
		m191ExecFlushed(t, pool, `VACUUM `+rel)
		check(fmt.Sprintf("mass delete beside an all-visible region, ANALYZE, VACUUM (freeze %v)", freeze), rel, 5000)
	}
	// The same table refilled to 100 000 rows into the freed space.
	m191Fill(t, pool, "m191_p1", 200001, 200001+89909)
	check("the same, refilled to 100 000", "m191_p1", 95000)

	// Retention: the oldest 90 % deleted, ANALYZE, VACUUM.
	m191Table(t, pool, "m191_ret")
	m191Fill(t, pool, "m191_ret", 1, 100000)
	m191ExecFlushed(t, pool, `VACUUM (FREEZE) m191_ret`)
	m191ExecFlushed(t, pool, `DELETE FROM m191_ret WHERE id <= 90000`)
	m191ExecFlushed(t, pool, `ANALYZE m191_ret`)
	m191ExecFlushed(t, pool, `VACUUM m191_ret`)
	check("retention: oldest 90 % deleted, ANALYZE, VACUUM", "m191_ret", 5000)

	// VACUUM only, never ANALYZE (the issue's setting with autovacuum off and
	// a routine VACUUM): half deleted, VACUUM, refilled, VACUUM.
	m191Table(t, pool, "m191_vac")
	m191Fill(t, pool, "m191_vac", 1, 100000)
	m191ExecFlushed(t, pool, `VACUUM (FREEZE) m191_vac`)
	m191ExecFlushed(t, pool, `DELETE FROM m191_vac WHERE id <= 50000`)
	m191ExecFlushed(t, pool, `VACUUM m191_vac`)
	m191Fill(t, pool, "m191_vac", 100001, 150000)
	m191ExecFlushed(t, pool, `VACUUM m191_vac`)
	check("VACUUM only: half deleted, VACUUM, refilled, VACUUM", "m191_vac", 90000)

	// A refill whose counts a live session still holds back (flushed at
	// most once a second, and at its end): n_live_tup trails.
	m191Table(t, pool, "m191_pend")
	m191Fill(t, pool, "m191_pend", 1, 20000)
	m191ExecFlushed(t, pool, `DELETE FROM m191_pend WHERE id % 10 <> 0`)
	m191ExecFlushed(t, pool, `VACUUM ANALYZE m191_pend`)
	w, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close(ctx) }()
	if _, err := w.Exec(ctx, `UPDATE m191_pend SET id = id WHERE id = 10`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(ctx, `INSERT INTO m191_pend SELECT g, repeat('x', 120) || g FROM generate_series(20001, 38000) g`); err != nil {
		t.Fatal(err)
	}
	check("refill with its counts pending in a live session", "m191_pend", 10000)
}

// TestMigration191Locks: the migration takes no lock on a table, runs twice
// alike, and the function holds no lock on the table it measures once it
// returns an estimate (pg_relation_size releases its ACCESS SHARE at once);
// its count holds ACCESS SHARE to the end of the transaction, as any read.
func TestMigration191Locks(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 190)
	body, err := migrations.FS.ReadFile(m191File)
	if err != nil {
		t.Fatal(err)
	}

	// Every table the threshold migrations measure, held ACCESS EXCLUSIVE
	// by another transaction while the file runs: a lock on any of them
	// would end the file with 55P03 after its 3 s lock_timeout.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE context_blocks, context_write_log, context_embed_failures, context_access_tokens,
		context_web_sessions, context_block_grants, context_oauth_codes, context_chat_sessions, context_llm_log IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		t.Fatalf("the file beside ACCESS EXCLUSIVE holders of the data tables: %v", err)
	}
	took := time.Since(t0)
	// Relations of the database (oid from FirstNormalObjectId, 16384, on):
	// every table of ctx and its TOAST; the system catalogs and their TOAST
	// (pg_proc's holds the function body) stay below.
	rows, err := tx.Query(ctx, `SELECT c.oid::regclass::text, c.relkind::text, l.mode
		  FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		 WHERE l.pid = pg_backend_pid() AND l.locktype = 'relation' AND c.oid >= 16384
		 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	for rows.Next() {
		var rel, kind, mode string
		if err := rows.Scan(&rel, &kind, &mode); err != nil {
			t.Fatal(err)
		}
		held = append(held, rel+" ("+kind+") "+mode)
	}
	rows.Close()
	var catalog []string
	rows, err = tx.Query(ctx, `SELECT coalesce(l.relation::regclass::text, l.locktype) || ' ' || l.mode
		  FROM pg_locks l WHERE l.pid = pg_backend_pid() AND l.locktype IN ('relation', 'object') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		catalog = append(catalog, s)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_ = holder.Rollback(ctx)
	t.Logf("the file took %s beside the holders; locks of its transaction: %v", took.Round(time.Millisecond), catalog)
	if len(held) > 0 {
		t.Errorf("the file holds locks on relations of the database: %v", held)
	}

	var def1, def2 string
	if err := pool.QueryRow(ctx, `SELECT pg_get_functiondef('ctx_migration_rows(regclass, bigint)'::regprocedure)`).Scan(&def1); err != nil {
		t.Fatal(err)
	}
	m191Exec(t, pool, string(body))
	if err := pool.QueryRow(ctx, `SELECT pg_get_functiondef('ctx_migration_rows(regclass, bigint)'::regprocedure)`).Scan(&def2); err != nil {
		t.Fatal(err)
	}
	if def1 != def2 {
		t.Error("a second run changed the function")
	}
	var conf []string
	var vol string
	var secdef bool
	if err := pool.QueryRow(ctx, `SELECT proconfig, provolatile::text, prosecdef FROM pg_proc WHERE oid = 'ctx_migration_rows(regclass, bigint)'::regprocedure`).Scan(&conf, &vol, &secdef); err != nil {
		t.Fatal(err)
	}
	if strings.Join(conf, ";") != "search_path=pg_catalog, pg_temp" || secdef {
		t.Errorf("proconfig %v, security definer %v; want search_path=pg_catalog, pg_temp and invoker rights", conf, secdef)
	}
	t.Logf("provolatile %s", vol)

	held1 := func(rel string, q string) []string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var n int64
		if err := tx.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, `SELECT mode FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'relation' AND relation = $1::regclass ORDER BY 1`, rel)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var modes []string
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				t.Fatal(err)
			}
			modes = append(modes, m)
		}
		return modes
	}
	m191Table(t, pool, "m191_lk")
	m191Fill(t, pool, "m191_lk", 1, 2000)
	if modes := held1("m191_lk", `SELECT ctx_migration_rows('m191_lk', 100)`); len(modes) != 1 || modes[0] != "AccessShareLock" {
		t.Errorf("count (never analysed): locks held on the table %v, want [AccessShareLock] (a read)", modes)
	}
	m191Exec(t, pool, `ANALYZE m191_lk`)
	if modes := held1("m191_lk", `SELECT ctx_migration_rows('m191_lk', 100)`); len(modes) != 0 {
		t.Errorf("estimate (analysed): locks held on the table after the call %v, want none", modes)
	}

	// While another transaction holds ACCESS EXCLUSIVE on the table, the
	// estimate waits for it like any reader (pg_relation_size opens the
	// relation): the caller's lock_timeout ends the wait with 55P03.
	h2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h2.Rollback(ctx) }()
	if _, err := h2.Exec(ctx, `LOCK TABLE m191_lk IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	tx3, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx3.Rollback(ctx) }()
	if _, err := tx3.Exec(ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
		t.Fatal(err)
	}
	var n int64
	err = tx3.QueryRow(ctx, `SELECT ctx_migration_rows('m191_lk', 100)`).Scan(&n)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Errorf("beside an ACCESS EXCLUSIVE holder: %v, want 55P03 after the lock_timeout", err)
	}
}

// m191Apply runs body in one transaction over a pool of its own, as the
// runner does, and returns the WARNINGs it raised.
func m191Apply(t *testing.T, dsn, body string) ([]string, time.Duration) {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var warnings []string
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		if n.Severity == "WARNING" {
			warnings = append(warnings, n.Message)
		}
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if _, err := tx.Exec(ctx, body); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	took := time.Since(t0)
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), warnings...), took
}

// m191Rebuilt is file with its copied size decision (the reltuples read and
// the bounded count after it) replaced by one call: the form the file takes
// with the function, nothing else changed.
func m191Rebuilt(t *testing.T, file, call string) string {
	t.Helper()
	b, err := migrations.FS.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	const decision = "SELECT c.reltuples::BIGINT INTO v_rows"
	if n := strings.Count(body, decision); n != 1 {
		t.Fatalf("%s: %d size decisions, want 1", file, n)
	}
	start := strings.Index(body, decision)
	lineStart := strings.LastIndex(body[:start], "\n") + 1
	end := strings.Index(body[start:], "END IF;\n")
	if end < 0 {
		t.Fatalf("%s: no END IF after the size decision", file)
	}
	end += start + len("END IF;\n")
	return body[:lineStart] + body[lineStart:start] + call + "\n" + body[end:]
}

// m191Scaled replaces the thresholds of a file body (old -> new) and fails
// when one of them is not there.
func m191Scaled(t *testing.T, body string, pairs ...string) string {
	t.Helper()
	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.Contains(body, pairs[i]) {
			t.Fatalf("threshold %q not found", pairs[i])
		}
		body = strings.ReplaceAll(body, pairs[i], pairs[i+1])
	}
	return body
}

func m191IndexExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = $1)`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// m191SeedTokens writes access tokens n+1..m (two per family: the access row
// points at the refresh row through parent_id) on the key's principal.
func m191SeedTokens(t *testing.T, pool *pgxpool.Pool, keyID, principal string, from, to int) {
	t.Helper()
	m191Exec(t, pool, `
		INSERT INTO context_access_tokens (id, token_hash, token_type, api_key_id, principal_id, audiences, refresh_family, parent_id, issued_via, expires_at)
		SELECT md5('tok-' || g)::uuid, encode(sha256(convert_to('tok-' || g, 'UTF8')), 'hex'),
		       CASE WHEN g % 2 = 0 THEN 'access' ELSE 'refresh' END, $3::uuid, $4::uuid, ARRAY['ctx-mcp', 'ctx-web'],
		       md5('fam-' || (g / 2))::uuid, CASE WHEN g % 2 = 0 THEN md5('tok-' || (g - 1))::uuid END, 'login', now() + interval '30 days'
		  FROM generate_series($1::int, $2::int) g
		 ORDER BY g`, from, to, keyID, principal)
}

// m191Key creates an API key and returns its id and principal.
func m191Key(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	ctx := context.Background()
	k, _, err := store.CreateApiKey(ctx, pool, "m191-key", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var principal string
	if err := pool.QueryRow(ctx, `SELECT principal_id FROM context_api_keys WHERE id = $1::uuid`, k.ID).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	return k.ID, principal
}

const m191Call = "v_rows := ctx_migration_rows(format('public.%I', v_idx.tbl)::regclass, v_idx.max_rows);"

// TestMigration191IssueMeasurement replays the measurement of issue #399 at
// a fiftieth of its size, with the thresholds scaled alike: the table
// analysed at a fifth of the threshold and grown to six times it without an
// ANALYZE (autovacuum off). The files 184 and 172 as shipped build inline
// above the threshold; the same files with their size decision replaced by
// ctx_migration_rows warn and build nothing on that table.
func TestMigration191IssueMeasurement(t *testing.T) {
	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 183)

	keyID, principal := m191Key(t, pool)
	for _, tbl := range []string{"context_access_tokens", "context_web_sessions", "context_write_log"} {
		m191Exec(t, pool, `ALTER TABLE `+tbl+` SET (autovacuum_enabled = false)`)
	}
	m191SeedTokens(t, pool, keyID, principal, 1, 2000)
	m191Exec(t, pool, `ANALYZE context_access_tokens`)
	m191Exec(t, pool, `ANALYZE context_web_sessions`)
	m191SeedTokens(t, pool, keyID, principal, 2001, 60000)
	m191Stats(t, pool, "184: analysed at 2000, grown to 60000", "context_access_tokens")
	t.Logf("184 tokens: the copied query %d (threshold 10000)", m191Old(t, pool, "public.context_access_tokens", 10000))

	old184, err := migrations.FS.ReadFile("184_access_token_fk_lookup_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	scale184 := []string{" 500000)", " 10000)"}
	warnings, took := m191Apply(t, dsn, m191Scaled(t, string(old184), scale184...))
	if !m191IndexExists(t, pool, "idx_access_tokens_parent") {
		t.Fatalf("184 as shipped: no inline build at 60000 rows analysed at 2000 (warnings %v); the issue does not reproduce", warnings)
	}
	t.Logf("184 as shipped: built idx_access_tokens_parent inline at 60000 rows (threshold 10000) in %s, warnings %v", took.Round(time.Millisecond), warnings)
	m191Exec(t, pool, `DROP INDEX idx_access_tokens_parent`)
	m191Exec(t, pool, `DROP INDEX idx_web_sessions_access_token`)

	// The function of migration 191 on this schema 183 (it reads the
	// catalog only and depends on no migration after 113).
	body191, err := migrations.FS.ReadFile(m191File)
	if err != nil {
		t.Fatal(err)
	}
	m191Exec(t, pool, string(body191))
	t.Logf("184 tokens: ctx_migration_rows %d (threshold 10000)", m191Rows(t, pool, "public.context_access_tokens", 10000))
	warnings, _ = m191Apply(t, dsn, m191Scaled(t, m191Rebuilt(t, "184_access_token_fk_lookup_indexes.sql", m191Call), scale184...))
	if m191IndexExists(t, pool, "idx_access_tokens_parent") {
		t.Errorf("184 with ctx_migration_rows: built idx_access_tokens_parent inline at 60000 rows (threshold 10000)")
	}
	if !m191IndexExists(t, pool, "idx_web_sessions_access_token") {
		t.Errorf("184 with ctx_migration_rows: the empty session table got no inline build")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "idx_access_tokens_parent is missing at") {
		t.Errorf("184 with ctx_migration_rows: warnings %v, want the one of the token index", warnings)
	}
	t.Logf("184 with ctx_migration_rows: %v", warnings)

	// 172 on the write log (threshold 3 000 000, scaled to 30 000): drop
	// the indexes 172 built when the chain passed it, analyse at 2000 rows,
	// grow to 60 000.
	m191Exec(t, pool, `DROP INDEX idx_write_log_matched_block`)
	m191Exec(t, pool, `DROP INDEX idx_embed_failures_block`)
	seedLog := func(from, to int) {
		m191Exec(t, pool, `INSERT INTO context_write_log (decision, similarity, scope, block_title, block_category, metadata)
			SELECT 'clean', 0.8, 'private', 'm191 write-log title number ' || g, 'learnings', '{"threshold_duplicate": 0.92}'
			  FROM generate_series($1::int, $2::int) g`, from, to)
	}
	seedLog(1, 2000)
	m191Exec(t, pool, `ANALYZE context_write_log`)
	seedLog(2001, 60000)
	t.Logf("172 write log: the copied query %d, ctx_migration_rows %d (threshold 30000)",
		m191Old(t, pool, "public.context_write_log", 30000), m191Rows(t, pool, "public.context_write_log", 30000))
	old172, err := migrations.FS.ReadFile("172_block_fk_lookup_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	scale172 := []string{", 3000000)", ", 30000)", " 500000)", " 10000)"}
	warnings, _ = m191Apply(t, dsn, m191Scaled(t, string(old172), scale172...))
	if !m191IndexExists(t, pool, "idx_write_log_matched_block") {
		t.Fatalf("172 as shipped: no inline build of the write-log index (warnings %v); the issue does not reproduce", warnings)
	}
	t.Logf("172 as shipped: built idx_write_log_matched_block inline at 60000 rows (threshold 30000), warnings %v", warnings)
	m191Exec(t, pool, `DROP INDEX idx_write_log_matched_block`)
	m191Exec(t, pool, `DROP INDEX idx_embed_failures_block`)
	warnings, _ = m191Apply(t, dsn, m191Scaled(t, m191Rebuilt(t, "172_block_fk_lookup_indexes.sql", m191Call), scale172...))
	if m191IndexExists(t, pool, "idx_write_log_matched_block") {
		t.Errorf("172 with ctx_migration_rows: built the write-log index inline at 60000 rows (threshold 30000)")
	}
	if !m191IndexExists(t, pool, "idx_embed_failures_block") {
		t.Errorf("172 with ctx_migration_rows: the empty embed-failure table got no inline build")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "idx_write_log_matched_block is missing at") {
		t.Errorf("172 with ctx_migration_rows: warnings %v, want the one of the write-log index", warnings)
	}
	t.Logf("172 with ctx_migration_rows: %v", warnings)
}

// TestMigration191MeasureIssue399 is the measurement of the issue at full
// size (not a gate): context_access_tokens analysed at 100 000 rows and
// grown to 3 000 000 with autovacuum off, then file 184 as shipped and as
// rebuilt on ctx_migration_rows, each in one transaction beside a token
// writer on a second connection. Lines with numbers start with MEASURE|.
func TestMigration191MeasureIssue399(t *testing.T) {
	if os.Getenv("CTX_M191_MEASURE") == "" {
		t.Skip("measurement; set CTX_M191_MEASURE=1")
	}
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 183)
	body191, err := migrations.FS.ReadFile(m191File)
	if err != nil {
		t.Fatal(err)
	}
	m191Exec(t, pool, string(body191))
	keyID, principal := m191Key(t, pool)
	for _, tbl := range []string{"context_access_tokens", "context_web_sessions"} {
		m191Exec(t, pool, `ALTER TABLE `+tbl+` SET (autovacuum_enabled = false)`)
	}
	t0 := time.Now()
	m191SeedTokens(t, pool, keyID, principal, 1, 100000)
	m191Exec(t, pool, `ANALYZE context_access_tokens`)
	m191Exec(t, pool, `ANALYZE context_web_sessions`)
	for from := 100001; from <= 3000000; from += 500000 {
		to := from + 499999
		if to > 3000000 {
			to = 3000000
		}
		m191SeedTokens(t, pool, keyID, principal, from, to)
	}
	var rt float64
	var rp int32
	var size int64
	if err := pool.QueryRow(ctx, `SELECT reltuples, relpages, pg_relation_size(oid) FROM pg_class WHERE oid = 'context_access_tokens'::regclass`).Scan(&rt, &rp, &size); err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURE|seed|rows=3000000|reltuples=%.0f|relpages=%d|pg_relation_size=%d|took=%s|load=%s", rt, rp, size, time.Since(t0).Round(time.Second), mwLoad())
	t.Logf("MEASURE|decision|the copied query=%d|ctx_migration_rows=%d|threshold=500000",
		m191Old(t, pool, "public.context_access_tokens", 500000), m191Rows(t, pool, "public.context_access_tokens", 500000))
	var fnTook time.Duration
	for i := 0; i < 5; i++ {
		t1 := time.Now()
		m191Rows(t, pool, "public.context_access_tokens", 500000)
		if d := time.Since(t1); i == 0 || d < fnTook {
			fnTook = d
		}
	}
	t.Logf("MEASURE|ctx_migration_rows call|best of 5=%s", fnTook)
	// The count the function runs below a threshold: at most that many rows.
	for _, limit := range []int{500000, 1000000} {
		var best time.Duration
		for i := 0; i < 3; i++ {
			var n int64
			t1 := time.Now()
			if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 FROM public.context_access_tokens LIMIT %d) t`, limit)).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(t1); i == 0 || d < best {
				best = d
			}
		}
		t.Logf("MEASURE|bounded count|limit=%d|best of 3=%s|load=%s", limit, best.Round(time.Millisecond), mwLoad())
	}

	writer := func(i int) (string, []any) {
		tag := fmt.Sprintf("m191-w-%d-%d", time.Now().UnixNano(), i)
		return `INSERT INTO context_access_tokens (token_hash, token_type, api_key_id, principal_id, audiences, refresh_family, issued_via, expires_at)
			VALUES (encode(sha256(convert_to($1, 'UTF8')), 'hex'), 'access', $2::uuid, $3::uuid, ARRAY['ctx-mcp'], gen_random_uuid(), 'oauth', now() + interval '1 hour')`,
			[]any{tag, keyID, principal}
	}
	run := func(label, body string) {
		l := mwStart(t, pool, writer)
		time.Sleep(200 * time.Millisecond)
		warnings, took := m191Apply(t, dsn, body)
		time.Sleep(200 * time.Millisecond)
		wait, n := l.end()
		t.Logf("MEASURE|%s|file=%s|writer_longest_wait=%s|writer_statements=%d|idx_access_tokens_parent=%v|warnings=%q|load=%s",
			label, took.Round(time.Millisecond), wait.Round(time.Millisecond), n, m191IndexExists(t, pool, "idx_access_tokens_parent"), warnings, mwLoad())
	}
	old184, err := migrations.FS.ReadFile("184_access_token_fk_lookup_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	run("184 as shipped", string(old184))
	m191Exec(t, pool, `DROP INDEX IF EXISTS idx_access_tokens_parent`)
	m191Exec(t, pool, `DROP INDEX IF EXISTS idx_web_sessions_access_token`)
	run("184 with ctx_migration_rows", m191Rebuilt(t, "184_access_token_fk_lookup_indexes.sql", m191Call))
}

// TestMigration191MeasureDeadHeap measures (not a gate) a heap of 10 000 000
// rows of which 5 are live: the function's count, which reads the heap until
// it has the threshold's rows or reaches the end, and an inline build over
// the same heap before and after a VACUUM (the 5 live rows sit at the end of
// the heap, so VACUUM cannot truncate it). Lines with numbers start with
// MEASURE|.
func TestMigration191MeasureDeadHeap(t *testing.T) {
	if os.Getenv("CTX_M191_MEASURE") == "" {
		t.Skip("measurement; set CTX_M191_MEASURE=1")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	m191Table(t, pool, "m191_deadheap")
	t0 := time.Now()
	for from := 1; from <= 10000000; from += 2000000 {
		m191Fill(t, pool, "m191_deadheap", from, from+1999999)
	}
	m191ExecFlushed(t, pool, `DELETE FROM m191_deadheap WHERE id % 2000000 <> 0`)
	m191Exec(t, pool, `ANALYZE m191_deadheap`)
	rt, rp := m191Stats(t, pool, "10 000 000 rows, 5 live, analysed", "m191_deadheap")
	var size int64
	if err := pool.QueryRow(ctx, `SELECT pg_relation_size('m191_deadheap')`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURE|dead heap seed|rows=10000000|live=5|reltuples=%.0f|relpages=%d|heap_mb=%d|took=%s|load=%s", rt, rp, size>>20, time.Since(t0).Round(time.Second), mwLoad())
	timed := func(label string) {
		for i := 1; i <= 2; i++ {
			t1 := time.Now()
			got := m191Rows(t, pool, "m191_deadheap", 500000)
			t.Logf("MEASURE|%s|call=%d|ctx_migration_rows=%d|took=%s|load=%s", label, i, got, time.Since(t1).Round(time.Millisecond), mwLoad())
		}
		took, _ := mwTimedTx(t, pool, []string{`CREATE INDEX m191_deadheap_idx ON m191_deadheap (id)`})
		t.Logf("MEASURE|%s|inline CREATE INDEX (SHARE to the commit)=%s|load=%s", label, took, mwLoad())
		m191Exec(t, pool, `DROP INDEX m191_deadheap_idx`)
	}
	timed("dead heap, before VACUUM")
	t2 := time.Now()
	m191ExecFlushed(t, pool, `VACUUM m191_deadheap`)
	if err := pool.QueryRow(ctx, `SELECT pg_relation_size('m191_deadheap')`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURE|VACUUM|took=%s|heap_mb_after=%d|load=%s", time.Since(t2).Round(time.Second), size>>20, mwLoad())
	timed("dead heap, after VACUUM")
}
