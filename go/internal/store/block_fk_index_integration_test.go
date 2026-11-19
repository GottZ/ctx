//go:build integration

package store_test

// Deleting a block costs one lookup per foreign key that points at
// context_blocks (issue #255). These tests pin that each lookup is an index
// probe, so a block delete costs the same however long the write log or any
// other referencing table grows:
//
//   - TestBlockForeignKeysHaveAServingIndex reads the catalog of a fresh
//     chain and fails on every such key without a serving index, and on
//     every such key that lives on a TimescaleDB hypertable;
//   - TestBlockDeleteCostIsFlat deletes blocks next to 10 000 and 100 000
//     write-log and embed-failure rows and compares the rows the delete reads
//     per block.
//
//	go test -tags=integration ./internal/store/ -run 'TestBlockForeignKeys|TestBlockDeleteCost' -count=1 -v

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// fkInventorySQL lists every foreign key of the database, TimescaleDB chunk
// copies included, with the table it points at (ref) and the indexes that
// serve its lookup. Deleting a referenced row runs one lookup per such key on
// the referencing table (`WHERE col = $1`, whatever the ON DELETE action:
// CASCADE deletes, SET NULL updates, NO ACTION and RESTRICT check). The block
// gate below reads the keys on context_blocks, the delete-action gate
// (fk_delete_action_index_integration_test.go, issue #277) every key whose
// delete action writes the referencing table. Without an index whose leading
// columns are the key's columns that lookup is a scan of the referencing
// table, once per deleted block. A partial index serves it only when its
// predicate follows from `col = $1`, which this query accepts in exactly one
// form: `col IS NOT NULL` on the key's single column. Only valid btree and
// hash indexes count (both probe `=`); the leading columns are key columns
// (indnkeyatts: an INCLUDE column is stored, not searched), and an expression
// column (attnum 0 in indkey) never matches.
//
// hypertable is true for a key on a TimescaleDB hypertable or on one of its
// chunks. TimescaleDB copies such a key onto every chunk, each copy with its
// own pair of triggers on context_blocks, so a block delete probes every
// chunk: measured for issue #255 with an index on every chunk, 1.2 ms per
// deleted block at 10 chunks, 7.2 ms at 100, 19.6 ms at 400. No index makes
// that constant, so the gate refuses the key itself.
const fkInventorySQL = `
SELECT c.conname,
       c.confrelid::regclass::text AS ref,
       c.conrelid::regclass::text AS tbl,
       (SELECT string_agg(a.attname, ', ' ORDER BY k.ord)
          FROM unnest(c.conkey) WITH ORDINALITY k(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum) AS cols,
       c.confdeltype::text AS on_delete,
       COALESCE((
         SELECT string_agg(i.indexrelid::regclass::text, ', ' ORDER BY i.indexrelid::regclass::text)
           FROM pg_index i
           JOIN pg_class ic ON ic.oid = i.indexrelid
           JOIN pg_am am    ON am.oid = ic.relam
          WHERE i.indrelid = c.conrelid
            AND i.indisvalid
            AND am.amname IN ('btree', 'hash')
            AND i.indnkeyatts >= cardinality(c.conkey)
            AND (i.indkey::int2[])[0:cardinality(c.conkey) - 1] @> c.conkey
            AND (i.indkey::int2[])[0:cardinality(c.conkey) - 1] <@ c.conkey
            AND (i.indpred IS NULL
                 OR (cardinality(c.conkey) = 1
                     AND pg_get_expr(i.indpred, i.indrelid) = format('(%I IS NOT NULL)',
                         (SELECT a.attname FROM pg_attribute a
                           WHERE a.attrelid = c.conrelid AND a.attnum = c.conkey[1]))))
       ), '') AS serving,
       (n.nspname = '_timescaledb_internal'
        OR EXISTS (SELECT 1 FROM timescaledb_information.hypertables h
                    WHERE h.hypertable_schema = n.nspname AND h.hypertable_name = r.relname)) AS hypertable
  FROM pg_constraint c
  JOIN pg_class      r ON r.oid = c.conrelid
  JOIN pg_namespace  n ON n.oid = r.relnamespace
 WHERE c.contype = 'f'
 ORDER BY ref, tbl, cols, c.conname`

