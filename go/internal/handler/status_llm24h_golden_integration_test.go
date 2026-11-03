//go:build integration

// W06-5 gates against a real PG18 + TimescaleDB with migration 159: the
// llm_24h rollups read from the hourly rollup plus the raw edge hour
// (llmusage.Groups / TenantGroups) are byte-equal to the raw-row queries they
// replace (golden), llm_24h_complete follows unattributed_ok on both sides of
// the edge, and the tenant view never counts a consumer's rows.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'LLM24h|TenantRollup' -count=1 -v
//
// CTX_W065_GOLDEN_UPDATE=1 rewrites testdata/llm24h_golden.json from the
// reference queries (status_llm24h_ref_integration_test.go);
// CTX_W065_GOLDEN_DUMP=<file> writes what the rollup readers returned.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

var llm24hGoldenFile = filepath.Join("testdata", "llm24h_golden.json")

// llm24hCollector is a collector over pool whose llm_24h window ends at now.
func llm24hCollector(pool *pgxpool.Pool, now time.Time) *StatusCollector {
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)
	c.llm24hNow = func() time.Time { return now }
	return c
}

// prodLLM24h runs the production readers — queryLLM24h and the tenant
// generation build — for the window ending at now.
func prodLLM24h(ctx context.Context, t *testing.T, pool *pgxpool.Pool, now time.Time) llm24hCase {
	t.Helper()
	c := llm24hCollector(pool, now)
	server, complete := c.queryLLM24h(ctx)
	if n := c.dbFails.Load(); n != 0 {
		t.Fatalf("queryLLM24h noted %d DB failures", n)
	}
	c.buildTenantRollupInto(ctx)
	gen := c.tenantRollup.Load()
	if gen == nil {
		t.Fatal("buildTenantRollupInto stored no generation")
	}
	return llm24hCase{Anchor: now.UTC().Format(time.RFC3339Nano), Server: server, Complete: complete, Tenant: *gen}
}

func marshalCases(t *testing.T, cases []llm24hCase) []byte {
	t.Helper()
	b, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// firstLineDiff names the first line where got and want differ, with the
// anchor of the case it lies in.
func firstLineDiff(got, want []byte) string {
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	anchor := ""
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if strings.Contains(wl, `"anchor"`) {
			anchor = strings.TrimSpace(wl)
		}
		if gl != wl {
			return fmt.Sprintf("line %d (case %s): got %q, want %q", i+1, anchor, strings.TrimSpace(gl), strings.TrimSpace(wl))
		}
	}
	return "no line differs"
}

