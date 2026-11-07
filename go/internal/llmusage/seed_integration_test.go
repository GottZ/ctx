//go:build integration

// W06-13 (c) against a real PG18 + TimescaleDB: SeedConsumers reads what
// every model consumer used per gateway pipeline in the running UTC day and
// month from the hourly rollup — equal, consumer by consumer and pipeline by
// pipeline, to the same sums over the raw context_llm_log rows, for random
// logs laid around the day and month boundaries — and its plan on a rollup
// of about one million consumer rows (CTX_W0613_SEED_PLAN=1, a measurement).
//
//	go test -tags=integration -p 1 ./internal/llmusage/ -run Seed -count=1 -v
package llmusage_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rawSeedSQL is the seed over the raw rows: the reference.
const rawSeedSQL = `
SELECT l.consumer_id::text, c.tenant_id::text, l.pipeline,
       COALESCE(sum(COALESCE(l.prompt_tokens, 0) + COALESCE(l.completion_tokens, 0)) FILTER (WHERE l.created_at >= $1), 0)::bigint,
       count(*) FILTER (WHERE l.created_at >= $1),
       COALESCE(sum(COALESCE(l.prompt_tokens, 0) + COALESCE(l.completion_tokens, 0)), 0)::bigint,
       COALESCE(sum(l.cost_usd) FILTER (WHERE l.backend_locality = 'external'), 0)::float8
  FROM context_llm_log l
  JOIN context_model_consumers c ON c.id = l.consumer_id
 WHERE l.consumer_id IS NOT NULL AND l.created_at >= $2
 GROUP BY 1, 2, 3`

