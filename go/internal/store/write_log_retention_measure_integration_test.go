//go:build integration

package store_test

// Issue #277, measured for docs/operations.md ("Write-log retention"): what a
// batch of the write-log retention costs beside 200 000 / 1 000 000 /
// 3 000 000 rows, 30 % of them past the horizon (the shape the issue measured
// the proposal at), in custom and generic plan at both random_page_cost
// values; what a whole run costs; what the first batch of a second run reads
// when no vacuum ran between; and what guard-stats' count(*) of the write log
// reads at the size the live instance had (46 000 rows). Not a gate: it logs
// lines starting with "WLM|".
//
//	CTX_WRITELOG_MEASURE=200000,1000000,3000000 go test -tags=integration -p 1 -count=1 -timeout 3h \
//	    -run TestEvictWriteLogMeasure -v ./internal/store/

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type wlmCost struct {
	buffers, wal int64
	ms           float64
	nodes        string
}

func wlmExplain(t *testing.T, pool *pgxpool.Pool, generic bool, sql string, args ...any) wlmCost {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var raw []byte
	if generic {
		lits := make([]string, len(args))
		for i, a := range args {
			switch v := a.(type) {
			case time.Time:
				lits[i] = "'" + v.UTC().Format("2006-01-02 15:04:05.999999") + "+00'"
			case int:
				lits[i] = strconv.Itoa(v)
			default:
				t.Fatalf("arg %T", a)
			}
		}
		for _, st := range []string{"SET plan_cache_mode = force_generic_plan", "PREPARE wlm AS " + sql} {
			if _, err := conn.Exec(ctx, st); err != nil {
				t.Fatalf("%s: %v", st, err)
			}
		}
		err = conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT JSON) EXECUTE wlm("+strings.Join(lits, ", ")+")").Scan(&raw)
		_, _ = conn.Exec(ctx, "DEALLOCATE wlm")
		_, _ = conn.Exec(ctx, "RESET plan_cache_mode")
	} else {
		err = conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT JSON) "+sql, args...).Scan(&raw)
	}
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var doc []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	num := func(k string) int64 { v, _ := doc[0].Plan[k].(float64); return int64(v) }
	var nodes []string
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		s, _ := n["Node Type"].(string)
		if v, ok := n["Index Name"].(string); ok {
			s += "(" + v + ")"
		}
		nodes = append(nodes, s)
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	walk(doc[0].Plan)
	return wlmCost{buffers: num("Shared Hit Blocks") + num("Shared Read Blocks"), wal: num("WAL Bytes"), ms: doc[0].ExecutionTime, nodes: strings.Join(nodes, ">")}
}

