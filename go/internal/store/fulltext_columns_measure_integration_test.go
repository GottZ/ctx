//go:build integration

// Measurement of issue #343 (not a gate; the gates are in
// fulltext_vectors_integration_test.go): what an update that leaves title and
// content alone costs before migration 178 (ts_de / ts_en generated, every
// update recomputes them because the table has BEFORE ROW UPDATE triggers) and
// after it (ordinary columns kept by a trigger), on stores of the reference
// instance's shape — 57 % never-ranked checkpoint blocks of 35 000 characters,
// interleaved with ranked blocks of 500 to 6 000. Also the mechanism (a store
// at 173, before the first BEFORE ROW UPDATE trigger, and at 177 with the two
// triggers dropped or disabled), the dump size, and the one-row write path
// under eight concurrent writers.
//
//	CTX_FTSCOL_MEASURE=10000,50000 CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18 go test -tags=integration -p 1 ./internal/store/ -run TestFullTextColumnsMeasure -count=1 -v -timeout 3h
package store_test

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type ftsColSizes struct {
	heap, toast, indexes int64
}

func ftsMeasureSizes(t *testing.T, pool *pgxpool.Pool) ftsColSizes {
	t.Helper()
	var s ftsColSizes
	if err := pool.QueryRow(context.Background(), `
		SELECT pg_relation_size('context_blocks'),
		       pg_relation_size(c.reltoastrelid),
		       pg_indexes_size('context_blocks')
		  FROM pg_class c WHERE c.oid = 'context_blocks'::regclass`).Scan(&s.heap, &s.toast, &s.indexes); err != nil {
		t.Fatalf("sizes: %v", err)
	}
	return s
}

func (s ftsColSizes) String() string {
	return fmt.Sprintf("heap %.1f MB, toast %.1f MB, indexes %.1f MB", mb(s.heap), mb(s.toast), mb(s.indexes))
}

func mb(b int64) float64 { return float64(b) / 1e6 }

// ftsLogColumns logs the stored size of content and both vectors per class,
// and how the vectors are compressed.
func ftsLogColumns(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT CASE WHEN type_name IN ('checkpoint','system-meta') THEN 'never-ranked' ELSE 'ranked' END AS class,
		       count(*),
		       sum(pg_column_size(content))::bigint,
		       COALESCE(sum(pg_column_size(ts_de)), 0)::bigint,
		       COALESCE(sum(pg_column_size(ts_en)), 0)::bigint,
		       count(*) FILTER (WHERE pg_column_compression(ts_de) = 'pglz'),
		       count(*) FILTER (WHERE pg_column_compression(ts_de) = 'lz4'),
		       count(*) FILTER (WHERE ts_de IS NOT NULL AND pg_column_compression(ts_de) IS NULL),
		       count(*) FILTER (WHERE pg_column_toast_chunk_id(ts_de) IS NOT NULL),
		       COALESCE(sum(octet_length(ts_de::text)), 0)::bigint
		  FROM context_blocks WHERE NOT is_archived GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var class string
		var n, content, de, en, pglz, lz4, plain, toasted, deText int64
		if err := rows.Scan(&class, &n, &content, &de, &en, &pglz, &lz4, &plain, &toasted, &deText); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Logf("%s %-12s rows %6d  content %8.1f MB  ts_de %8.1f MB  ts_en %8.1f MB  ts_de pglz %d lz4 %d uncompressed %d toasted %d  ts_de as text %.1f MB",
			label, class, n, mb(content), mb(de), mb(en), pglz, lz4, plain, toasted, mb(deText))
	}
}

