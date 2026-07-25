//go:build integration

// W05.8 (design/05 §7 Zeile W05.8, §6.1/§6.2/§6.3) — the CACHE arms of the 1M
// benchmark. Arms 1-7 (graph_bench_test.go) are all SQL and all ego-side; this
// file adds what the wave needs to replace §6.2's ESTIMATE with a measurement
// and to put the E-05-1 decision on numbers instead of expectation:
//
//	 8      full CSR rebuild over the 1M corpus: duration plus the RSS peak
//	        DURING the build (sampled). A reading taken after Build returns
//	        cannot see the transient §6.1 budgets — the count/fill passes and
//	        the edge buffer are already freed by then.
//	 9/10   ego through the snapshot arm, mirroring arms 1 and 4 parameter for
//	        parameter so each pair is directly comparable.
//	11-14   GraphExpand, the SQL arm FIRST: Inventur-Naht §9.10 — expand never
//	        had a 1M bench, so the W05.7 cache arm had no baseline to be
//	        compared against. Each arm runs with normal seeds and with 10^4-hub
//	        seeds; the hub variant is the named W05.7 measurement point for the
//	        O(seed degree) hub-damping walk.
//	15-19   Q3 degrees (SQL vs. snapshot over the SAME 1500-node response set),
//	        the §6.3/§6.4 worst case "1500 answer nodes x 10^4 hub" with and
//	        without DegreeWalkBudget, and Q2 induced edges from the snapshot.
//
// MEASUREMENT ONLY. None of these arms carries a latency threshold: whether the
// cache paths go live is E-05-1, and that is the user's decision AFTER these
// numbers exist (design/05 §8). What they DO assert is that they are not
// vacuous — every arm checks that the intended arm actually answered
// (BudgetReport.Source) and that the workload was the intended one. A silent
// SQL fallback (errEgoCacheStale) or an empty graph would otherwise produce a
// beautiful and meaningless number (the W10 lesson of arm 4's node-count guard).
//
// Corpus + run: .project/bench-graph/run.sh (BENCH_SKIP_SEED=1 reuses it).
package store

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/rrf"
)

// benchScratchDB is the ONLY database these arms may touch. ctx runs live on the
// same PostgreSQL instance and graphcache.Build reads the FULL block + link
// universe in three unbounded streams — pointing that at context_store would be
// a heavy read against production. run.sh assembles the DSN from this name; the
// guard below verifies the connection actually landed there, because a bench
// that silently benched prod is worse than one that does not run.
const benchScratchDB = "ctx_bench"

// benchDegreeWalkBudget is the shipped default of graph_cache.degree_walk_budget
// (config/config.go:330) — the value the degree arms calibrate (E-05-3(3)).
const benchDegreeWalkBudget = 4000

// benchGuardScratchDSN fails the run unless the pool is connected to ctx_bench.
func benchGuardScratchDSN(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var db string
	if err := pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatalf("bench: current_database(): %v", err)
	}
	if db != benchScratchDB {
		t.Fatalf("bench: CTX_BENCH_DSN is connected to %q — refusing to run outside the scratch DB %q", db, benchScratchDB)
	}
}

// --- Build: duration + the transient memory peak -----------------------------

// benchBuildStats is one rebuild measurement series.
type benchBuildStats struct {
	durs     []time.Duration // one entry per run, in run order
	rssBase  uint64          // process RSS before the first build
	rssPeak  uint64          // process RSS peak sampled DURING the builds
	rssAfter uint64          // process RSS with the snapshot alive, after FreeOSMemory
	heapPeak uint64          // runtime.MemStats.HeapAlloc peak
	sysPeak  uint64          // runtime.MemStats.Sys peak
	churn    uint64          // TotalAlloc delta of the LAST build (bytes allocated by one build)
	samples  int
	stats    graphcache.BuildStats
}