func seedTenantConsumers(ctx context.Context, t *testing.T, pool *pgxpool.Pool, slug string, n int) (string, []string) {
	t.Helper()
	var tenant string
	if err := pool.QueryRow(ctx, `INSERT INTO context_tenants (slug, display_name) VALUES ($1, $2) RETURNING id::text`, slug, slug).Scan(&tenant); err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_tenants SET max_model_consumers = 1000 WHERE id = $1::uuid`, tenant); err != nil {
		t.Fatalf("tenant cap: %v", err)
	}
	ids := make([]string, n)
	for i := range ids {
		if err := pool.QueryRow(ctx, `INSERT INTO context_model_consumers (tenant_id, label) VALUES ($1::uuid, $2) RETURNING id::text`,
			tenant, fmt.Sprintf("%s-%d", slug, i)).Scan(&ids[i]); err != nil {
			t.Fatalf("consumer: %v", err)
		}
	}
	return tenant, ids
}

func usageKey(u llmusage.ConsumerUsage) string { return u.ConsumerID + "|" + u.Pipeline }

// TestSeedConsumersEqualsTheRawRows is the parity gate of the boot seed: for
// random logs around the UTC day and month boundaries (internal rows and
// rows before the month included, which must not count) SeedConsumers equals
// the raw sums — tokens and calls of the day, tokens of the month, external
// cost of the month — and names each consumer's tenant.
func TestSeedConsumersEqualsTheRawRows(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tA, ca := seedTenantConsumers(ctx, t, pool, "w0613-a", 3)
	tB, cb := seedTenantConsumers(ctx, t, pool, "w0613-b", 2)
	tenantOf := map[string]string{}
	for _, c := range ca {
		tenantOf[c] = tA
	}
	for _, c := range cb {
		tenantOf[c] = tB
	}
	consumers := append(append([]string{}, ca...), cb...)
	var key string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM context_api_keys LIMIT 1`).Scan(&key); err != nil {
		key = ""
	}
	pipelines := []string{"gateway-chat", "gateway-embeddings", "gateway-rerank"}
	r := rand.New(rand.NewPCG(613, 1))
	for _, now := range []time.Time{
		time.Date(2026, 11, 7, 18, 30, 0, 0, time.UTC),
		time.Date(2026, 12, 1, 0, 0, 30, 0, time.UTC),
		time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC),
	} {
		if _, err := pool.Exec(ctx, `DELETE FROM context_llm_log; DELETE FROM context_llm_usage_hourly`); err != nil {
			t.Fatal(err)
		}
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		var at []time.Time
		var cons, pipe, loc []string
		var pt, ct []*int64
		var cost []*float64
		for range 400 {
			var base time.Time
			switch r.IntN(4) {
			case 0:
				base = dayStart.Add(time.Duration(r.Int64N(int64(2*time.Hour))) - time.Hour)
			case 1:
				base = monthStart.Add(time.Duration(r.Int64N(int64(2*time.Hour))) - time.Hour)
			default:
				base = monthStart.Add(-48 * time.Hour).Add(time.Duration(r.Int64N(int64(now.Sub(monthStart) + 48*time.Hour + time.Second))))
			}
			at = append(at, base)
			cons = append(cons, consumers[r.IntN(len(consumers))])
			pipe = append(pipe, pipelines[r.IntN(len(pipelines))])
			loc = append(loc, []string{"local", "external", ""}[r.IntN(3)])
			p, c := int64(r.IntN(5000)), int64(r.IntN(500))
			var pp, cp *int64
			if r.IntN(10) > 0 {
				pp, cp = &p, &c
			}
			pt, ct = append(pt, pp), append(ct, cp)
			var co *float64
			if r.IntN(3) == 0 {
				v := float64(r.IntN(1000)) / 10000
				co = &v
			}
			cost = append(cost, co)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms, consumer_id, backend_locality,
			                             prompt_tokens, completion_tokens, cost_usd)
			SELECT t, p, 'm', 'h', 10, c::uuid, NULLIF(loc, ''), pt, ct, co
			  FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::text[], $5::int8[], $6::int8[], $7::float8[])
			       AS u(t, c, p, loc, pt, ct, co)`, at, cons, pipe, loc, pt, ct, cost); err != nil {
			t.Fatalf("insert log: %v", err)
		}
		if key != "" {
			if _, err := pool.Exec(ctx, `
				INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms, api_key_id, prompt_tokens, completion_tokens)
				VALUES ($1, 'query', 'm', 'h', 10, $2::uuid, 999, 999)`, now.Add(-time.Minute), key); err != nil {
				t.Fatalf("internal row: %v", err)
			}
		}
		got, err := llmusage.SeedConsumers(ctx, pool, now)
		if err != nil {
			t.Fatalf("SeedConsumers(%v): %v", now, err)
		}
		rows, err := pool.Query(ctx, rawSeedSQL, dayStart, monthStart)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]llmusage.ConsumerUsage{}
		for rows.Next() {
			var u llmusage.ConsumerUsage
			if err := rows.Scan(&u.ConsumerID, &u.TenantID, &u.Pipeline, &u.DayTokens, &u.DayCalls, &u.MonthTokens, &u.MonthExternalCost); err != nil {
				t.Fatal(err)
			}
			want[usageKey(u)] = u
		}
		rows.Close()
		if len(got) != len(want) || len(want) == 0 {
			t.Fatalf("%v: %d seed rows, raw %d", now, len(got), len(want))
		}
		var keys []string
		for _, g := range got {
			w, ok := want[usageKey(g)]
			keys = append(keys, usageKey(g))
			if !ok || g.TenantID != tenantOf[g.ConsumerID] || g.DayTokens != w.DayTokens || g.DayCalls != w.DayCalls || g.MonthTokens != w.MonthTokens ||
				fmt.Sprintf("%.6f", g.MonthExternalCost) != fmt.Sprintf("%.6f", w.MonthExternalCost) {
				t.Errorf("%v %s: seed %+v, raw %+v", now, usageKey(g), g, w)
			}
		}
		sort.Strings(keys)
		t.Logf("%v: %d (consumer, pipeline) rows equal", now, len(keys))
	}
}

// TestSeedPlanAtOneMillionRows is the plan measurement of W06-13 (offene
// Frage 1): EXPLAIN (ANALYZE, BUFFERS) of the seed on a rollup of 50
// consumers × 30 days × 24 hours × 3 pipelines × 9 models (972 000 consumer
// rows) plus 200 000 internal rows. A measurement, not a gate: the plan and
// the runtime go to the log; a run needs CTX_W0613_SEED_PLAN=1.
func TestSeedPlanAtOneMillionRows(t *testing.T) {
	if os.Getenv("CTX_W0613_SEED_PLAN") == "" {
		t.Skip("set CTX_W0613_SEED_PLAN=1 for the plan measurement")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, consumers := seedTenantConsumers(ctx, t, pool, "w0613-plan", 50)
	start := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_usage_hourly (bucket_start, consumer_id, pipeline, model, backend_locality, calls, prompt_tokens, completion_tokens, cost_usd)
		SELECT h, c::uuid, p, 'model-' || m, 'local', 1, 100, 10, 0
		  FROM generate_series(timestamptz '2026-10-08 00:00Z', timestamptz '2026-11-06 23:00Z', interval '1 hour') h,
		       unnest($1::text[]) c,
		       unnest(ARRAY['gateway-chat','gateway-embeddings','gateway-rerank']) p,
		       generate_series(1, 9) m`, consumers); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_usage_hourly (bucket_start, api_key_id, pipeline, model, calls, prompt_tokens)
		SELECT h, gen_random_uuid(), 'query', 'model-' || m, 1, 100
		  FROM generate_series(timestamptz '2026-10-08 00:00Z', timestamptz '2026-11-06 23:00Z', interval '1 hour') h,
		       generate_series(1, 278) m`); err != nil {
		t.Fatalf("internal rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_llm_usage_hourly`); err != nil {
		t.Fatal(err)
	}
	var n int64
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM context_llm_usage_hourly`).Scan(&n)
	t.Logf("rollup rows: %d (built in %v)", n, time.Since(start).Round(time.Millisecond))
	now := time.Date(2026, 11, 7, 18, 30, 0, 0, time.UTC)
	for _, run := range []string{"cold", "warm"} {
		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+llmusage.SeedSQL(),
			time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			plan = append(plan, line)
		}
		rows.Close()
		t.Logf("%s plan:\n%s", run, strings.Join(plan, "\n"))
	}
	t0 := time.Now()
	got, err := llmusage.SeedConsumers(ctx, pool, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SeedConsumers: %d rows in %v", len(got), time.Since(t0).Round(time.Microsecond))
}