// TestStatusLLM24hGolden (gate b): for every golden anchor, the server rows
// with llm_24h_complete and the whole tenant generation of the production
// path serialise byte-equal to the reference queries (the raw-row SQL of
// before W06-5) and to testdata/llm24h_golden.json, which those queries wrote
// on the base tree. The seed puts rows into the raw edge and into rollup
// hours, so both halves of each reader carry figures.
func TestStatusLLM24hGolden(t *testing.T) {
	ctx := context.Background()
	var ref, prod []llm24hCase
	for _, anchor := range llm24hAnchors {
		pool := testdb.SetupTestDB(t)
		seedLLM24hGolden(ctx, t, pool, anchor)

		since, edgeEnd := llmusage.Bounds(anchor, llm24hSpan)
		var edgeRows, rollRows, outRows int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE created_at > $1 AND created_at < $2),
			       count(*) FILTER (WHERE created_at >= $2),
			       count(*) FILTER (WHERE created_at <= $1)
			  FROM context_llm_log`, since, edgeEnd).Scan(&edgeRows, &rollRows, &outRows); err != nil {
			t.Fatal(err)
		}
		t.Logf("anchor %s: edge (%s, %s) %d rows, rollup hours %d rows, outside %d rows",
			anchor.Format(time.RFC3339Nano), since.Format(time.RFC3339Nano), edgeEnd.Format(time.RFC3339), edgeRows, rollRows, outRows)
		if edgeEnd.Sub(since) > time.Microsecond && edgeRows == 0 {
			t.Errorf("anchor %s: no row in the raw edge — the golden no longer probes the edge", anchor)
		}

		r, err := refLLM24h(ctx, pool, anchor)
		if err != nil {
			t.Fatal(err)
		}
		ref = append(ref, r)
		prod = append(prod, prodLLM24h(ctx, t, pool, anchor))
	}

	refJSON, prodJSON := marshalCases(t, ref), marshalCases(t, prod)
	if path := os.Getenv("CTX_W065_GOLDEN_DUMP"); path != "" {
		if err := os.WriteFile(path, prodJSON, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("CTX_W065_GOLDEN_UPDATE") == "1" {
		if err := os.WriteFile(llm24hGoldenFile, refJSON, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s (%d bytes)", llm24hGoldenFile, len(refJSON))
	}
	golden, err := os.ReadFile(llm24hGoldenFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(refJSON) != string(golden) {
		t.Errorf("reference queries no longer reproduce %s: %s", llm24hGoldenFile, firstLineDiff(refJSON, golden))
	}
	if string(prodJSON) != string(golden) {
		t.Errorf("rollup readers differ from %s (the raw-row result): %s", llm24hGoldenFile, firstLineDiff(prodJSON, golden))
	}
	t.Logf("%d cases, golden %d bytes, reference %d bytes, rollup readers %d bytes", len(prod), len(golden), len(refJSON), len(prodJSON))
}

// TestStatusLLM24hCompleteFromRollup (gate c): llm_24h_complete is false as
// soon as one SUCCESSFUL row without a backend_name lies in the window —
// counted by the rollup (unattributed_ok) for a rollup hour and by the raw
// edge for the edge — and stays true for FAILED rows without one.
func TestStatusLLM24hCompleteFromRollup(t *testing.T) {
	ctx := context.Background()
	anchor := llm24hAnchors[0]
	since, edgeEnd := llmusage.Bounds(anchor, llm24hSpan)
	inEdge := since.Add(time.Minute)
	inRollup := anchor.Add(-2 * time.Hour)

	type logRow struct {
		at      time.Time
		backend *string
		errText *string
	}
	str := func(v string) *string { return &v }
	cases := []struct {
		name string
		rows []logRow
		want bool
	}{
		{"success without backend_name in a rollup hour", []logRow{
			{at: inRollup, backend: str("spark-chat")}, {at: inEdge, backend: str("spark-chat")}, {at: inRollup},
		}, false},
		{"success without backend_name in the raw edge", []logRow{
			{at: inRollup, backend: str("spark-chat")}, {at: inEdge, backend: str("spark-chat")}, {at: inEdge},
		}, false},
		{"failures without backend_name in the edge and a rollup hour", []logRow{
			{at: inRollup, backend: str("spark-chat")}, {at: inEdge, errText: str("timeout")}, {at: inRollup, errText: str("refused")},
		}, true},
		{"attributed rows only, backend_name '' included", []logRow{
			{at: inRollup, backend: str("spark-chat")}, {at: inEdge, backend: str("")}, {at: inRollup, backend: str("")},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			for _, r := range tc.rows {
				if _, err := pool.Exec(ctx, `
					INSERT INTO context_llm_log (created_at, pipeline, model, host, backend_name, duration_ms, error)
					VALUES ($1, 'query-synthesize', 'm', 'herbert', $2, 10, $3)`, r.at, r.backend, r.errText); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			var edgeRows, rollUnattributed int64
			if err := pool.QueryRow(ctx, `
				SELECT (SELECT count(*) FROM context_llm_log WHERE created_at > $1 AND created_at < $2),
				       (SELECT COALESCE(sum(unattributed_ok), 0) FROM context_llm_usage_hourly WHERE bucket_start >= $2)`,
				since, edgeEnd).Scan(&edgeRows, &rollUnattributed); err != nil {
				t.Fatal(err)
			}
			if edgeRows == 0 {
				t.Fatalf("no row in the raw edge (%s, %s) — the case does not probe the edge", since, edgeEnd)
			}
			rows, complete := llm24hCollector(pool, anchor).queryLLM24h(ctx)
			if complete != tc.want {
				t.Errorf("llm_24h_complete = %v, want %v (rollup unattributed_ok %d, edge rows %d, rows %+v)",
					complete, tc.want, rollUnattributed, edgeRows, rows)
			}
			ref, err := refLLM24h(ctx, pool, anchor)
			if err != nil {
				t.Fatal(err)
			}
			if ref.Complete != tc.want {
				t.Fatalf("the reference query says complete = %v — the case is wrong", ref.Complete)
			}
		})
	}
}

// TestStatusTenantRollupExcludesConsumers (gate e): consumer rows — a
// consumer_id, no api_key_id — count in the server view (they are part of the
// load) and in no tenant's view, neither from the raw edge nor from the
// rollup; the tenant sees exactly its knowledge key's rows.
func TestStatusTenantRollupExcludesConsumers(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	anchor := llm24hAnchors[0]
	since, _ := llmusage.Bounds(anchor, llm24hSpan)
	s := seedLLM24hIdentities(ctx, t, pool)
	for _, at := range []time.Time{since.Add(time.Minute), anchor.Add(-2 * time.Hour)} {
		for i := 0; i < 3; i++ {
			if _, err := pool.Exec(ctx, `
				INSERT INTO context_llm_log (created_at, pipeline, model, host, backend_name, duration_ms, cost_usd, consumer_id)
				VALUES ($1, 'gateway', 'm', 'h', 'spark-chat', 10, 0.5, $2::uuid)`, at, s.consumer); err != nil {
				t.Fatalf("consumer row: %v", err)
			}
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_llm_log (created_at, pipeline, model, host, backend_name, duration_ms, cost_usd, api_key_id)
			VALUES ($1, 'gateway', 'm', 'h', 'spark-chat', 10, 0.25, $2::uuid)`, at, s.keyB); err != nil {
			t.Fatalf("key row: %v", err)
		}
	}
	got := prodLLM24h(ctx, t, pool, anchor)
	want := []llm24hRow{{Backend: "spark-chat", Pipeline: "gateway", Calls: 2, AvgMs: 10, CostUSD: 0.5}}
	gb, _ := json.Marshal(got.Tenant)
	wb, _ := json.Marshal(map[string][]llm24hRow{"w065-b": want})
	if string(gb) != string(wb) {
		t.Errorf("tenant generation counts rows that are not the tenant's knowledge key's:\ngot  %s\nwant %s", gb, wb)
	}
	var serverCalls int
	for _, r := range got.Server {
		if r.Backend == "spark-chat" && r.Pipeline == "gateway" {
			serverCalls = r.Calls
		}
	}
	if serverCalls != 8 {
		t.Errorf("server view: spark-chat/gateway calls = %d, want 8 (6 consumer + 2 key rows): %+v", serverCalls, got.Server)
	}
}

