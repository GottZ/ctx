//go:build integration

package store_test

// The write-cost measurement behind migration 195 (issue #237, not a gate):
// on a store of CTX_I237_ROWS rows (default 1000000, grown as in
// TestBrowseIndexMeasure), rounds alternate between the two indexes of 195
// standing and both dropped, and each round runs, through the production
// write paths, 1 000 inserts (store.UpsertBlock), 1 000 updates that change
// a block's content and so its updated_at (store.UpdateBlock), 1 000 vector
// writes (store.StoreEmbedding) and 1 000 dream claims (dream.PickBlock,
// which writes dream_cooldown_until alone). Per path it logs the WAL bytes
// and the time per write and how many of the updates were HOT. Then it
// updates the updated_at of 2 000 rows 100 times each (200 000 updates) and
// logs the size of the two indexes before, after and after a VACUUM. Set
// CTX_I237_WRITE_MEASURE=1.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBrowseIndexWriteMeasure(t *testing.T) {
	if os.Getenv("CTX_I237_WRITE_MEASURE") == "" {
		t.Skip("measurement; set CTX_I237_WRITE_MEASURE=1")
	}
	ctx := context.Background()
	rows := 1000000
	if v := os.Getenv("CTX_I237_ROWS"); v != "" {
		if _, err := fmt.Sscan(v, &rows); err != nil {
			t.Fatal(err)
		}
	}
	pool := testdb.SetupTestDB(t)
	i237Seed(t, pool, 0, rows)
	idsOf := func(sql string, args ...any) []string {
		t.Helper()
		r, err := pool.Query(ctx, sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(r, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// The rows the update paths write: knowledge blocks of the big scope, a
	// fresh set per round and variant.
	pick := idsOf(`SELECT id::text FROM context_blocks WHERE scope = 'i237-big' AND NOT is_archived AND type_name = 'knowledge'
		ORDER BY id LIMIT 12000`)
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = float32(i%7) / 7
	}
	stats := func() (upd, hot int64, lsn string) {
		t.Helper()
		// The statistics of the other connections reach the shared view at
		// their next flush (at most a second after their transaction).
		time.Sleep(1200 * time.Millisecond)
		if _, err := pool.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT n_tup_upd, n_tup_hot_upd, pg_current_wal_lsn()::text FROM pg_stat_user_tables WHERE relid = 'context_blocks'::regclass`).Scan(&upd, &hot, &lsn); err != nil {
			t.Fatal(err)
		}
		return
	}
	walSince := func(lsn string) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `SELECT pg_current_wal_lsn() - $1::pg_lsn`, lsn).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	set := func(with bool) {
		t.Helper()
		for _, ix := range m195Indexes {
			m182Exec(t, pool, `DROP INDEX IF EXISTS `+ix.name)
			if with {
				m182Exec(t, pool, "CREATE INDEX "+ix.ddl[len("CREATE INDEX CONCURRENTLY "):])
			}
		}
		m182Exec(t, pool, `CHECKPOINT`)
	}
	const n = 1000
	next := 0
	for round := 1; round <= 3; round++ {
		for _, with := range []bool{true, false} {
			set(with)
			ids := pick[next : next+n]
			next += n
			measure := func(path string, f func(i int) error) {
				t.Helper()
				upd0, hot0, lsn := stats()
				t0 := time.Now()
				for i := 0; i < n; i++ {
					if err := f(i); err != nil {
						t.Fatalf("%s %d: %v", path, i, err)
					}
				}
				took := time.Since(t0)
				wal := walSince(lsn)
				upd1, hot1, _ := stats()
				var load string
				if b, err := os.ReadFile("/proc/loadavg"); err == nil {
					load = strings.Join(strings.Fields(string(b))[:3], " ")
				}
				t.Logf("MEASURE-WRITE|rows=%d|round=%d|indexes=%v|path=%s|wal_per_write=%d|ms_per_write=%.3f|updates=%d|hot=%d|load=%s",
					rows, round, with, path, wal/n, float64(took.Microseconds())/1000/n, upd1-upd0, hot1-hot0, load)
			}
			measure("insert UpsertBlock", func(i int) error {
				_, err := store.UpsertBlock(ctx, pool, "i237w", fmt.Sprintf("i237 write %d %v %d", round, with, i),
					"written by the write measurement "+fmt.Sprint(i), []string{"note"}, nil, "i237-w", true, store.SensitivityWrite{}, "knowledge")
				return err
			})
			measure("update UpdateBlock (content, updated_at)", func(i int) error {
				c := fmt.Sprintf("changed in round %d %v", round, with)
				_, _, err := store.UpdateBlock(ctx, pool, ids[i], store.UpdateBlockData{Content: &c}, []string{"i237-big"})
				return err
			})
			measure("vector StoreEmbedding", func(i int) error {
				return store.StoreEmbedding(ctx, pool, ids[i], "i237-model", vec)
			})
			measure("dream claim PickBlock (dream_cooldown_until)", func(int) error {
				b, err := dream.PickBlock(ctx, pool, []string{"knowledge"}, []string{"i237-big"}, time.Hour)
				if err == nil && b == nil {
					return fmt.Errorf("nothing to claim")
				}
				return err
			})
		}
	}

	// Bloat: 200 000 updates of the updated_at of the same 2 000 rows.
	set(true)
	size := func() string {
		t.Helper()
		var parts []string
		for _, ix := range m195Indexes {
			var b int64
			if err := pool.QueryRow(ctx, `SELECT pg_relation_size($1::regclass)`, ix.name).Scan(&b); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, fmt.Sprintf("%s=%.1fMB", ix.name, float64(b)/(1<<20)))
		}
		return strings.Join(parts, "|")
	}
	same := pick[next : next+2000]
	t.Logf("MEASURE-BLOAT|rows=%d|before|%s", rows, size())
	t0 := time.Now()
	for r := 0; r < 100; r++ {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET updated_at = clock_timestamp() WHERE id = ANY($1::uuid[])`, same); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("MEASURE-BLOAT|rows=%d|after 200000 updates in %s|%s", rows, time.Since(t0).Round(time.Second), size())
	m182Exec(t, pool, `VACUUM context_blocks`)
	t.Logf("MEASURE-BLOAT|rows=%d|after vacuum|%s", rows, size())
}