// ftsDumpEstimate is the gzip-6 size of the COPY text of the given columns —
// what pg_dump -Fc writes for the table data (it copies every column except
// generated ones and compresses with gzip at its default level).
func ftsDumpEstimate(t *testing.T, pool *pgxpool.Pool, cols string) int64 {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	cw := &countingWriter{}
	zw, _ := gzip.NewWriterLevel(cw, 6)
	if _, err := conn.Conn().PgConn().CopyTo(context.Background(), zw, "COPY (SELECT "+cols+" FROM context_blocks) TO STDOUT"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	_ = zw.Close()
	return cw.n
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

var _ io.Writer = (*countingWriter)(nil)

// dumpCols are the columns pg_dump copies today: every column but the
// generated ones (content_hash, ts_de, ts_en).
func ftsDumpCols(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var cols string
	if err := pool.QueryRow(context.Background(), `
		SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum)
		  FROM pg_attribute
		 WHERE attrelid = 'context_blocks'::regclass AND attnum > 0 AND NOT attisdropped AND attgenerated = ''`).Scan(&cols); err != nil {
		t.Fatalf("dump cols: %v", err)
	}
	return cols
}

type ftsOp struct {
	ms                       float64
	wal                      int64
	toastBefore, toastAfter  int64
	newDe, newEn, newContent int64
	rows                     int64
}

// ftsRunOp runs one statement after a CHECKPOINT and reports its time, the WAL
// it wrote, the TOAST growth, and how many rows got a new TOAST value for each
// vector and for content.
func ftsRunOp(t *testing.T, pool *pgxpool.Pool, sql string) ftsOp {
	t.Helper()
	ctx := context.Background()
	for _, s := range []string{
		`DROP TABLE IF EXISTS probe_toast_ids`,
		`CREATE UNLOGGED TABLE probe_toast_ids AS SELECT id, pg_column_toast_chunk_id(ts_de) de, pg_column_toast_chunk_id(ts_en) en, pg_column_toast_chunk_id(content) c FROM context_blocks`,
		`CHECKPOINT`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var r ftsOp
	r.toastBefore = ftsMeasureSizes(t, pool).toast
	var lsn0 string
	if err := pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn0); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	tag, err := pool.Exec(ctx, sql)
	if err != nil {
		t.Fatalf("op %s: %v", sql, err)
	}
	r.ms = float64(time.Since(t0).Microseconds()) / 1000
	r.rows = tag.RowsAffected()
	if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint`, lsn0).Scan(&r.wal); err != nil {
		t.Fatal(err)
	}
	r.toastAfter = ftsMeasureSizes(t, pool).toast
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE p.de IS NOT NULL AND pg_column_toast_chunk_id(b.ts_de) IS DISTINCT FROM p.de),
		       count(*) FILTER (WHERE p.en IS NOT NULL AND pg_column_toast_chunk_id(b.ts_en) IS DISTINCT FROM p.en),
		       count(*) FILTER (WHERE p.c IS NOT NULL AND pg_column_toast_chunk_id(b.content) IS DISTINCT FROM p.c)
		  FROM context_blocks b JOIN probe_toast_ids p USING (id)`).Scan(&r.newDe, &r.newEn, &r.newContent); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r ftsOp) String() string {
	return fmt.Sprintf("%6d rows %9.1f ms  WAL %8.1f MB  TOAST %.1f -> %.1f MB  new TOAST values ts_de %d ts_en %d content %d",
		r.rows, r.ms, mb(r.wal), mb(r.toastBefore), mb(r.toastAfter), r.newDe, r.newEn, r.newContent)
}

// ftsSingleRowMs is the median of n single-row updates of wide rows, one
// statement each (the per-block form of an archive or a metadata write).
func ftsSingleRowMs(t *testing.T, pool *pgxpool.Pool, sql string, n int) float64 {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE type_name = 'checkpoint' AND NOT is_archived ORDER BY id LIMIT $1`, n)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	ms := make([]float64, 0, len(ids))
	for _, id := range ids {
		t0 := time.Now()
		if _, err := pool.Exec(ctx, sql, id); err != nil {
			t.Fatalf("single row: %v", err)
		}
		ms = append(ms, float64(time.Since(t0).Microseconds())/1000)
	}
	sort.Float64s(ms)
	return ms[len(ms)/2]
}

func ftsExec(t *testing.T, pool *pgxpool.Pool, stmts ...string) float64 {
	t.Helper()
	t0 := time.Now()
	for _, s := range stmts {
		if _, err := pool.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return float64(time.Since(t0).Microseconds()) / 1000
}

// ftsConcurrentWrites runs the one-row write paths with eight concurrent
// writers — an insert of a new block, a metadata update and a content update
// of that block, each through the store — and logs the median per call and the
// calls per second of each kind.
func ftsConcurrentWrites(t *testing.T, pool *pgxpool.Pool, label string, width int) {
	t.Helper()
	ctx := context.Background()
	const writers, per = 8, 25
	kinds := []string{"insert", "metadata update", "content update"}
	ms := map[string][]float64{}
	wall := map[string]time.Duration{}
	var mu sync.Mutex
	ids := make([][]string, writers)
	for _, kind := range kinds {
		var wg sync.WaitGroup
		t0 := time.Now()
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < per; i++ {
					c0 := time.Now()
					var err error
					switch kind {
					case "insert":
						var b *store.Block
						b, err = store.UpsertBlock(ctx, pool, "writes", fmt.Sprintf("%s w%d i%d", label, w, i),
							unrankedText(uint64(w*1000+i), width, "writemarker", 1), nil, nil, "private", true, store.SensitivityWrite{}, "knowledge")
						if err == nil {
							ids[w] = append(ids[w], b.ID)
						}
					case "metadata update":
						_, _, err = store.UpdateBlock(ctx, pool, ids[w][i], store.UpdateBlockData{Metadata: map[string]any{"n": i}}, []string{"private"})
					default:
						c := unrankedText(uint64(w*1000+i+500000), width, "writemarker", 1)
						_, _, err = store.UpdateBlock(ctx, pool, ids[w][i], store.UpdateBlockData{Content: &c}, []string{"private"})
					}
					d := float64(time.Since(c0).Microseconds()) / 1000
					if err != nil {
						t.Errorf("%s: %v", kind, err)
						return
					}
					mu.Lock()
					ms[kind] = append(ms[kind], d)
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		wall[kind] = time.Since(t0)
	}
	for _, kind := range kinds {
		v := ms[kind]
		sort.Float64s(v)
		if len(v) == 0 {
			continue
		}
		t.Logf("%s %-16s width %5d: median %7.2f ms, p90 %7.2f ms, %6.0f calls/s",
			label, kind, width, v[len(v)/2], v[len(v)*9/10], float64(len(v))/wall[kind].Seconds())
	}
}

const (
	ftsArchiveSQL    = `UPDATE context_blocks SET is_archived = true, updated_at = now() WHERE type_name = 'checkpoint' AND NOT is_archived`
	ftsRankedMetaSQL = `UPDATE context_blocks SET metadata = metadata || '{"probe": 1}' WHERE type_name = 'knowledge'`
	ftsSingleMetaSQL = `UPDATE context_blocks SET metadata = metadata || '{"probe": 2}', updated_at = now() WHERE id = $1`
)

func TestFullTextColumnsMeasure(t *testing.T) {
	sizes := os.Getenv("CTX_FTSCOL_MEASURE")
	if testing.Short() || sizes == "" {
		t.Skip("measurement; set CTX_FTSCOL_MEASURE to the store sizes, e.g. 10000,50000")
	}
	ctx := context.Background()
	beforeTriggers := `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'context_blocks'::regclass AND NOT tgisinternal AND (tgtype & 2) = 2 AND (tgtype & 1) = 1 AND (tgtype & 16) = 16`

	t.Run("mechanism", func(t *testing.T) {
		shape := unrankedShape{ranked: 300, unranked: 400, unrankedWidth: 35000, interleaved: true}
		for _, c := range []struct {
			label string
			pool  *pgxpool.Pool
			prep  []string
		}{
			{"at 173", testdb.SetupTestDBUpTo(t, 173), nil},
			{"at 177", testdb.SetupTestDBUpTo(t, 177), nil},
			{"at 177, the two BEFORE triggers dropped", testdb.SetupTestDBUpTo(t, 177),
				[]string{`DROP TRIGGER trg_block_retype ON context_blocks`, `DROP TRIGGER trg_block_embed_excluded ON context_blocks`}},
			{"at 177, the two BEFORE triggers disabled", testdb.SetupTestDBUpTo(t, 177),
				[]string{`ALTER TABLE context_blocks DISABLE TRIGGER trg_block_retype`, `ALTER TABLE context_blocks DISABLE TRIGGER trg_block_embed_excluded`}},
			{"at 178", testdb.SetupTestDB(t), nil},
		} {
			if err := unrankedSeed(shape)(ctx, c.pool); err != nil {
				t.Fatal(err)
			}
			ftsExec(t, c.pool, c.prep...)
			var version string
			var n int
			_ = c.pool.QueryRow(ctx, `SELECT version()`).Scan(&version)
			_ = c.pool.QueryRow(ctx, beforeTriggers).Scan(&n)
			t.Logf("%s: %s, BEFORE ROW UPDATE triggers %d", c.label, version, n)
			t.Logf("%s archive:     %s", c.label, ftsRunOp(t, c.pool, ftsArchiveSQL))
			t.Logf("%s ranked meta: %s", c.label, ftsRunOp(t, c.pool, ftsRankedMetaSQL))
		}
	})

	for _, f := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 100 {
			t.Fatalf("size %q", f)
		}
		unranked := n * 57 / 100
		shape := unrankedShape{ranked: n - unranked, unranked: unranked, unrankedWidth: 35000, interleaved: true}
		label := fmt.Sprintf("n=%d", n)
		for _, side := range []string{"177", "178"} {
			t.Run(label+"/"+side, func(t *testing.T) {
				var pool *pgxpool.Pool
				if side == "177" {
					pool = testdb.SetupTestDBUpTo(t, 177)
					if err := unrankedSeed(shape)(ctx, pool); err != nil {
						t.Fatal(err)
					}
				} else {
					pool = testdb.SetupSeededTestDB(t, fmt.Sprintf("i343-measure-%d", n), unrankedSeed(shape))
				}
				l := label + " at " + side
				t.Logf("%s sizes: %s", l, ftsMeasureSizes(t, pool))
				ftsLogColumns(t, pool, l)
				t.Logf("%s dump estimate (COPY text of the dumped columns, gzip-6): %.1f MB", l, mb(ftsDumpEstimate(t, pool, ftsDumpCols(t, pool))))
				t.Logf("%s single-row metadata update of a wide block: median %.2f ms", l, ftsSingleRowMs(t, pool, ftsSingleMetaSQL, 200))
				ftsConcurrentWrites(t, pool, l, 600)
				ftsConcurrentWrites(t, pool, l, 35000)
				t.Logf("%s ranked meta: %s", l, ftsRunOp(t, pool, ftsRankedMetaSQL))
				t.Logf("%s archive:     %s", l, ftsRunOp(t, pool, ftsArchiveSQL))
			})
		}
	}
}