type blockFK struct {
	name, ref, table, cols, onDelete, serving string
	hypertable                                bool
}

func (f blockFK) String() string {
	action := map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}[f.onDelete]
	serving := f.serving
	if serving == "" {
		serving = "-"
	}
	return fmt.Sprintf("%s(%s) %s [%s] served by %s", f.table, f.cols, action, f.name, serving)
}

// readBlockFKs is the inventory of the keys that point at context_blocks.
func readBlockFKs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []blockFK {
	t.Helper()
	var out []blockFK
	for _, f := range readFKs(t, ctx, pool) {
		if f.ref == "context_blocks" {
			out = append(out, f)
		}
	}
	return out
}

// readFKs is the inventory of every foreign key (fkInventorySQL).
func readFKs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []blockFK {
	t.Helper()
	rows, err := pool.Query(ctx, fkInventorySQL)
	if err != nil {
		t.Fatalf("fk inventory: %v", err)
	}
	defer rows.Close()
	var fks []blockFK
	for rows.Next() {
		var f blockFK
		if err := rows.Scan(&f.name, &f.ref, &f.table, &f.cols, &f.onDelete, &f.serving, &f.hypertable); err != nil {
			t.Fatalf("fk inventory: %v", err)
		}
		fks = append(fks, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("fk inventory: %v", err)
	}
	return fks
}

// blockFKViolations is what the gate refuses in an inventory.
func blockFKViolations(fks []blockFK) []string {
	var out []string
	for _, f := range fks {
		switch {
		case f.hypertable:
			out = append(out, f.String()+": a key on a hypertable is copied onto every chunk, and a block delete probes each copy")
		case f.serving == "":
			out = append(out, f.String()+": no index serves the lookup, a block delete scans the table")
		}
	}
	return out
}

// TestBlockForeignKeysHaveAServingIndex is the catalog gate of issue #255:
// every foreign key that points at context_blocks has an index that serves
// its lookup, on a plain table. A new key without one fails here with the
// table and column; the fix is an index in the same migration (for a table
// that can be large, by the pattern of 172_block_fk_lookup_indexes.sql).
func TestBlockForeignKeysHaveAServingIndex(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh_chain", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		fks := readBlockFKs(t, ctx, pool)
		var b strings.Builder
		for _, f := range fks {
			b.WriteString("  " + f.String() + "\n")
		}
		t.Logf("%d foreign keys point at context_blocks:\n%s", len(fks), b.String())
		// The inventory must see the keys the issue names, so a query that
		// matches nothing cannot pass.
		for _, want := range []string{"context_write_log(matched_block_id)", "context_embed_failures(block_id)", "context_blocks(parent_id)"} {
			found := false
			for _, f := range fks {
				found = found || f.table+"("+f.cols+")" == want
			}
			if !found {
				t.Errorf("the inventory does not list %s", want)
			}
		}
		for _, v := range blockFKViolations(fks) {
			t.Errorf("%s", v)
		}
	})

	// The gate's classification on keys made for it: which index serves a
	// key, which does not, and that a hypertable key is refused with its
	// chunk copies listed.
	t.Run("classification", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		for _, st := range []string{
			`CREATE TABLE gate_none (b uuid REFERENCES context_blocks(id) ON DELETE SET NULL)`,
			`CREATE TABLE gate_full (b uuid REFERENCES context_blocks(id) ON DELETE CASCADE)`,
			`CREATE INDEX gate_full_b ON gate_full (b)`,
			`CREATE TABLE gate_notnull (b uuid REFERENCES context_blocks(id) ON DELETE SET NULL)`,
			`CREATE INDEX gate_notnull_b ON gate_notnull (b) WHERE b IS NOT NULL`,
			`CREATE TABLE gate_otherpred (b uuid REFERENCES context_blocks(id), x int)`,
			`CREATE INDEX gate_otherpred_b ON gate_otherpred (b) WHERE x IS NULL`,
			`CREATE TABLE gate_second (a int, b uuid REFERENCES context_blocks(id))`,
			`CREATE INDEX gate_second_ab ON gate_second (a, b)`,
			`CREATE TABLE gate_leading (b uuid REFERENCES context_blocks(id), a int)`,
			`CREATE INDEX gate_leading_ba ON gate_leading (b, a)`,
			`CREATE TABLE gate_brin (b uuid REFERENCES context_blocks(id))`,
			`CREATE INDEX gate_brin_b ON gate_brin USING brin (b)`,
			`CREATE TABLE gate_hash (b uuid REFERENCES context_blocks(id))`,
			`CREATE INDEX gate_hash_b ON gate_hash USING hash (b)`,
			`CREATE TABLE gate_expr (b uuid REFERENCES context_blocks(id))`,
			`CREATE INDEX gate_expr_b ON gate_expr ((b::text))`,
			`CREATE TABLE gate_invalid (b uuid REFERENCES context_blocks(id))`,
			`CREATE INDEX gate_invalid_b ON gate_invalid (b)`,
			// A key of two columns needs a unique pair on context_blocks; the
			// test database is this subtest's own.
			`ALTER TABLE context_blocks ADD CONSTRAINT gate_id_scope UNIQUE (id, scope)`,
			`CREATE TABLE gate_pair (b uuid, s varchar(50), FOREIGN KEY (b, s) REFERENCES context_blocks(id, scope))`,
			`CREATE INDEX gate_pair_sb ON gate_pair (s, b)`,
			`CREATE TABLE gate_pair_include (b uuid, s varchar(50), FOREIGN KEY (b, s) REFERENCES context_blocks(id, scope))`,
			`CREATE INDEX gate_pair_include_b ON gate_pair_include (b) INCLUDE (s)`,
			`CREATE TABLE gate_ht (ts timestamptz NOT NULL, b uuid REFERENCES context_blocks(id) ON DELETE SET NULL)`,
			`SELECT create_hypertable('gate_ht', 'ts', chunk_time_interval => interval '1 day')`,
			`CREATE INDEX gate_ht_b ON gate_ht (b) WHERE b IS NOT NULL`,
			`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'gate', 'gate', 'private')`,
			`INSERT INTO gate_ht SELECT '2026-01-01'::timestamptz + g * interval '1 day', (SELECT id FROM context_blocks WHERE title = 'gate') FROM generate_series(0, 2) g`,
		} {
			if _, err := pool.Exec(ctx, st); err != nil {
				t.Fatalf("%s: %v", st, err)
			}
		}
		// An INVALID index is what an aborted CREATE INDEX CONCURRENTLY
		// leaves behind; flagging one by hand needs a superuser.
		invalid := true
		if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'gate_invalid_b'::regclass`); err != nil {
			t.Logf("cannot flag gate_invalid_b INVALID on this cluster (%v): that case stays unproven here", err)
			invalid = false
		}
		got := map[string]string{}
		chunks := 0
		for _, f := range readBlockFKs(t, ctx, pool) {
			if !strings.Contains(f.table, "gate_") && !strings.Contains(f.table, "_timescaledb_internal.") {
				continue
			}
			if strings.HasPrefix(f.table, "_timescaledb_internal.") {
				chunks++
				if !f.hypertable || f.serving == "" {
					t.Errorf("chunk key %s: hypertable=%v serving=%q, want a hypertable key served by the chunk's copy of gate_ht_b", f, f.hypertable, f.serving)
				}
				continue
			}
			verdict := "served"
			if v := blockFKViolations([]blockFK{f}); len(v) > 0 {
				verdict = "refused"
			}
			got[f.table] = verdict
		}
		want := map[string]string{
			"gate_none":         "refused", // no index
			"gate_full":         "served",  // btree (b)
			"gate_notnull":      "served",  // btree (b) WHERE b IS NOT NULL
			"gate_otherpred":    "refused", // a predicate `b = $1` does not imply
			"gate_second":       "refused", // b is not the leading column
			"gate_leading":      "served",  // b leads, the rest is extra
			"gate_brin":         "refused", // brin answers `=` by reading whole block ranges
			"gate_hash":         "served",  // hash (b)
			"gate_expr":         "refused", // an expression of b is not b
			"gate_pair":         "served",  // both key columns lead, in any order
			"gate_pair_include": "refused", // s is an INCLUDE column, not searched
			"gate_ht":           "refused", // hypertable parent
		}
		if invalid {
			want["gate_invalid"] = "refused" // INVALID index
		}
		for tbl, w := range want {
			if got[tbl] != w {
				t.Errorf("%s: %q, want %q", tbl, got[tbl], w)
			}
		}
		if chunks != 3 {
			t.Errorf("%d chunk copies of gate_ht's key in the inventory, want 3 (one per chunk)", chunks)
		}
	})
}

// blockDeleteCostBlocks is the number of blocks each measured delete takes.
const blockDeleteCostBlocks = 50

// TestBlockDeleteCostIsFlat deletes the same number of blocks next to 10 000
// and next to 100 000 rows in context_write_log and context_embed_failures,
// with the statement the scope purge runs (scope_purge.go), and compares the
// rows the delete reads from each referencing table per deleted block. The
// foreign-key lookups run as cached plans of the backend; each delete runs
// once under the generic plan (what a long-lived connection runs after five
// executions) and once under the custom plan. Without a serving index a
// lookup reads the whole table, once per block: 10 000 rows per block on the
// small fixture, 100 000 on the large one.
//
// The rows are seeded in bulk with the columns the guard's audit insert
// writes (guard.writeAuditLog: block_id, matched_block_id set to the nearest
// candidate) and the embed backfill's failure memo writes; a write path per
// row would only measure the fixture.
func TestBlockDeleteCostIsFlat(t *testing.T) {
	ctx := context.Background()
	sizes := []int{10_000, 100_000}
	modes := []string{"force_generic_plan", "force_custom_plan"}
	// perBlock[mode][size][table] = rows read per deleted block.
	perBlock := map[string]map[int]map[string]float64{}
	for _, m := range modes {
		perBlock[m] = map[int]map[string]float64{}
	}
	for _, n := range sizes {
		pool := testdb.SetupTestDB(t)
		seedBlockDeleteCost(t, ctx, pool, n, modes)
		for _, m := range modes {
			perBlock[m][n] = deleteAndCount(t, ctx, pool, "del-"+m+"-", m)
		}
	}
	small, large := sizes[0], sizes[1]
	for _, m := range modes {
		for _, tbl := range []string{"context_write_log", "context_embed_failures"} {
			// The fixture gives every deleted block one row in each table, so
			// a delete that reads nothing measured nothing.
			if perBlock[m][small][tbl] < 1 {
				t.Errorf("%s: %s: %.1f rows read per deleted block at %d rows, want at least the block's own row — the measurement saw nothing",
					m, tbl, perBlock[m][small][tbl], small)
			}
		}
		for tbl, s := range perBlock[m][small] {
			l := perBlock[m][large][tbl]
			t.Logf("%s: %s: %.1f rows read per deleted block next to %d rows, %.1f next to %d", m, tbl, s, small, l, large)
			if l > 2*s+4 {
				t.Errorf("%s: %s: a block delete reads %.1f rows per block next to %d rows and %.1f next to %d — the lookup grows with the table",
					m, tbl, s, small, l, large)
			}
		}
	}
}

// seedBlockDeleteCost writes n blocks that stay, n write-log rows and n
// embed failures on them, and per mode blockDeleteCostBlocks blocks to
// delete, each with one write-log row (its own block_id and matched_block_id)
// and one embed failure.
func seedBlockDeleteCost(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int, modes []string) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	const batch = 10_000
	for off := 0; off < n; off += batch {
		exec(`INSERT INTO context_blocks (category, title, content, scope)
			SELECT 'learnings', 'keep-' || g, 'body ' || g, 'private' FROM generate_series($1::int + 1, $1::int + $2::int) g`, off, min(batch, n-off))
	}
	exec(`INSERT INTO context_write_log (block_id, matched_block_id, decision, similarity, scope, block_title, block_category)
		SELECT id, id, 'accept', 0.5, 'private', title, category FROM context_blocks WHERE title LIKE 'keep-%'`)
	exec(`INSERT INTO context_embed_failures (block_id, last_error, last_class, next_attempt_at)
		SELECT id, 'seed', 'wire', now() FROM context_blocks WHERE title LIKE 'keep-%'`)
	for _, m := range modes {
		prefix := "del-" + m + "-"
		exec(`INSERT INTO context_blocks (category, title, content, scope)
			SELECT 'learnings', $1 || g, 'body ' || g, 'private' FROM generate_series(1, $2::int) g`, prefix, blockDeleteCostBlocks)
		exec(`INSERT INTO context_write_log (block_id, matched_block_id, decision, similarity, scope, block_title, block_category)
			SELECT id, id, 'accept', 0.5, 'private', title, category FROM context_blocks WHERE title LIKE $1 || '%'`, prefix)
		exec(`INSERT INTO context_embed_failures (block_id, last_error, last_class, next_attempt_at)
			SELECT id, 'seed', 'wire', now() FROM context_blocks WHERE title LIKE $1 || '%'`, prefix)
	}
	exec(`ANALYZE context_blocks`)
	exec(`ANALYZE context_write_log`)
	exec(`ANALYZE context_embed_failures`)
}

// deleteAndCount deletes the blocks whose title starts with prefix in one
// transaction under the given plan_cache_mode and returns, per table that
// has a foreign key on context_blocks, the rows the delete read per deleted
// block: heap rows of sequential scans, heap rows fetched by index scans and
// index entries read (so a lookup that walks a whole index to filter in it
// counts too). The pg_stat_xact counters hold what this backend has not
// reported yet, earlier transactions included, so the count is the
// difference of two reads inside the transaction.
func deleteAndCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefix, mode string) map[string]float64 {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reads := func() map[string]int64 {
		rows, err := tx.Query(ctx, `
			SELECT s.relname, s.seq_tup_read + COALESCE(s.idx_tup_fetch, 0)
			       + COALESCE((SELECT sum(pg_stat_get_xact_tuples_returned(i.indexrelid))
			                     FROM pg_index i WHERE i.indrelid = s.relid), 0)
			  FROM pg_stat_xact_user_tables s
			 WHERE s.relid IN (SELECT conrelid FROM pg_constraint
			                    WHERE contype = 'f' AND confrelid = 'public.context_blocks'::regclass)`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var rel string
			var n int64
			if err := rows.Scan(&rel, &n); err != nil {
				t.Fatal(err)
			}
			out[rel] = n
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var ids []string
	if err := tx.QueryRow(ctx, `SELECT array_agg(id::text) FROM context_blocks WHERE title LIKE $1 || '%'`, prefix).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
		t.Fatal(err)
	}
	before := reads()
	tag, err := tx.Exec(ctx, `DELETE FROM context_blocks WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != blockDeleteCostBlocks {
		t.Fatalf("deleted %d blocks, want %d", tag.RowsAffected(), blockDeleteCostBlocks)
	}
	after := reads()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for rel, n := range after {
		out[rel] = float64(n-before[rel]) / blockDeleteCostBlocks
	}
	return out
}
