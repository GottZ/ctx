//go:build integration

package dream_test

// Issue #536 (first half): the report backfill decides at the write log's
// eviction boundary in the snapshot of its decision count, and the dream
// queue counts read the reader's eligible rows in one pass, without a walk
// of an index over all of them and without JIT.
//
//	go test -tags=integration -p 1 ./internal/dream/ -run 'TestRangeReads' -count=1 -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestRangeReadsBackfillRefusesAWindowTheRetentionReached: a backfill whose
// window starts before the eviction boundary its decision count reads is
// refused (*dream.WriteLogEvictedError). The handler checks the boundary
// before the synthesis in a statement of its own; a retention run between
// that check and the count deleted the window's decisions, and the backfill
// replaced the day's report with one without them — here the retention has
// run before the synthesis reads, the case that interleaving leaves.
func TestRangeReadsBackfillRefusesAWindowTheRetentionReached(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -10)
	windowStart := time.Date(day.Year(), day.Month(), day.Day(), 3, 0, 0, 0, time.UTC).Add(-24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO context_write_log (decision, scope, created_at)
		VALUES ('dream_link', $1, $2), ('clean', $1, $3)`, reportScope, windowStart.Add(2*time.Hour), windowStart.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The retention at five days deletes them and records its horizon.
	if n, err := store.EvictWriteLog(ctx, pool, 5); err != nil || n != 2 {
		t.Fatalf("retention: deleted %d, %v; want 2", n, err)
	}
	_, err := dream.GenerateDailyReportFor(ctx, pool, reportRouter(t, ctx, pool), reportScope, day)
	var evicted *dream.WriteLogEvictedError
	if !errors.As(err, &evicted) {
		t.Fatalf("backfill of %s after the retention reached its window: %v, want a *dream.WriteLogEvictedError", day.Format("2006-01-02"), err)
	}
	boundary, berr := store.WriteLogEvictedBefore(ctx, pool)
	if berr != nil || boundary == nil || !evicted.Boundary.Equal(*boundary) {
		t.Errorf("refused at %v, the recorded boundary is %v (%v)", evicted.Boundary, boundary, berr)
	}
	// A window inside the horizon is not refused: no activity, no report.
	inside := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -2)
	if id, err := dream.GenerateDailyReportFor(ctx, pool, reportRouter(t, ctx, pool), reportScope, inside); err != nil || id != "" {
		t.Errorf("backfill of %s inside the horizon: %q, %v; want no report and no error", inside.Format("2006-01-02"), id, err)
	}
	// The rolling report does not read the boundary: it still runs where the
	// way back of migration 194 dropped the column.
	if _, err := pool.Exec(ctx, `ALTER TABLE context_guard_state DROP COLUMN write_log_evicted_before`); err != nil {
		t.Fatal(err)
	}
	if id, err := dream.GenerateDailyReport(ctx, pool, reportRouter(t, ctx, pool), reportScope); err != nil || id != "" {
		t.Errorf("the rolling report without the boundary column: %q, %v; want no report and no error", id, err)
	}
}

// qdOld is dream.QueueDepth's statement in 89d9e70c, byte for byte: the
// reference of its seven numbers.
const qdOld = `WITH eligible AS (
			SELECT dream_cooldown_until AS cd, dream_checked_at AS chk, embedding IS NULL AS no_embed
			FROM context_blocks
			WHERE NOT is_archived
		  AND lifecycle_state IN ('knowledge', 'canonical', 'synthesis')
		  AND type_name = ANY($2::text[])
			  AND scope = ANY($1::text[])
		)
		SELECT
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND (cd IS NULL OR cd < now()))::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd >= now())::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND chk IS NULL)::int,
			(SELECT count(*) FROM eligible WHERE no_embed)::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd > now() AND cd <= now() + interval '1 hour')::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd > now() AND cd <= now() + interval '6 hours')::int,
			(SELECT min(cd) FROM eligible WHERE NOT no_embed AND cd > now())`

type qdTracer struct {
	mu   sync.Mutex
	sqls []string
	args [][]any
}

func (q *qdTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	q.mu.Lock()
	q.sqls = append(q.sqls, d.SQL)
	q.args = append(q.args, d.Args)
	q.mu.Unlock()
	return ctx
}

func (q *qdTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestRangeReadsQueueDepthReadsTheEligibleRowsOnce: the dream queue counts
// are one aggregate pass, sent in a transaction with plain index scans and
// JIT off, and answer the numbers of the statement before for every reader
// below. Before, a CTE every one of seven sub-selects read (PostgreSQL
// materializes it), on the pool, whose custom plan at random_page_cost 4
// walked idx_blocks_sensitivity over every live row of the table (22-32 s at
// 10 000 000 rows) and whose generic plan compiled JIT on every execution
// from 4 000 000 rows on.
func TestRangeReadsQueueDepthReadsTheEligibleRowsOnce(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, is_archived, dream_checked_at, dream_cooldown_until, sensitivity)
		SELECT 'learnings', 'qd ' || i, 'qd body ' || i, (ARRAY['qd-a','qd-b','qd-c','qd-o'])[1 + (i % 4)],
		       (ARRAY['knowledge','issue','checkpoint','reference'])[1 + ((i / 4) % 4)], i % 13 = 0,
		       CASE WHEN i % 3 = 0 THEN NULL ELSE now() - interval '2 days' END,
		       CASE WHEN i % 3 = 1 THEN now() + make_interval(mins => i % 600) WHEN i % 3 = 2 THEN now() - interval '1 day' END,
		       'internal'
		  FROM generate_series(1, 20000) i`); err != nil {
		t.Fatal(err)
	}
	// Part of the eligible rows hold a vector (the counts that read
	// embedding IS NOT NULL are not all zero).
	vec, _ := json.Marshal(make([]float32, 1024))
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = $1::vector WHERE scope IN ('qd-a','qd-b') AND title LIKE '%7'`, string(vec)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `VACUUM ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}
	tr := &qdTracer{}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = tr
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	for _, c := range []struct {
		scopes, linkable []string
	}{
		{[]string{"qd-a", "qd-b", "qd-c"}, []string{"knowledge"}},
		{[]string{"qd-a"}, []string{"knowledge", "reference"}},
		{[]string{"qd-c", "qd-c"}, []string{"knowledge"}},
		{[]string{"qd-none"}, []string{"knowledge"}},
		{[]string{"qd-a", "qd-o"}, []string{}},
	} {
		tr.mu.Lock()
		tr.sqls, tr.args = nil, nil
		tr.mu.Unlock()
		got, err := dream.QueueDepth(ctx, traced, c.scopes, c.linkable)
		if err != nil {
			t.Fatalf("QueueDepth(%v, %v): %v", c.scopes, c.linkable, err)
		}
		var want dream.QueueStats
		if err := pool.QueryRow(ctx, qdOld, c.scopes, c.linkable).Scan(&want.PickableNow, &want.InCooldown, &want.NeverDreamed,
			&want.AwaitingEmbed, &want.Incoming1h, &want.Incoming6h, &want.NextPendingAt); err != nil {
			t.Fatal(err)
		}
		next := func(q dream.QueueStats) string {
			if q.NextPendingAt == nil {
				return "<nil>"
			}
			return q.NextPendingAt.UTC().Format(time.RFC3339Nano)
		}
		counts := func(q dream.QueueStats) string {
			return fmt.Sprint(q.PickableNow, q.InCooldown, q.NeverDreamed, q.AwaitingEmbed, q.Incoming1h, q.Incoming6h, next(q))
		}
		if counts(*got) != counts(want) {
			t.Errorf("QueueDepth(%v, %v) = %s, the statement before answers %s", c.scopes, c.linkable, counts(*got), counts(want))
		}
		if want.PickableNow+want.InCooldown+want.AwaitingEmbed == 0 && c.scopes[0] != "qd-none" && len(c.linkable) > 0 {
			t.Errorf("QueueDepth(%v, %v): the fixture holds no eligible row", c.scopes, c.linkable)
		}
		tr.mu.Lock()
		sqls := strings.Join(tr.sqls, "\n")
		tr.mu.Unlock()
		if !strings.Contains(sqls, "SET LOCAL enable_indexscan = off") || !strings.Contains(sqls, "SET LOCAL jit = off") {
			t.Errorf("QueueDepth(%v, %v) did not turn plain index scans and JIT off for its statement; sent:\n%s", c.scopes, c.linkable, sqls)
		}
		if strings.Contains(sqls, "WITH eligible AS") {
			t.Errorf("QueueDepth(%v, %v) still reads the eligible rows through a CTE", c.scopes, c.linkable)
		}
	}
	// Under those settings no plan of the statement walks an index over the
	// eligible rows, at either random_page_cost, custom or generic.
	tr.mu.Lock()
	tr.sqls, tr.args = nil, nil
	tr.mu.Unlock()
	if _, err := dream.QueueDepth(ctx, traced, []string{"qd-a", "qd-b", "qd-c"}, []string{"knowledge"}); err != nil {
		t.Fatal(err)
	}
	tr.mu.Lock()
	var sql string
	var args []any
	for i := len(tr.sqls) - 1; i >= 0; i-- {
		if strings.Contains(tr.sqls[i], "dream_cooldown_until") {
			sql, args = tr.sqls[i], tr.args[i]
			break
		}
	}
	tr.mu.Unlock()
	if sql == "" {
		t.Fatal("the counts' statement was not traced")
	}
	for _, rpc := range testdb.RandomPageCosts {
		p := testdb.WithSessionSettings(t, pool, map[string]string{"random_page_cost": rpc, "enable_indexscan": "off", "jit": "off"})
		for plan, c := range map[string]testdb.PlanCost{
			"custom":  testdb.ExplainCustom(t, p, sql, args...),
			"generic": testdb.ExplainGeneric(t, p, sql, args...),
		} {
			if strings.Contains(c.Plan, `"Node Type": "Index Scan"`) || strings.Contains(c.Plan, `"JIT"`) {
				t.Errorf("random_page_cost %s, %s plan: an index walk or JIT under the statement's settings:\n%s", rpc, plan, c.Plan)
			}
			t.Logf("random_page_cost %s, %s plan: %s", rpc, plan, c)
		}
	}
}
