//go:build integration

// G39 / F5-W5 — performance proof of the ego-graph store against a synthetic
// 1M-node / ~3.2M-edge corpus in the scratch DB ctx_bench (NOT prod). Skipped
// unless CTX_BENCH_DSN points at that DB.
//
// It measures the REAL store functions (EgoGraph, hopNeighbors, fillDegrees) —
// no SQL is duplicated into the bench, so the numbers track graph.go and cannot
// silently drift from the queries the handler actually runs (design §5.3; the
// skeleton ego-latency.sh there left the Q1 pipeline as a placeholder, which is
// exactly the store/wire drift this avoids).
//
// Corpus + run:  .project/bench-graph/run.sh  (builds ctx_bench, sets the DSN).
// Manual:
//   CTX_BENCH_DSN='postgres://USER:PASS@<db-ip>:5432/ctx_bench?sslmode=disable' \
//     go test -tags=integration ./internal/store/ -run TestGraphBench1M -v -timeout 600s
package store

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func benchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CTX_BENCH_DSN")
	if dsn == "" {
		t.Skip("CTX_BENCH_DSN unset — run .project/bench-graph/run.sh to build ctx_bench first")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("bench: connect ctx_bench: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func benchHubID(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
		t.Fatalf("bench: hub %q id (corpus seeded?): %v", title, err)
	}
	return id
}

// benchPercentile returns the p-quantile (0..1) of an ascending-sorted slice
// using nearest-rank (ceil).
func benchPercentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// benchMeasure runs fn warmup times (discarded), then n timed times, and returns
// the p50/p95/max latency. A non-nil error fails the test immediately.
func benchMeasure(t *testing.T, n, warmup int, fn func() error) (p50, p95, max time.Duration) {
	t.Helper()
	for i := 0; i < warmup; i++ {
		if err := fn(); err != nil {
			t.Fatalf("bench warmup: %v", err)
		}
	}
	ds := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("bench iter %d: %v", i, err)
		}
		ds[i] = time.Since(start)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return benchPercentile(ds, 0.50), benchPercentile(ds, 0.95), ds[n-1]
}

func benchMS(d time.Duration) string {
	return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
}

// TestGraphBench1M is the F5-W5 acceptance gate: four arms, each with its own
// p95 threshold (design §5.3). A breach is reported as a test failure AND with
// the documented remedy (lower the ceilings, or rebuild Q2 on per-source LATERAL).
func TestGraphBench1M(t *testing.T) {
	pool := benchPool(t)
	ctx := context.Background()
	scopes := []string{"private"} // does NOT include 'bench_other' → low-visibility arms

	hubSTD := benchHubID(t, pool, "HUB-STD")
	hubAV := benchHubID(t, pool, "HUB-AV10K")
	hubLV := benchHubID(t, pool, "HUB-LV")
	hubCAP := benchHubID(t, pool, "HUB-CAP")
	focusQ2 := benchHubID(t, pool, "FOCUS-Q2")

	type result struct {
		arm                 string
		p50, p95, max, thr  time.Duration
		detail              string
	}
	var results []result

	// --- Arm 1: Standard (all-visible), full pipeline hops=2/cap=25/limit=500 ---
	{
		p := EgoParams{Focus: hubSTD, Hops: 2, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}
		var last *EgoResult
		p50, p95, mx := benchMeasure(t, 100, 5, func() error {
			r, e := EgoGraph(ctx, pool, p, scopes)
			last = r
			return e
		})
		results = append(results, result{"1 Standard (ego h2/cap25/lim500)", p50, p95, mx, 100 * time.Millisecond,
			fmt.Sprintf("nodes=%d edges=%d trunc=%v", len(last.Nodes), len(last.Edges), last.Truncated)})
	}

	// --- Arm 2a: Degree-Q3, all-visible Grad-10k hub (hit-cap path, 201) ---
	{
		ids := []string{hubAV}
		var deg int
		p50, p95, mx := benchMeasure(t, 100, 5, func() error {
			nodes := []GraphNode{{ID: hubAV}}
			e := fillDegrees(ctx, pool, ids, scopes, nodes)
			deg = nodes[0].Degree
			return e
		})
		results = append(results, result{"2a Degree all-visible (G10k → hit-cap)", p50, p95, mx, 10 * time.Millisecond,
			fmt.Sprintf("degree=%d", deg)})
	}

	// --- Arm 2b: Degree-Q3, low-visibility Grad-10k hub (<201 vis → scan-budget) ---
	{
		ids := []string{hubLV}
		var deg int
		p50, p95, mx := benchMeasure(t, 100, 5, func() error {
			nodes := []GraphNode{{ID: hubLV}}
			e := fillDegrees(ctx, pool, ids, scopes, nodes)
			deg = nodes[0].Degree
			return e
		})
		results = append(results, result{"2b Degree low-visibility (G10k, scan-budget 1000)", p50, p95, mx, 10 * time.Millisecond,
			fmt.Sprintf("degree=%d", deg)})
	}

	// --- Arm 3: Q1 hopNeighbors hops=1/cap=25 on 90%-foreign hub (cap-scan depth) ---
	{
		p := EgoParams{Focus: hubCAP, Hops: 1, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}
		var cnt int
		p50, p95, mx := benchMeasure(t, 100, 5, func() error {
			cands, e := hopNeighbors(ctx, pool, []string{hubCAP}, scopes, p)
			cnt = len(cands)
			return e
		})
		results = append(results, result{"3 Cap-scan depth (Q1, 90% foreign, cap25)", p50, p95, mx, 25 * time.Millisecond,
			fmt.Sprintf("visible neighbors=%d", cnt)})
	}

	// --- Arm 4: API worst case at the SHIPPED ceiling, full pipeline
	// hops=3/cap=100/limit=1500/edge=20000. limit=1500 = handler egoMaxLimit,
	// lowered from 5000 by this very benchmark (5000 ran p95 ~850ms; 2000 was
	// marginal at p95 ~490ms/max ~610ms under host load; see the
	// TestGraphBench1MDiag sweep for the evidence and REPORT.md for the decision). ---
	{
		p := EgoParams{Focus: focusQ2, Hops: 3, PerNodeCap: 100, Limit: 1500, EdgeLimit: 20000}
		var last *EgoResult
		p50, p95, mx := benchMeasure(t, 50, 3, func() error {
			r, e := EgoGraph(ctx, pool, p, scopes)
			last = r
			return e
		})
		results = append(results, result{"4 API worst-case (ego h3/cap100/lim1500/edge20k)", p50, p95, mx, 500 * time.Millisecond,
			fmt.Sprintf("nodes=%d edges=%d trunc=%v", len(last.Nodes), len(last.Edges), last.Truncated)})
		// W10 guard: a tiny node set would make the Q2 number meaningless. The arm
		// is only valid if it actually delivered a large set that hit the edge cap.
		if len(last.Nodes) < 1000 {
			t.Errorf("Arm4 delivered only %d nodes — workload too small to exercise the Q2 worst case (check seed-03 Q2 region)", len(last.Nodes))
		}
	}

	// --- Report ---
	t.Logf("=== G39 / F5-W5 — ego latency on ~1M nodes / ~3.2M edges (warm, readScopes=[private]) ===")
	t.Logf("%-50s %9s %9s %9s %8s  %-4s  %s", "arm", "p50", "p95", "max", "thr", "stat", "detail")
	for _, r := range results {
		stat := "PASS"
		if r.p95 > r.thr {
			stat = "FAIL"
		}
		t.Logf("%-50s %9s %9s %9s %8s  %-4s  %s",
			r.arm, benchMS(r.p50), benchMS(r.p95), benchMS(r.max), benchMS(r.thr), stat, r.detail)
	}
	for _, r := range results {
		if r.p95 > r.thr {
			t.Errorf("THRESHOLD BREACH: %q p95=%s > %s — lower the ceilings (limit/edge_limit) OR rebuild Q2 on per-source LATERAL with a per-node edge budget (design §3.3 Q2 note)",
				r.arm, benchMS(r.p95), benchMS(r.thr))
		}
	}
}