// benchProcRSS reads the resident set size in bytes from /proc/self/statm
// (field 2 = resident pages). Cheap and stop-the-world-free, which is why the
// RSS sampler can run at 20ms without distorting the very duration it brackets.
func benchProcRSS() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

// benchMemSampler samples process RSS + Go heap until Stop. runtime.MemStats is
// what §6.1/§7 name for the peak; the process RSS beside it is what the host
// actually pays (Go heap peak and RSS diverge — the runtime returns pages
// lazily).
type benchMemSampler struct {
	stop, done     chan struct{}
	rss, heap, sys uint64
	n              int
}

func benchStartMemSampler(every time.Duration) *benchMemSampler {
	s := &benchMemSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		var ms runtime.MemStats
		for {
			select {
			case <-s.stop:
				return
			case <-tick.C:
				if r := benchProcRSS(); r > s.rss {
					s.rss = r
				}
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > s.heap {
					s.heap = ms.HeapAlloc
				}
				if ms.Sys > s.sys {
					s.sys = ms.Sys
				}
				s.n++
			}
		}
	}()
	return s
}

// Stop ends the sampler and establishes the happens-before edge for its fields.
func (s *benchMemSampler) Stop() {
	close(s.stop)
	<-s.done
}

// benchBuildSnapshot runs `runs` full rebuilds and returns the LAST snapshot.
//
// Run 1 builds with NO previous snapshot alive (cold). Every later run keeps the
// PREVIOUS snapshot referenced THROUGH the build (runtime.KeepAlive) — that is
// the production swap situation and exactly the double-buffer transient §6.1
// budgets (old + new + edge buffer), so the RSS peak reported here is that
// number and not a single-snapshot figure.
func benchBuildSnapshot(t *testing.T, pool *pgxpool.Pool, runs int) (*graphcache.Snapshot, benchBuildStats) {
	t.Helper()
	ctx := context.Background()
	var st benchBuildStats

	runtime.GC()
	debug.FreeOSMemory()
	st.rssBase = benchProcRSS()

	sampler := benchStartMemSampler(20 * time.Millisecond)
	var ms0, ms1 runtime.MemStats
	var cur *graphcache.Snapshot
	for i := 0; i < runs; i++ {
		runtime.ReadMemStats(&ms0)
		start := time.Now()
		s, err := graphcache.Build(ctx, pool)
		d := time.Since(start)
		if err != nil {
			sampler.Stop()
			t.Fatalf("bench: graphcache.Build run %d: %v", i+1, err)
		}
		runtime.ReadMemStats(&ms1)
		runtime.KeepAlive(cur) // double buffer: the old snapshot lived through this build
		st.durs = append(st.durs, d)
		st.churn = ms1.TotalAlloc - ms0.TotalAlloc
		cur = s
	}
	sampler.Stop()
	st.rssPeak, st.heapPeak, st.sysPeak, st.samples = sampler.rss, sampler.heap, sampler.sys, sampler.n

	runtime.GC()
	debug.FreeOSMemory()
	st.rssAfter = benchProcRSS()
	st.stats = cur.Stats
	return cur, st
}

func benchMB(b uint64) string { return fmt.Sprintf("%.0fMB", float64(b)/(1<<20)) }

// --- Expand seeds ------------------------------------------------------------

// benchExpandCfg mirrors the shipped graph.* defaults (config/config.go:237-252)
// with the stage enabled. Hub damping is ON — the damping degree is the walk
// whose cost this bench puts a number on.
func benchExpandCfg() rrf.GraphConfig {
	return rrf.GraphConfig{
		Enabled: true, Directed: true, HopDepth: 1,
		SeedCount: 5, SeedScoreFloor: 0.5, PerSeedCap: 3, MaxInjected: 10,
		MinConfidence: 0.75, MinConfidenceRecurrent: 0.8,
		BoostWeight: 0.20, HubDamping: true,
		WeightTopical: 0.5, WeightFactual: 0.9, WeightCausal: 0.9, WeightRecurrent: 1.0,
		NewPlacementFrac: 0.6,
	}
}