// TestStatusLLM24hLongNamesCut pins the one place the rollup readers differ
// from the raw-row aggregate: names longer than the rollup's 100 characters.
// Edge and rollup cut alike, so a long name is one row with the cut name —
// never one cut row from the rollup next to one full row from the edge — and
// two names that differ only after the 100th character share that row.
func TestStatusLLM24hLongNamesCut(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	anchor := llm24hAnchors[0]
	since, _ := llmusage.Bounds(anchor, llm24hSpan)
	prefix, pipe := strings.Repeat("b", 100), strings.Repeat("p", 120)
	for _, at := range []time.Time{since.Add(time.Minute), anchor.Add(-2 * time.Hour)} {
		for _, backend := range []string{prefix + "-first-backend-xxxx", prefix + "-second-backend-yyy"} {
			if _, err := pool.Exec(ctx, `
				INSERT INTO context_llm_log (created_at, pipeline, model, host, backend_name, duration_ms)
				VALUES ($1, $2, 'm', 'h', $3, 10)`, at, pipe, backend); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	got := prodLLM24h(ctx, t, pool, anchor)
	want := []llm24hRow{{Backend: prefix, Pipeline: pipe[:100], Calls: 4, AvgMs: 10}}
	gb, _ := json.Marshal(got.Server)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("long names:\ngot  %s\nwant %s", gb, wb)
	}
	ref, err := refLLM24h(ctx, pool, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(ref.Server) != 2 || len(ref.Server[0].Backend) != 119 {
		t.Errorf("the raw-row reference should keep both full names: %+v", ref.Server)
	}
}