// TestGraphBench1MDiag breaks the Arm-4 worst case into its three store phases
// (Q1 walk / Q2 induced edges / Q3 degrees) and sweeps the node `limit` to find
// the ceiling at which the full pipeline p95 drops below 500ms. Diagnostic only —
// never asserts; it informs whether the remedy is a ceiling cut or a Q2 rebuild.
func TestGraphBench1MDiag(t *testing.T) {
	pool := benchPool(t)
	ctx := context.Background()
	scopes := []string{"private"}
	focusQ2 := benchHubID(t, pool, "FOCUS-Q2")

	pMax := EgoParams{Focus: focusQ2, Hops: 3, PerNodeCap: 100, Limit: 5000, EdgeLimit: 20000}

	// One run to capture the worst-case node set for the phase isolation.
	res, err := EgoGraph(ctx, pool, pMax, scopes)
	if err != nil {
		t.Fatalf("diag: ego: %v", err)
	}
	ids := make([]string, len(res.Nodes))
	index := make(map[string]int, len(res.Nodes))
	for i := range res.Nodes {
		ids[i] = res.Nodes[i].ID
		index[res.Nodes[i].ID] = i
	}

	_, indP95, _ := benchMeasure(t, 20, 3, func() error {
		_, _, e := inducedEdges(ctx, pool, ids, index, pMax)
		return e
	})
	_, degP95, _ := benchMeasure(t, 20, 3, func() error {
		nn := make([]GraphNode, len(res.Nodes))
		copy(nn, res.Nodes)
		return fillDegrees(ctx, pool, ids, scopes, nn)
	})
	_, fullP95, _ := benchMeasure(t, 20, 3, func() error {
		_, e := EgoGraph(ctx, pool, pMax, scopes)
		return e
	})

	t.Logf("=== Arm-4 phase breakdown (node set = %d) ===", len(ids))
	t.Logf("Q2 inducedEdges p95 = %s", benchMS(indP95))
	t.Logf("Q3 fillDegrees  p95 = %s", benchMS(degP95))
	t.Logf("full pipeline   p95 = %s   (Q1 walk ≈ %s)", benchMS(fullP95), benchMS(fullP95-indP95-degP95))

	t.Logf("=== node-limit sweep (hops=3/cap=100/edge=20000, 60 iters) — find the <500ms ceiling with margin ===")
	for _, lim := range []int{3000, 2500, 2000, 1500} {
		p := EgoParams{Focus: focusQ2, Hops: 3, PerNodeCap: 100, Limit: lim, EdgeLimit: 20000}
		var last *EgoResult
		p50, p95, mx := benchMeasure(t, 60, 5, func() error {
			r, e := EgoGraph(ctx, pool, p, scopes)
			last = r
			return e
		})
		flag := "ok"
		if p95 > 500*time.Millisecond {
			flag = "OVER"
		}
		t.Logf("limit=%-4d → p50=%-9s p95=%-9s max=%-9s nodes=%-4d edges=%-5d  %s",
			lim, benchMS(p50), benchMS(p95), benchMS(mx), len(last.Nodes), len(last.Edges), flag)
	}
}
