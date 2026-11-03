//go:build integration

// The reference side of the W06-5 llm_24h gates: the seed every golden case
// lays out, and the raw-row status queries as they stood before W06-5
// (internal/handler/status.go and status_tenant.go at 14b99639) as the
// reference the rollup readers are held against.
package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// refLLM24hSQL is queryLLM24h before W06-5, verbatim except for two things:
// now() is the bound $1, so a case can lay rows exactly on the window's
// boundaries, and the ORDER BY carries a tie-break (backend, pipeline). The
// raw query ordered by calls alone; rows with as many calls came in plan
// order. Both paths now break the tie the same way (W06-5 deviation 1).
const refLLM24hSQL = `
		SELECT COALESCE(backend_name, host) AS backend, pipeline,
		       count(*)::int AS calls,
		       COALESCE(avg(duration_ms), 0)::int AS avg_ms,
		       (count(*) FILTER (WHERE error IS NOT NULL))::int AS errors,
		       COALESCE(sum(prompt_tokens), 0)::bigint AS prompt_tokens,
		       COALESCE(sum(completion_tokens), 0)::bigint AS completion_tokens,
		       COALESCE(sum(cost_usd), 0)::float8 AS cost_usd,
		       bool_and(backend_name IS NOT NULL OR error IS NOT NULL) AS attributed
		FROM context_llm_log
		WHERE created_at > $1::timestamptz - interval '24 hours'
		GROUP BY 1, 2
		ORDER BY calls DESC, backend, pipeline`

// refTenantLLM24hSQL is buildTenantRollupInto's query before W06-5, with the
// same two changes as refLLM24hSQL.
const refTenantLLM24hSQL = `
		SELECT cts.scope,
		       COALESCE(cll.backend_name, cll.host) AS backend, cll.pipeline,
		       count(*)::int AS calls,
		       COALESCE(avg(cll.duration_ms), 0)::int AS avg_ms,
		       (count(*) FILTER (WHERE cll.error IS NOT NULL))::int AS errors,
		       COALESCE(sum(cll.prompt_tokens), 0)::bigint AS prompt_tokens,
		       COALESCE(sum(cll.completion_tokens), 0)::bigint AS completion_tokens,
		       COALESCE(sum(cll.cost_usd), 0)::float8 AS cost_usd
		FROM context_llm_log cll
		JOIN context_api_keys cak ON cll.api_key_id = cak.id
		JOIN context_tenant_scopes cts ON cak.tenant_id = cts.tenant_id
		WHERE cll.created_at > $1::timestamptz - interval '24 hours'
		GROUP BY cts.scope, 2, cll.pipeline
		ORDER BY cts.scope, calls DESC, backend, pipeline`

// llm24hCase is one golden case: the server view with its completeness flag
// and the whole tenant generation, for one window anchor.
type llm24hCase struct {
	Anchor   string                 `json:"anchor"`
	Server   []llm24hRow            `json:"server"`
	Complete bool                   `json:"complete"`
	Tenant   map[string][]llm24hRow `json:"tenant"`
}

