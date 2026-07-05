//go:build integration

// F5-W6-W4 — performance proof of the overview "landkarte" against the same
// 1M-node / ~3.2M-edge synthetic corpus G39 built (scratch DB ctx_bench, NOT
// prod). Skipped unless CTX_BENCH_DSN points at that DB.
//
// Two tests, because the three §5.3 arms have very different cost profiles and
// must NOT share a failure mode (the first cut put all three in one test → the
// Louvain arm's 30-min non-convergence killed the cheap arms before they ran):
//
//	TestOverviewBenchLouvain     — arm (a): the gonum Louvain rebuild. A SCALING
//	    SWEEP (10k→1M nodes), not a single 1M run, so it shows WHERE Modularize's
//	    cost wall is instead of just timing out. Results print immediately
//	    (fmt.Printf, not t.Logf) so a timeout on the top stage loses nothing.
//	    Diagnostic — never asserts.
//
//	TestOverviewBenchRefreshRead — arms (b)+(c): seeds a SYNTHETIC 1M-member
//	    partition via plain SQL (seconds, independent of whether Louvain could
//	    produce it at scale), then measures (b) the refresh aggregation (the
//	    unbenchmarked 3M-link double-join) and (c) the request path. This isolates
//	    the finding: the bottleneck is Louvain, NOT the aggregation or the read.
//
// Both measure the REAL functions/SQL (loadNodes/loadEdges/computeClustering and
// the nodeAggSQL/edgeAggSQL constants cluster.go runs, plus store.GraphOverview)
// — NO SQL is duplicated into the bench, the G39 discipline.
//
// Corpus + run:  bash .project/bench-graph/run.sh overview   (builds ctx_bench,
// sets the DSN). Manual:
//
//	CTX_BENCH_DSN='postgres://USER:PASS@<db-ip>:5432/ctx_bench?sslmode=disable' \
//	  go test -tags=integration ./internal/overview/ -run TestOverviewBench -v -timeout 1800s
package overview

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

func benchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CTX_BENCH_DSN")
	if dsn == "" {
		t.Skip("CTX_BENCH_DSN unset — run .project/bench-graph/run.sh overview to build ctx_bench first")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("bench: connect ctx_bench: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// benchPercentile returns the p-quantile (0..1) of an ascending-sorted slice
// (nearest-rank, ceil).
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

// benchMeasure runs fn warmup times (discarded), then n timed times, returning
// p50/p95/max. A non-nil error fails the test immediately.
func benchMeasure(t *testing.T, n, warmup int, fn func() error) (p50, p95, mx time.Duration) {
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

func benchMS(d time.Duration) string  { return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000) }
func benchSec(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

// readVmHWMkB reads the process RSS high-water mark (peak resident set) from
// /proc/self/status — the honest "does the rebuild fit on the host" number.
func readVmHWMkB(t *testing.T) int64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Logf("readVmHWM: %v (RSS peak unavailable on this OS)", err)
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "VmHWM:") {
			fields := strings.Fields(line) // ["VmHWM:", "12345", "kB"]
			if len(fields) >= 2 {
				v, _ := strconv.ParseInt(fields[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

func mbFromKB(kb int64) string    { return fmt.Sprintf("%.0f MB", float64(kb)/1024) }
func mbFromBytes(b uint64) string { return fmt.Sprintf("%.0f MB", float64(b)/1024/1024) }

// loadCorpus reads the full node + edge set once (timed), the input to arm (a).
func loadCorpus(t *testing.T, ctx context.Context, pool *pgxpool.Pool) ([]string, []rawEdge) {
	t.Helper()
	t0 := time.Now()
	nodes, _, err := loadNodes(ctx, pool, []string{"knowledge", "audit-trail"})
	if err != nil {
		t.Fatalf("loadNodes: %v", err)
	}
	loadNodesDur := time.Since(t0)

	t1 := time.Now()
	edges, err := loadEdges(ctx, pool)
	if err != nil {
		t.Fatalf("loadEdges: %v", err)
	}
	loadEdgesDur := time.Since(t1)

	if len(nodes) < 900_000 {
		t.Fatalf("corpus too small (%d nodes) — run .project/bench-graph/run.sh overview first", len(nodes))
	}
	fmt.Printf("[load] nodes=%d (%s)  edges=%d (%s)\n", len(nodes), benchMS(loadNodesDur), len(edges), benchMS(loadEdgesDur))
	return nodes, edges
}

// TestOverviewBenchLouvain — arm (a). Scaling sweep of computeClustering (the
// in-memory gonum Modularize) over growing node prefixes of the same 1M corpus.
// computeClustering drops dangling edges by node-set membership, so passing
// nodes[:n] with all edges yields the induced subgraph automatically — its edge
// density grows ~quadratically with n, mirroring the real load. Diagnostic only.
func TestOverviewBenchLouvain(t *testing.T) {
	pool := benchPool(t)
	ctx := context.Background()
	nodes, edges := loadCorpus(t, ctx, pool)

	fmt.Printf("=== F5-W6-W4 arm (a): gonum Louvain scaling sweep (single-threaded Modularize, seed fixed) ===\n")
	fmt.Printf("%-9s %12s %10s %12s  %s\n", "nodes", "wall-clock", "clusters", "modularity", "note")

	const stageAbort = 240 * time.Second // a stage past this ⇒ the next is hopeless; stop the sweep
	stages := []int{10_000, 25_000, 50_000, 100_000, 200_000, 400_000, 700_000, 1_000_000}
	for _, n := range stages {
		if n > len(nodes) {
			n = len(nodes)
		}
		runtime.GC()
		t0 := time.Now()
		cl := computeClustering(nodes[:n], edges, 1.0)
		dur := time.Since(t0)
		note := ""
		if dur > stageAbort {
			note = "← exceeds stage budget; larger stages skipped (extrapolate)"
		}
		fmt.Printf("%-9d %12s %10d %12.4f  %s\n", n, benchSec(dur), cl.clusterCount, cl.modularity, note)
		if dur > stageAbort {
			break
		}
		if n == len(nodes) {
			break
		}
	}
	vmPeak := readVmHWMkB(t)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("[ram] process RSS high-water (VmHWM) = %s   Go Sys = %s\n", mbFromKB(vmPeak), mbFromBytes(ms.Sys))
	fmt.Printf("[note] graph build (load+symmetrize+SetWeightedEdge) is cheap & RAM-bound; the wall-clock above is dominated by Modularize, which is single-threaded and CPU-bound.\n")
}

// seedSyntheticPartition fills graph_cluster_member with a random nClusters-way
// partition (cluster_id = min member uuid per bucket, exactly the Louvain
// invariant) WITHOUT running Louvain — so arms (b)/(c) can be measured at the 1M
// target scale even though the gonum Louvain run itself does not converge there.
// A random partition is PESSIMISTIC for supergraph density (real Louvain
// maximizes intra-cluster edges → a sparser meta-graph), so the (b)/(c) numbers
// are an upper bound, not a flattering one.
func seedSyntheticPartition(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nClusters int) (members, clusters int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `TRUNCATE graph_cluster_member`); err != nil {
		t.Fatalf("seed partition: truncate: %v", err)
	}
	// cluster_id = lexicographically smallest member uuid per bucket — the exact
	// content-stable rule cluster.go uses (it compares uuids as strings: u <
	// minUUID). PG has no min(uuid) aggregate, so min over the text form, cast back.
	tag, err := pool.Exec(ctx, `
		INSERT INTO graph_cluster_member (block_id, cluster_id)
		SELECT id, (min(id::text) OVER (PARTITION BY abs(hashtextextended(id::text, 0)) % $1))::uuid
		FROM context_blocks
		WHERE NOT is_archived AND type_name <> 'system-meta'`, nClusters)
	if err != nil {
		t.Fatalf("seed partition: insert: %v", err)
	}
	members = int(tag.RowsAffected())
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT cluster_id) FROM graph_cluster_member`).Scan(&clusters); err != nil {
		t.Fatalf("seed partition: count clusters: %v", err)
	}
	return members, clusters
}

// fillAggregates runs the real nodeAggSQL/edgeAggSQL (the constants cluster.go
// uses) persistently against the seeded member table, so arm (c) reads populated
// tables. Returns the edge-row count (the read surface for arm (c)).
func fillAggregates(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (nodeRows, edgeRows int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `TRUNCATE graph_cluster_node, graph_cluster_edge`); err != nil {
		t.Fatalf("fillAggregates: truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, nodeAggSQL); err != nil {
		t.Fatalf("fillAggregates: nodeAggSQL: %v", err)
	}
	if _, err := pool.Exec(ctx, edgeAggSQL); err != nil {
		t.Fatalf("fillAggregates: edgeAggSQL: %v", err)
	}
	// Per-scope meta rows since B-W5 (088) — replace-all like the global run.
	if _, err := pool.Exec(ctx, `DELETE FROM graph_overview_meta`); err != nil {
		t.Fatalf("fillAggregates: meta teardown: %v", err)
	}
	if _, err := pool.Exec(ctx, metaWriteGlobalSQL, 0.0, 1.0); err != nil {
		t.Fatalf("fillAggregates: meta: %v", err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_node`).Scan(&nodeRows)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_edge`).Scan(&edgeRows)
	return nodeRows, edgeRows
}

// timeAggInTx times one aggregation INSERT (the constant cluster.go runs) in
// isolation: TRUNCATE the target, run the INSERT, ROLLBACK — leaving the
// persisted state intact for arm (c). Returns the median wall-clock over n runs.
// `table` is a hard-wired literal, never caller input.
func timeAggInTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, aggSQL string, n int) time.Duration {
	t.Helper()
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("agg %s: begin: %v", table, err)
		}
		if _, err := tx.Exec(ctx, "TRUNCATE "+table); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("agg %s: truncate: %v", table, err)
		}
		start := time.Now()
		if _, err := tx.Exec(ctx, aggSQL); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("agg %s: insert: %v", table, err)
		}
		durs = append(durs, time.Since(start))
		_ = tx.Rollback(ctx)
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return durs[len(durs)/2]
}

// explainEdgeAgg logs the EXPLAIN (ANALYZE) plan of the heavy (C) double-join so
// the report shows the join strategy (a bad plan, not raw size, is what blows
// the refresh budget — the §3.3 MATERIALIZED concern). Runs in a rolled-back tx.
func explainEdgeAgg(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Logf("explain: begin: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "TRUNCATE graph_cluster_edge"); err != nil {
		t.Logf("explain: truncate: %v", err)
		return
	}
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, TIMING) "+edgeAggSQL)
	if err != nil {
		t.Logf("explain: %v", err)
		return
	}
	defer rows.Close()
	fmt.Printf("--- EXPLAIN ANALYZE edgeAggSQL (the (C) 3M-link double-join) ---\n")
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			break
		}
		fmt.Printf("  %s\n", line)
	}
}

// TestOverviewBenchRefreshRead — arms (b) + (c) at the 1M target scale against a
// synthetic partition. (b) is reported (single off-peak job); (c) has a hard p95
// threshold (design §5.3: <50ms warm; >200ms is the §4-W4 ABORT trigger).
func TestOverviewBenchRefreshRead(t *testing.T) {
	pool := benchPool(t)
	ctx := context.Background()

	const nClusters = 1000 // pessimistic-dense "landkarte" (real Louvain → fewer/sparser)
	members, clusters := seedSyntheticPartition(t, ctx, pool, nClusters)
	fmt.Printf("=== F5-W6-W4 arms (b)+(c): synthetic %d-way partition over the 1M corpus ===\n", nClusters)
	fmt.Printf("[seed] graph_cluster_member = %d rows, %d distinct clusters\n", members, clusters)

	// ---- Arm (b): the (C) aggregation — the off-peak daemon refresh budget ----
	nodeRows, edgeRows := fillAggregates(t, ctx, pool)
	nodeAggDur := timeAggInTx(t, ctx, pool, "graph_cluster_node", nodeAggSQL, 3)
	edgeAggDur := timeAggInTx(t, ctx, pool, "graph_cluster_edge", edgeAggSQL, 3)
	fmt.Printf("Arm (b) refresh aggregation (median of 3, off-peak daemon budget):\n")
	fmt.Printf("  nodeAggSQL (1M-block scan ⋈ member)        = %s\n", benchMS(nodeAggDur))
	fmt.Printf("  edgeAggSQL (3M-link double-join ⋈ member)  = %s   ← the (C) path G39 never measured\n", benchMS(edgeAggDur))
	fmt.Printf("  aggregate tables (arm (c) read surface): graph_cluster_node=%d rows, graph_cluster_edge=%d rows\n", nodeRows, edgeRows)
	explainEdgeAgg(t, ctx, pool)

	// ---- Arm (c): request path — store.GraphOverview over the small tables ----
	params := store.OverviewParams{MinClusterSize: 1, MinInterWeight: 0, NodeLimit: 500, EdgeLimit: 2000}
	for _, tc := range []struct {
		name   string
		scopes []string
	}{
		{"readScopes=[private]", []string{"private"}},
		{"readScopes=[private,bench_other]", []string{"private", "bench_other"}},
	} {
		var last *store.OverviewResult
		p50, p95, mx := benchMeasure(t, 100, 5, func() error {
			r, e := store.GraphOverview(ctx, pool, params, tc.scopes)
			last = r
			return e
		})
		thr := 50 * time.Millisecond
		stat := "PASS"
		if p95 > thr {
			stat = "FAIL"
		}
		fmt.Printf("Arm (c) request %-34s p50=%-9s p95=%-9s max=%-9s thr=%-7s %s  (nodes=%d edges=%d trunc=%v)\n",
			tc.name, benchMS(p50), benchMS(p95), benchMS(mx), benchMS(thr), stat, len(last.Nodes), len(last.Edges), last.Truncated)
		if p95 > 200*time.Millisecond {
			t.Errorf("ABORT (design §4-W4): request %s p95=%s > 200ms — read path is NOT corpus-independent; investigate aggregate-table cardinality / index use",
				tc.name, benchMS(p95))
		} else if p95 > thr {
			t.Errorf("THRESHOLD BREACH: request %s p95=%s > %s — expected sub-10ms, bounded by cluster count not corpus (design §5.3)",
				tc.name, benchMS(p95), benchMS(thr))
		}
	}
}

// TestOverviewBenchReadScaling — arm (c) ceiling sweep. RefreshRead's §4-W4 ABORT
// fired because the read cost is O(visible cluster-PAIRS) = O(min(clusters,
// node_limit)²), NOT O(cluster count) as the design assumed: the edge query's PK
// bitmap scan touches every (cluster_a, cluster_b) pair among the returned nodes,
// then sorts+aggregates them. So node_limit is the tunable ceiling (the G39
// egoMaxLimit analogue), not an index — the pairs must be read regardless.
//
// This holds the DENSEST possible supergraph (1000-way random partition →
// ≈C(n,2) pairs all present, an upper bound; real Louvain is far sparser) and
// sweeps node_limit to find where the read crosses the 50ms target. Diagnostic —
// never asserts.
func TestOverviewBenchReadScaling(t *testing.T) {
	pool := benchPool(t)
	ctx := context.Background()
	scopes := []string{"private"}

	const nClusters = 1000
	seedSyntheticPartition(t, ctx, pool, nClusters)
	_, edgeRows := fillAggregates(t, ctx, pool)
	fmt.Printf("=== F5-W6-W4 arm (c) ceiling sweep: read p95 vs node_limit (readScopes=[private]) ===\n")
	fmt.Printf("corpus: %d-way random partition, graph_cluster_edge=%d rows (densest supergraph, upper bound)\n", nClusters, edgeRows)
	fmt.Printf("%-12s %14s %10s %10s %10s  %s\n", "node_limit", "≈vis_pairs", "p50", "p95", "max", "note")
	for _, nl := range []int{50, 100, 150, 200, 300, 500, 1000} {
		params := store.OverviewParams{MinClusterSize: 1, MinInterWeight: 0, NodeLimit: nl, EdgeLimit: 2000}
		visible := nl
		if visible > nClusters {
			visible = nClusters
		}
		approxPairs := visible * (visible - 1) / 2 // dense supergraph: ≈all pairs present
		var last *store.OverviewResult
		p50, p95, mx := benchMeasure(t, 60, 5, func() error {
			r, e := store.GraphOverview(ctx, pool, params, scopes)
			last = r
			return e
		})
		note := fmt.Sprintf("nodes=%d edges=%d", len(last.Nodes), len(last.Edges))
		if p95 > 50*time.Millisecond {
			note += "  ← over 50ms target"
		}
		fmt.Printf("%-12d %14d %10s %10s %10s  %s\n", nl, approxPairs, benchMS(p50), benchMS(p95), benchMS(mx), note)
	}
}

// TestOverviewBenchStructured — F5-W6-W4 GEGENPROBE. ReadScaling/RefreshRead nutzten
// eine ZUFÄLLIGE Partition → dichtester Supergraph (≈C(n,2) Paare, 500k edge-rows,
// Read reißt). Diese Gegenprobe misst gegen die GROUND-TRUTH-Community-Partition des
// STRUKTURIERTEN Korpus (seed-structured.sql: 1000 Communities, 90% intra / 10% inter
// zu 6 Nachbarn) — was Louvain bei hoher Modularität nahezu findet. Hypothese: der
// Supergraph ist spärlich (jede Community → ~wenige Nachbarn) → graph_cluster_edge
// klein → Read << 50ms auch bei node_limit=500. Nur gegen den strukturierten Korpus
// (sbench_ids); skippt sonst. Diagnostic — never asserts.
func TestOverviewBenchStructured(t *testing.T) {
	pool := benchPool(t)
	ctx := context.Background()

	var hasSbench bool
	_ = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='sbench_ids')").Scan(&hasSbench)
	if !hasSbench {
		t.Skip("kein strukturierter Korpus (sbench_ids fehlt) — .project/bench-graph/seed-structured.sql bauen")
	}

	// Ground-Truth-Community-Partition: cluster_id = min member uuid je Community
	// (comm = (rn-1)/1000), exakt die Louvain-cluster_id-Regel. Deterministisch und
	// unabhängig von der Louvain-Konvergenz (die arm (a) separat misst).
	if _, err := pool.Exec(ctx, "TRUNCATE graph_cluster_member, graph_cluster_node, graph_cluster_edge"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_cluster_member (block_id, cluster_id)
		SELECT s.id, (min(s.id::text) OVER (PARTITION BY (s.rn-1)/1000))::uuid
		FROM sbench_ids s`); err != nil {
		t.Fatalf("seed ground-truth partition: %v", err)
	}
	var members, clusters int
	_ = pool.QueryRow(ctx, "SELECT count(*), count(DISTINCT cluster_id) FROM graph_cluster_member").Scan(&members, &clusters)

	nodeRows, edgeRows := fillAggregates(t, ctx, pool)
	fmt.Printf("=== F5-W6-W4 GEGENPROBE: strukturierter Korpus, Ground-Truth-Partition ===\n")
	fmt.Printf("[seed] %d Member / %d Communities — graph_cluster_node=%d, graph_cluster_edge=%d rows  (Zufalls-Gegenstück: 500311 rows)\n",
		members, clusters, nodeRows, edgeRows)

	scopes := []string{"private"}
	fmt.Printf("%-12s %10s %10s %10s  %s\n", "node_limit", "p50", "p95", "max", "note")
	for _, nl := range []int{50, 100, 200, 500, 1000} {
		params := store.OverviewParams{MinClusterSize: 1, MinInterWeight: 0, NodeLimit: nl, EdgeLimit: 2000}
		var last *store.OverviewResult
		p50, p95, mx := benchMeasure(t, 60, 5, func() error {
			r, e := store.GraphOverview(ctx, pool, params, scopes)
			last = r
			return e
		})
		note := fmt.Sprintf("nodes=%d edges=%d", len(last.Nodes), len(last.Edges))
		if p95 > 50*time.Millisecond {
			note += "  ← über 50ms"
		}
		fmt.Printf("%-12d %10s %10s %10s  %s\n", nl, benchMS(p50), benchMS(p95), benchMS(mx), note)
	}
}