// wlmFill writes n rows: 90 % of the dream audit's width (a metadata object
// of about 400 bytes), 10 % of the guard's; past of them older than 30 days,
// the rest inside, in time order.
func wlmFill(t *testing.T, pool *pgxpool.Pool, n, past int) {
	t.Helper()
	ctx := context.Background()
	const chunk = 200000
	for off := 0; off < n; off += chunk {
		m := chunk
		if off+m > n {
			m = n - off
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO context_write_log (decision, similarity, scope, block_title, block_category, metadata, created_at)
			SELECT CASE WHEN g % 10 = 0 THEN 'clean' ELSE 'dream_link' END, 0.5, 'private', 'title of block ' || g, 'decisions',
			       CASE WHEN g % 10 = 0 THEN '{"threshold_duplicate": 0.95, "threshold_review": 0.85}'::jsonb
			            ELSE jsonb_build_object('source', 'dream_v1', 'links_created', 1, 'links',
			                 jsonb_build_array(jsonb_build_object('target_id', gen_random_uuid(), 'confidence', random(), 'relationship', 'topical'),
			                                   jsonb_build_object('target_id', gen_random_uuid(), 'confidence', random(), 'relationship', 'factual'),
			                                   jsonb_build_object('target_id', gen_random_uuid(), 'confidence', random(), 'relationship', 'causal'))) END,
			       CASE WHEN g <= $3 THEN now() - interval '60 days' + make_interval(secs => g * 0.001)
			            ELSE now() - interval '20 days' + make_interval(secs => (g - $3) * 0.001) END
			  FROM generate_series($1::int + 1, $1::int + $2::int) g`, off, m, past)
		if err != nil {
			t.Fatalf("fill: %v", err)
		}
	}
}

func TestEvictWriteLogMeasure(t *testing.T) {
	sizes := os.Getenv("CTX_WRITELOG_MEASURE")
	if sizes == "" {
		t.Skip("CTX_WRITELOG_MEASURE unset (comma-separated table sizes)")
	}
	ctx := context.Background()
	t.Run("count46000", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		wlmFill(t, pool, 46000, 0)
		for _, state := range []string{"after inserts", "after vacuum"} {
			if state == "after vacuum" {
				if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) context_write_log`); err != nil {
					t.Fatal(err)
				}
			}
			for _, rpc := range testdb.RandomPageCosts {
				p := testdb.WithRandomPageCost(t, pool, rpc)
				for i := 0; i < 3; i++ {
					c := wlmExplain(t, p, false, `SELECT count(*)::int FROM context_write_log`)
					t.Logf("WLM|count|46000|%s|rpc %s|%d buffers|%.2f ms|%s", state, rpc, c.buffers, c.ms, c.nodes)
				}
			}
		}
	})
	for _, s := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(s, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			past := n * 3 / 10
			start := time.Now()
			wlmFill(t, pool, n, past)
			if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) context_write_log`); err != nil {
				t.Fatal(err)
			}
			var heap string
			_ = pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_table_size('context_write_log'))`).Scan(&heap)
			t.Logf("WLM|fill|%d rows (%d past)|heap %s|%s", n, past, heap, time.Since(start).Round(time.Second))
			for _, rpc := range testdb.RandomPageCosts {
				p := testdb.WithRandomPageCost(t, pool, rpc)
				for i := 0; i < 3; i++ {
					c := wlmExplain(t, p, false, `SELECT count(*)::int FROM context_write_log`)
					t.Logf("WLM|count|%d|after vacuum|rpc %s|%d buffers|%.2f ms|%s", n, rpc, c.buffers, c.ms, c.nodes)
				}
			}
			var horizon time.Time
			if err := pool.QueryRow(ctx, `SELECT now() - interval '30 days'`).Scan(&horizon); err != nil {
				t.Fatal(err)
			}
			// Batches 1, 2, 5 of a run, per cell on a part of the past rows of
			// its own, after a vacuum (a run's first batch starts at the
			// oldest entry of the index). The keyset of batch b > 1 stands for
			// the one EvictWriteLog passes (created_at of the last row the
			// batch before deleted): the oldest row left past the horizon —
			// rows are distinct and deleted in order, so the range is the same
			// minus that one dead entry.
			for _, rpc := range testdb.RandomPageCosts {
				p := testdb.WithRandomPageCost(t, pool, rpc)
				for _, generic := range []bool{false, true} {
					mode := map[bool]string{false: "custom", true: "generic"}[generic]
					if _, err := pool.Exec(ctx, `VACUUM context_write_log`); err != nil {
						t.Fatal(err)
					}
					var key time.Time
					// Four cells share the rows past the horizon; a small table
					// gives each cell fewer batches.
					per := min(5, past/(4*store.WriteLogRetentionBatch))
					for b := 1; b <= per; b++ {
						c := wlmExplain(t, p, generic, store.WriteLogEvictBatchSQLForTest, horizon, key, store.WriteLogRetentionBatch)
						// The next keyset, only when a batch of the cell follows:
						// the cells together may delete every row past the
						// horizon, and min() of none is NULL.
						if b < per {
							if err := pool.QueryRow(ctx, `SELECT min(created_at) FROM context_write_log WHERE created_at < $1`, horizon).Scan(&key); err != nil {
								t.Fatal(err)
							}
						}
						if b == 1 || b == 2 || b == per {
							t.Logf("WLM|batch|%d|rpc %s|%s|batch %d|%d buffers|%.1f ms|WAL %d B|%s", n, rpc, mode, b, c.buffers, c.ms, c.wal, c.nodes)
						}
					}
				}
			}
			// The rest in one run of the production function.
			var wal0, wal1 string
			_ = pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&wal0)
			st := time.Now()
			got, err := store.EvictWriteLog(ctx, pool, 30)
			el := time.Since(st)
			_ = pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&wal1)
			var walBytes int64
			_ = pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)::bigint`, wal1, wal0).Scan(&walBytes)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("WLM|run|%d|deleted %d in %s (%.1f ms per batch)|WAL %d MB", n, got, el.Round(time.Millisecond),
				float64(el.Milliseconds())/float64((got+int64(store.WriteLogRetentionBatch)-1)/int64(store.WriteLogRetentionBatch)), walBytes>>20)
			// A second run right after, no vacuum between: its first batch
			// starts at the oldest entry of the index again.
			wlmFill2 := func(m int) {
				if _, err := pool.Exec(ctx, `
					INSERT INTO context_write_log (decision, scope, block_title, created_at)
					SELECT 'clean', 'private', 't', now() - interval '25 days' + make_interval(secs => g * 0.001)
					  FROM generate_series(1, $1::int) g`, m); err != nil {
					t.Fatal(err)
				}
			}
			wlmFill2(store.WriteLogRetentionBatch)
			if err := pool.QueryRow(ctx, `SELECT now() - interval '24 days'`).Scan(&horizon); err != nil {
				t.Fatal(err)
			}
			c := wlmExplain(t, pool, false, store.WriteLogEvictBatchSQLForTest, horizon, time.Time{}, store.WriteLogRetentionBatch)
			t.Logf("WLM|second run, no vacuum|%d|first batch|%d buffers|%.1f ms|%s", n, c.buffers, c.ms, c.nodes)
			if _, err := pool.Exec(ctx, `VACUUM context_write_log`); err != nil {
				t.Fatal(err)
			}
			wlmFill2(store.WriteLogRetentionBatch)
			c = wlmExplain(t, pool, false, store.WriteLogEvictBatchSQLForTest, horizon, time.Time{}, store.WriteLogRetentionBatch)
			t.Logf("WLM|second run, after vacuum|%d|first batch|%d buffers|%.1f ms|%s", n, c.buffers, c.ms, c.nodes)
		})
	}
}

