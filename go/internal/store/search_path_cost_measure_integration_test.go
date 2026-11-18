//go:build integration

package store_test

// The price of migration 179 on the write path of context_blocks: the seven
// trigger functions of the table that it pins, with and without the SET
// clause, on one database, alternating in blocks. Measurement, not a gate.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// blockTriggerFunctions are the functions migration 179 pins that fire on a
// write of context_blocks.
var blockTriggerFunctions = []string{
	"context_blocks_embed_excluded()", "context_blocks_retype()",
	"context_block_counts_ins()", "context_block_counts_upd()", "context_block_counts_del()",
	"context_block_counts_trunc()", "mark_guard_dirty()",
}

var executionTime = regexp.MustCompile(`Execution Time: ([0-9.]+) ms`)

func TestFunctionSearchPathCost(t *testing.T) {
	if os.Getenv("CTX_SEARCHPATH_MEASURE") == "" {
		t.Skip("measurement; set CTX_SEARCHPATH_MEASURE=1")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const seed = 10000
	if _, err := pool.Exec(ctx, `
INSERT INTO context_blocks (category, title, content, scope, type_name)
SELECT 'm', 'm ' || g, repeat('wort ' || g || ' ', 60), 'm-a', CASE WHEN g % 2 = 0 THEN 'knowledge' ELSE 'checkpoint' END
  FROM generate_series(1, $1) g`, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, is_archived)
VALUES ('m', 'a 1', repeat('alt ', 150), 'm-a', 'checkpoint', true)`); err != nil {
		t.Fatalf("seed archived: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	setPinned := func(pinned bool) {
		for _, f := range blockTriggerFunctions {
			s := `ALTER FUNCTION ` + f + ` RESET search_path`
			if pinned {
				s = `ALTER FUNCTION ` + f + ` SET search_path = pg_catalog, public, pg_temp`
			}
			if _, err := pool.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	// explain runs one statement under EXPLAIN ANALYZE in a transaction it
	// rolls back, so every run sees the same table, and returns the
	// execution time (triggers included).
	explain := func(stmt string) float64 {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF) `+stmt, pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		var ms float64
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if m := executionTime.FindStringSubmatch(line); m != nil {
				ms, _ = strconv.ParseFloat(m[1], 64)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		return ms
	}
	cases := []struct {
		name, stmt string
		per        int
	}{
		{"insert one knowledge block", `INSERT INTO context_blocks (category, title, content, scope, type_name) VALUES ('m', 'x', repeat('neu ', 150), 'm-a', 'knowledge')`, 60},
		{"insert one checkpoint block", `INSERT INTO context_blocks (category, title, content, scope, type_name) VALUES ('m', 'y', repeat('neu ', 150), 'm-a', 'checkpoint')`, 60},
		{"update metadata of one block", `UPDATE context_blocks SET metadata = metadata || '{"k":1}' WHERE id = (SELECT id FROM context_blocks WHERE title = 'm 4')`, 60},
		{"archive one block", `UPDATE context_blocks SET is_archived = true WHERE id = (SELECT id FROM context_blocks WHERE title = 'm 6')`, 60},
		{"unarchive one checkpoint", `UPDATE context_blocks SET is_archived = false WHERE id = (SELECT id FROM context_blocks WHERE title = 'a 1')`, 60},
		{"archive 5000 blocks", `UPDATE context_blocks SET is_archived = true WHERE scope = 'm-a' AND title LIKE 'm 1%'`, 4},
		{"insert 5000 blocks", `INSERT INTO context_blocks (category, title, content, scope, type_name) SELECT 'm', 'n ' || g, repeat('neu ' || g || ' ', 60), 'm-a', 'knowledge' FROM generate_series(1, 5000) g`, 4},
	}
	const blocks = 8
	for _, c := range cases {
		ms := map[bool][]float64{}
		for b := 0; b < blocks; b++ {
			for _, pinned := range []bool{false, true} {
				setPinned(pinned)
				for w := 0; w < 3; w++ {
					explain(c.stmt)
				}
				for i := 0; i < c.per; i++ {
					ms[pinned] = append(ms[pinned], explain(c.stmt))
				}
			}
		}
		med := func(v []float64) float64 {
			sort.Float64s(v)
			return v[len(v)/2]
		}
		u, p := med(ms[false]), med(ms[true])
		t.Logf("MEASURE %-30s unpinned %.3f ms  pinned %.3f ms  delta %+.3f ms %+.1f %%  (n=%d each)", c.name, u, p, p-u, (p-u)/u*100, len(ms[true]))
	}
	// The production write path: one UpsertBlock call (its own transaction,
	// several statements), new block each call, alternating in blocks.
	for _, typ := range []string{"knowledge", "checkpoint"} {
		ms := map[bool][]float64{}
		n := 0
		for b := 0; b < blocks; b++ {
			for _, pinned := range []bool{false, true} {
				setPinned(pinned)
				for i := 0; i < 63; i++ {
					n++
					t0 := time.Now()
					if _, err := store.UpsertBlock(ctx, pool, "m", fmt.Sprintf("store %s %d", typ, n), "body of a store write "+strings.Repeat("wort ", 110), []string{"t"}, map[string]any{"k": n}, "m-a", true, store.SensitivityWrite{}, typ); err != nil {
						t.Fatalf("upsert: %v", err)
					}
					if i >= 3 {
						ms[pinned] = append(ms[pinned], float64(time.Since(t0).Microseconds())/1000)
					}
				}
			}
		}
		med := func(v []float64) float64 {
			sort.Float64s(v)
			return v[len(v)/2]
		}
		u, p := med(ms[false]), med(ms[true])
		t.Logf("MEASURE store.UpsertBlock %-12s unpinned %.3f ms  pinned %.3f ms  delta %+.3f ms %+.1f %%  (n=%d each)", typ, u, p, p-u, (p-u)/u*100, len(ms[true]))
	}
	setPinned(true)
}