// refLLM24h runs both reference queries for the window ending at now, scanned
// exactly as the code of before W06-5 scanned them.
func refLLM24h(ctx context.Context, pool pgxdb.Querier, now time.Time) (llm24hCase, error) {
	out := llm24hCase{Anchor: now.UTC().Format(time.RFC3339Nano), Server: []llm24hRow{}, Complete: true, Tenant: map[string][]llm24hRow{}}
	rows, err := pool.Query(ctx, refLLM24hSQL, now)
	if err != nil {
		return out, fmt.Errorf("reference server query: %w", err)
	}
	for rows.Next() {
		var r llm24hRow
		var attributed bool
		if err := rows.Scan(&r.Backend, &r.Pipeline, &r.Calls, &r.AvgMs, &r.Errors,
			&r.PromptTokens, &r.CompletionTokens, &r.CostUSD, &attributed); err != nil {
			rows.Close()
			return out, fmt.Errorf("reference server scan: %w", err)
		}
		if !attributed {
			out.Complete = false
		}
		out.Server = append(out.Server, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows, err = pool.Query(ctx, refTenantLLM24hSQL, now)
	if err != nil {
		return out, fmt.Errorf("reference tenant query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var r llm24hRow
		if err := rows.Scan(&scope, &r.Backend, &r.Pipeline, &r.Calls, &r.AvgMs,
			&r.Errors, &r.PromptTokens, &r.CompletionTokens, &r.CostUSD); err != nil {
			return out, fmt.Errorf("reference tenant scan: %w", err)
		}
		out.Tenant[scope] = append(out.Tenant[scope], r)
	}
	return out, rows.Err()
}

// llm24hAnchors are the golden window ends: mid-hour, since exactly on a
// full hour (the edge is then a whole hour, Bounds), and since one
// microsecond before a full hour (the edge holds no instant at all).
var llm24hAnchors = []time.Time{
	time.Date(2026, 11, 3, 10, 20, 30, 123456000, time.UTC),
	time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
	time.Date(2026, 11, 3, 10, 59, 59, 999999000, time.UTC),
}

// goldenSeed names the identities of seedLLM24hGolden.
type goldenSeed struct {
	keyA1, keyA2, keyB, keyC string // tenant a (two scopes), tenant b, tenant c (no scope)
	consumer                 string
}

// seedLLM24hIdentities creates the golden tenants, scopes and keys.
func seedLLM24hIdentities(ctx context.Context, t *testing.T, pool *pgxpool.Pool) goldenSeed {
	t.Helper()
	var s goldenSeed
	mkTenant := func(slug string, scopes ...string) string {
		var tid string
		if err := pool.QueryRow(ctx, `INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&tid); err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		for _, sc := range scopes {
			if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, sc, tid); err != nil {
				t.Fatalf("scope %s: %v", sc, err)
			}
		}
		return tid
	}
	mkKey := func(label, tid string) string {
		var kid string
		if err := pool.QueryRow(ctx, `WITH p AS (
			INSERT INTO context_principals (display_name) VALUES ($1) RETURNING id
		) INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		  SELECT $2, $1, $1, $3::uuid, p.id FROM p RETURNING id::text`, label, "sk-"+label, tid).Scan(&kid); err != nil {
			t.Fatalf("key %s: %v", label, err)
		}
		return kid
	}
	ta := mkTenant("w065-a", "w065-a", "w065-a2")
	tb := mkTenant("w065-b", "w065-b")
	tc := mkTenant("w065-c")
	s.keyA1, s.keyA2 = mkKey("w065-a1", ta), mkKey("w065-a2", ta)
	s.keyB, s.keyC = mkKey("w065-b", tb), mkKey("w065-c", tc)
	s.consumer = "0196f0a0-0000-7000-8000-0000000065c1"
	return s
}

// seedLLM24hGolden lays the golden log around the window ending at now:
// three backends plus two host fallbacks, six pipelines, errors with and
// without a backend_name, a successful row without one (complete = false),
// an empty backend_name and an empty host, NULL durations, tokens and costs,
// half-way averages, three groups tied on calls, consumer rows (consumer_id,
// no api_key_id) and background rows, two tenants — one with two scopes — and
// a key of a tenant without a scope; every group with rows on both sides of
// the edge's end, and for each a heavy row outside the window that would
// show if it leaked. Positions are relative to since = now − 24 h and
// edgeEnd (llmusage.Bounds), so a position's side of the window can depend on
// the anchor; the golden file holds one case per anchor.
func seedLLM24hGolden(ctx context.Context, t *testing.T, pool *pgxpool.Pool, now time.Time) goldenSeed {
	t.Helper()
	s := seedLLM24hIdentities(ctx, t, pool)
	since := now.UTC().Truncate(time.Microsecond).Add(-24 * time.Hour)
	edgeEnd := since.Truncate(time.Hour).Add(time.Hour)
	us := time.Microsecond
	in := []time.Time{ // inside the window for every anchor but where noted
		since.Add(us), // edge (the rollup for the 1 µs edge)
		since.Add(edgeEnd.Sub(since) / 2).Truncate(us), // middle of the edge (since itself for the 1 µs edge: out)
		edgeEnd.Add(-us), // last edge instant (since for the 1 µs edge: out)
		edgeEnd,          // first rollup hour
		edgeEnd.Add(30 * time.Minute),
		now.Add(-time.Hour),
		now,
		now.Add(90 * time.Minute), // after now: counts, as created_at > since has no upper bound
	}
	out := []time.Time{since, since.Add(-us), since.Add(-time.Hour), now.Add(-48 * time.Hour)}

	type row struct {
		pipeline, host       string
		backend, errText     *string
		duration, ptok, ctok *int
		cost                 *string
		key, consumer        *string
	}
	str := func(v string) *string { return &v }
	num := func(v int) *int { return &v }
	k := func(v string) *string { return &v }
	profiles := []row{
		// spark-chat/dream-eval: 5 rows, one failed; a NULL duration; background, tenant a and a consumer
		{pipeline: "dream-eval", host: "spark", backend: str("spark-chat"), duration: num(100), ptok: num(1000), ctok: num(10)},
		{pipeline: "dream-eval", host: "spark", backend: str("spark-chat"), duration: num(200), ptok: num(2000), key: k(s.keyA1)},
		{pipeline: "dream-eval", host: "spark", backend: str("spark-chat"), errText: str("boom"), key: k(s.keyA1)},
		{pipeline: "dream-eval", host: "spark", backend: str("spark-chat"), duration: num(301), ctok: num(7), consumer: &s.consumer},
		{pipeline: "dream-eval", host: "spark", backend: str("spark-chat"), duration: num(400), ptok: num(5), ctok: num(5)},
		// three groups of three calls (a tie): averages half-way between integers
		{pipeline: "query-synthesize", host: "spark", backend: str("spark-chat"), duration: num(1), cost: str("0.1"), key: k(s.keyA1)},
		{pipeline: "query-synthesize", host: "spark", backend: str("spark-chat"), duration: num(2), cost: str("0.2"), key: k(s.keyB)},
		{pipeline: "query-synthesize", host: "spark", backend: str("spark-chat"), key: k(s.keyA2)},
		{pipeline: "web-chat", host: "spark", backend: str("spark-chat"), duration: num(2), key: k(s.keyB)},
		{pipeline: "web-chat", host: "spark", backend: str("spark-chat"), duration: num(3), key: k(s.keyB)},
		{pipeline: "web-chat", host: "spark", backend: str("spark-chat"), duration: num(3), errText: str("late"), key: k(s.keyB)},
		{pipeline: "query-synthesize", host: "api.example", backend: str("ext-openai"), duration: num(1500), cost: str("0.12345678"), ptok: num(11), ctok: num(22), consumer: &s.consumer},
		{pipeline: "query-synthesize", host: "api.example", backend: str("ext-openai"), duration: num(2500), cost: str("0.12345678"), errText: str("429"), consumer: &s.consumer},
		{pipeline: "query-synthesize", host: "api.example", backend: str("ext-openai"), duration: num(999), cost: str("0.12345678"), key: k(s.keyA1)},
		// host fallback, successful and un-attributed: llm_24h_complete = false
		{pipeline: "rerank", host: "herbert", duration: num(5), key: k(s.keyA2)},
		{pipeline: "rerank", host: "herbert", duration: num(6)},
		// host fallback on failed rows only: a known failure, still attributed
		{pipeline: "query-embed", host: "spark-embed-host", errText: str("timeout"), key: k(s.keyC)},
		{pipeline: "query-embed", host: "spark-embed-host", duration: num(9), errText: str("timeout"), key: k(s.keyA1)},
		// backend '' from both sides: a NULL backend_name over host '', and backend_name ''
		{pipeline: "embed-backfill", host: "", duration: num(7)},
		{pipeline: "embed-backfill", host: "h", backend: str(""), duration: num(8), key: k(s.keyB)},
		// no duration, no tokens, no cost at all
		{pipeline: "dream-keyword-embed", host: "spark", backend: str("spark-embed")},
	}
	insert := func(at time.Time, r row, heavy bool) {
		t.Helper()
		dur, cost, ptok := r.duration, r.cost, r.ptok
		if heavy {
			dur, cost, ptok = num(99999), str("9.9"), num(99999)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_llm_log (created_at, pipeline, model, host, backend_name, duration_ms, error,
			                             prompt_tokens, completion_tokens, cost_usd, api_key_id, consumer_id)
			VALUES ($1, $2, 'm', $3, $4, $5, $6, $7, $8, $9::numeric, $10::uuid, $11::uuid)`,
			at, r.pipeline, r.host, r.backend, dur, r.errText, ptok, r.ctok, cost, r.key, r.consumer); err != nil {
			t.Fatalf("seed row %s at %s: %v", r.pipeline, at, err)
		}
	}
	for i, r := range profiles {
		insert(in[i%len(in)], r, false)
		insert(in[(i+3)%len(in)], r, false) // a second row of the profile on another side of edgeEnd
		insert(out[i%len(out)], r, true)
	}
	return s
}
