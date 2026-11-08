//go:build integration

// W06-16 gates of the series reader against PG18 + TimescaleDB with every
// migration (RC-D S1, cluster C2):
//
//   - null ≠ 0: a bucket before the coverage is incomplete with null
//     counters, a covered bucket without rows is a real 0, the running bucket
//     is incomplete with its partial values, refusals before migration 167
//     are null;
//
//   - tenant filter: rows of other consumers and internal rows never appear
//     in a filtered series; exposure of target/dispatch_class;
//
//   - plan: the filtered series reads the rollup through
//     idx_llm_usage_consumer (EXPLAIN, custom and generic) — red against a
//     filter on api_key_id or a scan over bucket_start.
//
//     go test -tags=integration -p 1 -count=1 -run 'TestReadSeries|TestReadCover|TestSeriesPlan' -v ./internal/llmusage/
package llmusage_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// usageRollupRow inserts one rollup row of a consumer.
func usageRollupRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, consumer string, at time.Time, calls, prompt, durSum, durN int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_usage_hourly (bucket_start, consumer_id, pipeline, model, model_alias, backend_name,
		       calls, prompt_tokens, completion_tokens, duration_ms_sum, duration_n, latency_hist)
		VALUES ($1, $2::uuid, 'gateway-chat', 'm', 'chat', 'spark', $3, $4, 1, $5, $6,
		        array_fill(0::bigint, ARRAY[16]))`, at, consumer, calls, prompt, durSum, durN); err != nil {
		t.Fatalf("rollup row: %v", err)
	}
}

// TestReadSeriesNullNotZero is W06-16's gate "null ≠ 0" (masterplan §3.1).
// The rollup's first row lies at now − 30 h, so coverage starts there; the
// ledger (migration 167) is moved to now − 10 h. A 48-hour hourly series of
// consumer A: buckets before −30 h ⇒ complete:false, calls null; −30 h ⇒ 3
// calls; −20 h (no row) ⇒ calls 0, complete:true; the running hour ⇒
// complete:false with its partial value; rejected null before −10 h, [] after.
func TestReadSeriesNullNotZero(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, ids := seedTenantConsumers(ctx, t, pool, "w0616-null", 1)
	a := ids[0]
	now := time.Now().UTC()
	hour := now.Truncate(time.Hour)
	usageRollupRow(ctx, t, pool, a, hour.Add(-30*time.Hour), 3, 300, 900, 3)
	usageRollupRow(ctx, t, pool, a, hour, 2, 200, 100, 2)
	if _, err := pool.Exec(ctx, `UPDATE _migrations SET applied_at = $1 WHERE version = 167`, hour.Add(-10*time.Hour)); err != nil {
		t.Fatalf("move migration 167: %v", err)
	}
	cover, err := llmusage.ReadCover(ctx, pool)
	if err != nil {
		t.Fatalf("W06-16 gate (null ≠ 0): ReadCover: %v", err)
	}
	if !cover.UsageSince.Equal(hour.Add(-30*time.Hour)) || !cover.RejectionsSince.Equal(hour.Add(-10*time.Hour)) {
		t.Errorf("W06-16 gate (coverage): %+v, want usage since %v, rejections since %v", cover, hour.Add(-30*time.Hour), hour.Add(-10*time.Hour))
	}
	q, err := llmusage.Align(llmusage.SeriesQuery{From: now.Add(-48 * time.Hour), To: now, Bucket: llmusage.BucketHour,
		Group: llmusage.GroupConsumer, ConsumerIDs: []string{a}, Now: now})
	if err != nil {
		t.Fatalf("W06-16 gate (null ≠ 0): Align: %v", err)
	}
	res, err := llmusage.ReadSeries(ctx, pool, q, cover)
	if err != nil {
		t.Fatalf("W06-16 gate (null ≠ 0): ReadSeries: %v", err)
	}
	if len(res.Series) != 1 {
		t.Fatalf("W06-16 gate (null ≠ 0): %d series, want 1 (consumer %s)", len(res.Series), a)
	}
	if res.Complete {
		t.Errorf("W06-16 gate (null ≠ 0): result complete = true over a window that starts before the coverage")
	}
	byStart := map[time.Time]llmusage.Point{}
	for _, p := range res.Series[0].Points {
		byStart[p.BucketStart.UTC()] = p
	}
	check := func(name string, at time.Time, complete bool, calls *int64, rejectedNull bool) {
		t.Helper()
		p, ok := byStart[at]
		if !ok {
			t.Errorf("W06-16 gate (null ≠ 0) %s: no point at %v", name, at)
			return
		}
		switch {
		case p.Complete != complete:
			t.Errorf("W06-16 gate (null ≠ 0) %s: complete = %v, want %v", name, p.Complete, complete)
		case (calls == nil) != (p.Calls == nil) || (calls != nil && *calls != *p.Calls):
			t.Errorf("W06-16 gate (null ≠ 0) %s: calls = %v, want %v (nil = null, never 0)", name, ptrStr(p.Calls), ptrStr(calls))
		case (p.Rejected == nil) != rejectedNull:
			t.Errorf("W06-16 gate (null ≠ 0) %s: rejected null = %v, want %v", name, p.Rejected == nil, rejectedNull)
		}
	}
	three, zero, two := int64(3), int64(0), int64(2)
	check("before coverage (−40 h)", hour.Add(-40*time.Hour), false, nil, true)
	check("first covered hour (−30 h)", hour.Add(-30*time.Hour), true, &three, true)
	check("covered, no traffic (−20 h)", hour.Add(-20*time.Hour), true, &zero, true)
	check("ledger covered (−5 h)", hour.Add(-5*time.Hour), true, &zero, false)
	check("running hour", hour, false, &two, false)
	if p := byStart[hour.Add(-30*time.Hour)]; p.AvgMs == nil || *p.AvgMs != 300 {
		t.Errorf("W06-16 gate (avg): avg_ms = %v, want 300 (900 ms over 3)", ptrStrF(p.AvgMs))
	}
	if p := byStart[hour.Add(-20*time.Hour)]; p.AvgMs != nil || p.P95Ms != nil {
		t.Errorf("W06-16 gate (null ≠ 0): a covered hour without samples has avg %v / p95 %v, want null", ptrStrF(p.AvgMs), ptrStrF(p.P95Ms))
	}
}

func ptrStr(p *int64) string {
	if p == nil {
		return "null"
	}
	b, _ := json.Marshal(*p)
	return string(b)
}

func ptrStrF(p *float64) string {
	if p == nil {
		return "null"
	}
	b, _ := json.Marshal(*p)
	return string(b)
}

// TestReadSeriesTenantFilterAndExposure: a series filtered to consumer A
// never carries consumer B's rows nor internal (knowledge-key) rows; a
// refusal of A with reason capacity carries target/dispatch_class only for
// ServerAdmin; an auth refusal (no consumer) is never in a filtered series.
func TestReadSeriesTenantFilterAndExposure(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, ids := seedTenantConsumers(ctx, t, pool, "w0616-exp", 2)
	a, b := ids[0], ids[1]
	now := time.Now().UTC()
	hour := now.Truncate(time.Hour).Add(-2 * time.Hour)
	usageRollupRow(ctx, t, pool, a, hour.Add(-24*time.Hour), 1, 1, 1, 1) // coverage start
	usageRollupRow(ctx, t, pool, a, hour, 5, 50, 10, 5)
	usageRollupRow(ctx, t, pool, b, hour, 7, 70, 10, 7)
	if _, err := pool.Exec(ctx, `INSERT INTO context_llm_usage_hourly (bucket_start, api_key_id, pipeline, calls) VALUES ($1, gen_random_uuid(), 'query', 11)`, hour); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE _migrations SET applied_at = $1 WHERE version = 167`, hour.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_rejections_hourly (bucket_start, consumer_id, op, model_alias, dispatch_class, target, reason, detail, n)
		VALUES ($1, $2::uuid, 'chat', 'chat', 'external_urgent', 'http://spark:30000', 'capacity', 'server_busy', 4),
		       ($1, NULL, '', '', '', '', 'auth', 'invalid_key', 9)`, hour, a); err != nil {
		t.Fatal(err)
	}
	cover, err := llmusage.ReadCover(ctx, pool)
	if err != nil {
		t.Fatalf("W06-16 gate (exposure): ReadCover: %v", err)
	}
	read := func(admin bool) []byte {
		t.Helper()
		q, err := llmusage.Align(llmusage.SeriesQuery{From: hour, To: hour.Add(time.Hour), Bucket: llmusage.BucketHour,
			Group: llmusage.GroupConsumer, ConsumerIDs: []string{a}, ServerAdmin: admin, Now: now})
		if err != nil {
			t.Fatalf("W06-16 gate (exposure): Align: %v", err)
		}
		res, err := llmusage.ReadSeries(ctx, pool, q, cover)
		if err != nil {
			t.Fatalf("W06-16 gate (exposure): ReadSeries: %v", err)
		}
		out, _ := json.Marshal(res)
		return out
	}
	tenant := string(read(false))
	if strings.Contains(tenant, b) || strings.Contains(tenant, `"calls":7`) || strings.Contains(tenant, `"calls":11`) {
		t.Errorf("W06-16 gate (tenant filter): A's series carries B's or internal rows: %s", tenant)
	}
	if !strings.Contains(tenant, `"calls":5`) || !strings.Contains(tenant, `"server_busy"`) {
		t.Errorf("W06-16 gate (Finding 24): A's own usage or refusal is missing: %s", tenant)
	}
	if strings.Contains(tenant, "spark:30000") || strings.Contains(tenant, "external_urgent") || strings.Contains(tenant, "invalid_key") {
		t.Errorf("W06-16 gate (exposure): a non-server-admin series carries target/dispatch_class or an auth refusal: %s", tenant)
	}
	if admin := string(read(true)); !strings.Contains(admin, "spark:30000") || !strings.Contains(admin, "external_urgent") {
		t.Errorf("W06-16 gate (exposure): the server-admin series lacks target/dispatch_class: %s", admin)
	}
}

// TestSeriesPlanConsumerIndex is V-06-a (a): the filtered series reads the
// rollup through idx_llm_usage_consumer on a rollup of 50 consumers × 30
// days × 9 models plus internal rows (≈ 520 000 rows), under the custom plan
// and the generic plan of a prepared statement.
func TestSeriesPlanConsumerIndex(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, consumers := seedTenantConsumers(ctx, t, pool, "w0616-plan", 50)
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_usage_hourly (bucket_start, consumer_id, pipeline, model, model_alias, calls, prompt_tokens)
		SELECT h, c::uuid, 'gateway-chat', 'model-' || m, 'alias-' || m, 1, 100
		  FROM generate_series(timestamptz '2026-10-08 00:00Z', timestamptz '2026-11-06 23:00Z', interval '1 hour') h,
		       unnest($1::text[]) c, generate_series(1, 9) m`, consumers); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_usage_hourly (bucket_start, api_key_id, pipeline, model, calls)
		SELECT h, gen_random_uuid(), 'query', 'model-' || m, 1
		  FROM generate_series(timestamptz '2026-10-08 00:00Z', timestamptz '2026-11-06 23:00Z', interval '1 hour') h,
		       generate_series(1, 278) m`); err != nil {
		t.Fatalf("internal rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_llm_usage_hourly`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	q, err := llmusage.Align(llmusage.SeriesQuery{From: now.Add(-7 * 24 * time.Hour), To: now, Bucket: llmusage.BucketDay,
		Group: llmusage.GroupAlias, ConsumerIDs: consumers[:2], Now: now})
	if err != nil {
		t.Fatalf("W06-16 gate (plan, V-06-a): Align: %v", err)
	}
	sql := llmusage.SeriesSQL(q)
	if sql == "" {
		t.Fatal("W06-16 gate (plan, V-06-a): SeriesSQL returned no statement")
	}
	if strings.Contains(sql, "IS NULL OR") {
		t.Errorf("W06-16 gate (plan): the series SQL carries an IS NULL OR predicate:\n%s", sql)
	}
	var plan string
	if err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, q.From, q.To, consumers[:2]).Scan(&plan); err != nil {
		t.Errorf("W06-16 gate (plan, V-06-a): explain custom (args $1 from, $2 to, $3 ids): %v", err)
	}
	if !strings.Contains(plan, "idx_llm_usage_consumer") {
		t.Errorf("W06-16 gate (plan, V-06-a): custom plan does not use idx_llm_usage_consumer:\n%s", plan)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	res, err := conn.Conn().PgConn().Exec(ctx, "EXPLAIN (FORMAT JSON, GENERIC_PLAN) "+sql).ReadAll()
	if err != nil || len(res) != 1 || len(res[0].Rows) != 1 {
		t.Fatalf("W06-16 gate (plan, V-06-a): explain generic: %v (%d results)", err, len(res))
	}
	if generic := string(res[0].Rows[0][0]); !strings.Contains(generic, "idx_llm_usage_consumer") {
		t.Errorf("W06-16 gate (plan, V-06-a): generic plan does not use idx_llm_usage_consumer:\n%s", generic)
	}
}