// benchExpandSeeds turns block ids into the pre-sorted RRF slice GraphExpand
// consumes. Scores descend gently so all of them clear SeedScoreFloor*top and
// the seed set is exactly the ids handed in (no floor break mid-list).
func benchExpandSeeds(ids []string) []rrf.SearchResult {
	out := make([]rrf.SearchResult, len(ids))
	for i, id := range ids {
		out[i] = rrf.SearchResult{
			ID: id, Title: "bench seed", Category: "benchhub", Scope: "private",
			RRFScore: 1.0 - 0.01*float64(i),
		}
	}
	return out
}

// benchIDsByCategory returns the n lowest-uuid block ids of a seed category.
func benchIDsByCategory(t *testing.T, pool *pgxpool.Pool, category string, n int) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text FROM context_blocks WHERE category = $1 ORDER BY id LIMIT $2`, category, n)
	if err != nil {
		t.Fatalf("bench: ids of category %q: %v", category, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("bench: scan id: %v", err)
		}
		out = append(out, id)
	}
	if len(out) != n {
		t.Fatalf("bench: category %q yielded %d ids, want %d (corpus seeded?)", category, len(out), n)
	}
	return out
}

// benchExpandArm measures one GraphExpand configuration on ONE arm (SQL when
// cache.Snapshot is nil, snapshot otherwise) and asserts that this arm is the
// one that answered — a stale-fallback to SQL would silently turn a cache
// measurement into a second SQL measurement.
func benchExpandArm(t *testing.T, pool *pgxpool.Pool, name string, iters int,
	seeds []rrf.SearchResult, scopes []string, cache rrf.ExpandCache, wantSource string) benchResult {
	t.Helper()
	ctx := context.Background()
	var out []rrf.SearchResult
	var rep *graphcache.BudgetReport
	p50, p95, mx := benchMeasure(t, iters, 2, func() error {
		o, r, e := rrf.GraphExpandCachedWithReport(ctx, pool, seeds, scopes, nil, benchVisibleTypes,
			benchExpandCfg(), cache)
		out, rep = o, r
		return e
	})
	if rep == nil || rep.Source != wantSource {
		src := "<nil report>"
		if rep != nil {
			src = rep.Source
		}
		t.Errorf("%s: arm %q answered instead of %q — measurement is not what it claims", name, src, wantSource)
	}
	injected := len(out) - len(seeds)
	if injected <= 0 {
		t.Errorf("%s: no neighbour injected (in=%d out=%d) — expand workload is vacuous",
			name, len(seeds), len(out))
	}
	return benchResult{name, p50, p95, mx, 0,
		fmt.Sprintf("seeds=%d injected=%d src=%s", len(seeds), injected, rep.Source)}
}

// --- Degrees -----------------------------------------------------------------

// benchCacheDegrees is the Q3 snapshot stage of egoCacheHops.degrees, driven
// directly over a NodeID set: the same MakeDegreeHints + DegreeHitCap wiring, so
// the number below is the cost the ego cache arm actually pays.
func benchCacheDegrees(snap *graphcache.Snapshot, nodes []uint32, hints *graphcache.DegreeHints) (sum, capped int) {
	for _, n := range nodes {
		d, c := snap.Degree(n, hints)
		sum += d
		if c {
			capped++
		}
	}
	return sum, capped
}

// benchNodeIDs resolves delivered block ids to NodeIDs (the ego cache arm's
// entry step). An id the snapshot does not know is a hard failure here: on the
// live path it would trigger the complete SQL fallback, which in a bench would
// mean measuring the wrong arm.
func benchNodeIDs(t *testing.T, snap *graphcache.Snapshot, ids []string) []uint32 {
	t.Helper()
	out := make([]uint32, 0, len(ids))
	for _, id := range ids {
		u, err := uuid.Parse(id)
		if err != nil {
			t.Fatalf("bench: parse node id %q: %v", id, err)
		}
		n, ok := snap.NodeID(u)
		if !ok {
			t.Fatalf("bench: node %s missing from the snapshot — corpus changed under the build?", id)
		}
		out = append(out, n)
	}
	return out
}

// --- The arm block -----------------------------------------------------------

// benchW058Arms runs arms 8-19 and returns their rows for the shared report
// table. It is called from TestGraphBench1M after the SQL arms so both halves
// print in one table against one corpus.
func benchW058Arms(t *testing.T, pool *pgxpool.Pool, scopes []string) []benchResult {
	t.Helper()
	benchGuardScratchDSN(t, pool)
	ctx := context.Background()
	var res []benchResult

	// --- Arm 8: full CSR rebuild (duration + transient RSS peak) ---
	snap, bs := benchBuildSnapshot(t, pool, 3)
	sorted := append([]time.Duration(nil), bs.durs...)
	for i := 1; i < len(sorted); i++ { // 3 values: an insertion sort is the honest tool
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	res = append(res, benchResult{"8 CSR rebuild (full build @1M, 3 runs)",
		sorted[len(sorted)/2], sorted[len(sorted)-1], sorted[len(sorted)-1], 0,
		fmt.Sprintf("nodes=%d dream=%d struct=%d sup=%d dangling=%d/%d",
			bs.stats.Nodes, bs.stats.DreamEdges, bs.stats.StructEdges,
			bs.stats.SupersedesDisplay, bs.stats.DreamDangling, bs.stats.StructDangling)})
	t.Logf("=== W05.8 arm 8 — rebuild raw values (design/05 §6.2 replaces its estimate with these) ===")
	for i, d := range bs.durs {
		regime := "cold (no previous snapshot)"
		if i > 0 {
			regime = "double-buffer (previous snapshot alive through the build)"
		}
		t.Logf("  build run %d: %s   %s", i+1, benchMS(d), regime)
	}
	t.Logf("  RSS base=%s peak(DURING build)=%s after-GC(snapshot alive)=%s | heap peak=%s sys peak=%s | churn/build=%s | samples=%d @20ms",
		benchMB(bs.rssBase), benchMB(bs.rssPeak), benchMB(bs.rssAfter),
		benchMB(bs.heapPeak), benchMB(bs.sysPeak), benchMB(bs.churn), bs.samples)

	cache := EgoCache{Snapshot: snap, Age: time.Since(snap.BuiltAt), DegreeWalkBudget: benchDegreeWalkBudget}

	// --- Arm 9: cache ego, arm-1 parameters ---
	{
		p := EgoParams{Focus: benchHubID(t, pool, "HUB-STD"), Hops: 2, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}
		var last *EgoResult
		p50, p95, mx := benchMeasure(t, 100, 5, func() error {
			r, e := EgoGraphCached(ctx, pool, p, scopes, nil, benchVisibleTypes, cache)
			last = r
			return e
		})
		res = append(res, benchResult{"9 Cache-Ego standard (h2/cap25/lim500)", p50, p95, mx, 0,
			fmt.Sprintf("nodes=%d edges=%d trunc=%v src=%s",
				len(last.Nodes), len(last.Edges), last.Truncated, last.Budget.Source)})
		if last.Budget.Source != graphcache.SourceCache {
			t.Errorf("Arm9 was answered by %q, not the snapshot arm — the cache measurement is a second SQL measurement", last.Budget.Source)
		}
	}

	// --- Arm 10: cache ego, arm-4 parameters (the shipped ceiling) ---
	// The SQL run of the SAME parameters comes first: it is the reference the arm
	// reports its content delta against (the W05.6 order-granularity deviation —
	// u16 confidence fixpoint vs. exact float — can move WHICH node sits at the
	// per-node cap boundary), and its node set feeds the Q2/Q3 arms below, so
	// both arms measure over one identical response set.
	focusQ2 := benchHubID(t, pool, "FOCUS-Q2")
	pWorst := EgoParams{Focus: focusQ2, Hops: 3, PerNodeCap: 100, Limit: 1500, EdgeLimit: 20000}
	worst, err := EgoGraph(ctx, pool, pWorst, scopes, nil, benchVisibleTypes)
	if err != nil {
		t.Fatalf("bench: SQL reference run for the worst-case arms: %v", err)
	}
	{
		var last *EgoResult
		p50, p95, mx := benchMeasure(t, 50, 3, func() error {
			r, e := EgoGraphCached(ctx, pool, pWorst, scopes, nil, benchVisibleTypes, cache)
			last = r
			return e
		})
		res = append(res, benchResult{"10 Cache-Ego worst-case (h3/cap100/lim1500/edge20k)", p50, p95, mx, 0,
			fmt.Sprintf("nodes=%d edges=%d struct=%d trunc=%v src=%s | vs SQL: dn=%+d de=%+d ds=%+d",
				len(last.Nodes), len(last.Edges), len(last.StructEdges), last.Truncated, last.Budget.Source,
				len(last.Nodes)-len(worst.Nodes), len(last.Edges)-len(worst.Edges),
				len(last.StructEdges)-len(worst.StructEdges))})
		if last.Budget.Source != graphcache.SourceCache {
			t.Errorf("Arm10 was answered by %q, not the snapshot arm", last.Budget.Source)
		}
		if len(last.Nodes) < 1000 {
			t.Errorf("Arm10 delivered only %d nodes — too small to exercise the Q2/Q3 worst case", len(last.Nodes))
		}
	}

	// --- Arms 11-14: GraphExpand, SQL baseline FIRST (Inventur-Naht §9.10) ---
	poolSeeds := benchExpandSeeds(benchIDsByCategory(t, pool, "benchq2pool", 5))
	hubSeeds := benchExpandSeeds([]string{
		benchHubID(t, pool, "HUB-Q2-1"), benchHubID(t, pool, "HUB-Q2-2"),
		benchHubID(t, pool, "HUB-Q2-3"), benchHubID(t, pool, "HUB-Q2-4"),
		benchHubID(t, pool, "HUB-Q2-5"),
	})
	expandCache := rrf.ExpandCache{Snapshot: snap, Age: time.Since(snap.BuiltAt)}
	res = append(res,
		benchExpandArm(t, pool, "11 SQL-Expand baseline (5 pool seeds, hop1/cap3)", 50,
			poolSeeds, scopes, rrf.ExpandCache{}, graphcache.SourceSQL),
		benchExpandArm(t, pool, "12 Cache-Expand (5 pool seeds, hop1/cap3)", 50,
			poolSeeds, scopes, expandCache, graphcache.SourceCache),
		benchExpandArm(t, pool, "13 SQL-Expand hub seeds (5x10^4, O(seed-degree))", 20,
			hubSeeds, scopes, rrf.ExpandCache{}, graphcache.SourceSQL),
		benchExpandArm(t, pool, "14 Cache-Expand hub seeds (5x10^4, O(seed-degree))", 20,
			hubSeeds, scopes, expandCache, graphcache.SourceCache),
	)

	// --- Arms 15-19: Q3 degrees + Q2 induced over ONE response set (the SQL
	// reference run above) ---
	ids := make([]string, len(worst.Nodes))
	for i := range worst.Nodes {
		ids[i] = worst.Nodes[i].ID
	}
	nodeIDs := benchNodeIDs(t, snap, ids)

	{
		var deg int
		p50, p95, mx := benchMeasure(t, 20, 3, func() error {
			nn := make([]GraphNode, len(worst.Nodes))
			copy(nn, worst.Nodes)
			e := fillDegrees(ctx, pool, ids, scopes, nil, benchVisibleTypes, nn)
			deg = 0
			for i := range nn {
				deg += nn[i].Degree
			}
			return e
		})
		res = append(res, benchResult{"15 SQL-Q3 degrees (response set, 4 legs)", p50, p95, mx, 0,
			fmt.Sprintf("nodes=%d degree-sum=%d", len(ids), deg)})
	}
	{
		hints := snap.MakeDegreeHints(scopes, benchVisibleTypes, benchDegreeWalkBudget)
		hints.HitCap = DegreeHitCap
		var sum, capped int
		p50, p95, mx := benchMeasure(t, 20, 3, func() error {
			sum, capped = benchCacheDegrees(snap, nodeIDs, &hints)
			return nil
		})
		res = append(res, benchResult{"16 Cache-Q3 degrees (response set, walk 4000)", p50, p95, mx, 0,
			fmt.Sprintf("nodes=%d degree-sum=%d walk-capped=%d", len(nodeIDs), sum, capped)})
	}

	// The §6.3/§6.4 worst case: 1500 answer nodes that are ALL a 10^4 hub. The
	// corpus has no such response set (nor could a real one exist), so the arm
	// builds it — HUB-LV repeated 1500 times. HUB-LV is the honest choice: its
	// 10k neighbours are 99% invisible, so the DegreeHitCap (201) never fires and
	// the walk runs the FULL adjacency, which is the case the walk budget exists
	// for. An all-visible hub would stop after ~201 examined edges and measure
	// the cheap path.
	worstNode := benchNodeIDs(t, snap, []string{benchHubID(t, pool, "HUB-LV")})[0]
	worstSet := make([]uint32, 1500)
	for i := range worstSet {
		worstSet[i] = worstNode
	}
	for _, budget := range []int{0, benchDegreeWalkBudget} {
		hints := snap.MakeDegreeHints(scopes, benchVisibleTypes, budget)
		hints.HitCap = DegreeHitCap
		var sum, capped int
		p50, p95, mx := benchMeasure(t, 20, 3, func() error {
			sum, capped = benchCacheDegrees(snap, worstSet, &hints)
			return nil
		})
		name := fmt.Sprintf("18 Cache-Q3 worst 1500x10^4 hub (walk budget %d)", budget)
		if budget == 0 {
			name = "17 Cache-Q3 worst 1500x10^4 hub (NO walk budget)"
		}
		res = append(res, benchResult{name, p50, p95, mx, 0,
			fmt.Sprintf("degree/node=%d walk-capped=%d/%d", sum/len(worstSet), capped, len(worstSet))})
		// Non-vacuity, per case — and deliberately NOT "the count must be > 0" for
		// the budgeted run: HUB-LV's 9.9k invisible neighbours carry HIGHER
		// raw_confidence than its 100 visible ones (seed-03), and the CSR adjacency
		// is sorted raw_confidence DESC, so a budget below the invisible run-length
		// legitimately returns 0 with capped=true. That IS the lower-bound contract
		// (§4.1) — asserting a positive count here would assert the contract away.
		switch {
		case budget == 0 && sum == 0:
			t.Errorf("unbudgeted degree walk counted 0 visible neighbours on HUB-LV — hint arrays wired?")
		case budget > 0 && capped != len(worstSet):
			t.Errorf("budgeted degree walk capped only %d of %d nodes — the walk budget did not bite, the arm measures the unbudgeted path",
				capped, len(worstSet))
		}
	}

	{
		var dream, structural int
		p50, p95, mx := benchMeasure(t, 20, 3, func() error {
			ind := snap.InducedEdges(nodeIDs)
			dream, structural = len(ind.Dream), len(ind.Struct)
			return nil
		})
		res = append(res, benchResult{"19 Cache-Q2 induced (response set, map membership)", p50, p95, mx, 0,
			fmt.Sprintf("nodes=%d dream=%d struct=%d", len(nodeIDs), dream, structural)})
		if dream == 0 {
			t.Errorf("Q2 induced returned no dream edge over a %d-node set — membership probe wired?", len(nodeIDs))
		}
	}
	return res
}
