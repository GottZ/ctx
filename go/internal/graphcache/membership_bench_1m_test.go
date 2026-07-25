//go:build integration

// W05.8 (design/05 §4.1: "Wahl per Mikro-Bench in W05.6, Bench-Arm W05.8 deckt
// sie mit ab") — the InducedEdges membership probe at the 1M corpus.
//
// W05.6 measured sorted-slice vs. map on a SYNTHETIC 200k universe with a
// uniform degree of 6 (snapshot_membership_bench_test.go) and wired the map arm
// on that evidence. This arm re-runs the same comparison against the real bench
// corpus: 1M nodes, ~3.2M dream edges, hub degrees up to 10^4 — i.e. against the
// skewed degree distribution the synthetic bench does not have. The sorted arm
// stays in the package precisely so this stays re-measurable.
//
// Measurement only: it prints both arms and never asserts a winner. It DOES
// assert that both arms return the same edge counts — a probe that silently
// dropped edges would otherwise look like a fast one.
//
// Run: bash .project/bench-graph/run.sh membership (BENCH_SKIP_SEED=1 to reuse).
package graphcache_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
)

// TestMembershipBench1M compares the two sparse membership probes over response
// sets across the allowed range (egoMaxLimit = 1500).
func TestMembershipBench1M(t *testing.T) {
	dsn := os.Getenv("CTX_BENCH_DSN")
	if dsn == "" {
		t.Skip("CTX_BENCH_DSN unset — run .project/bench-graph/run.sh membership")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bench: connect ctx_bench: %v", err)
	}
	defer pool.Close()

	// Same guard as the store arms: Build reads the FULL universe, so it must
	// never point at the live database that shares this PostgreSQL instance.
	var db string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatalf("bench: current_database(): %v", err)
	}
	if db != "ctx_bench" {
		t.Fatalf("bench: connected to %q — refusing to run outside the scratch DB ctx_bench", db)
	}

	start := time.Now()
	snap, err := graphcache.Build(ctx, pool)
	if err != nil {
		t.Fatalf("bench: build: %v", err)
	}
	t.Logf("=== W05.8 membership probe @1M — build %v, nodes=%d dream=%d struct=%d ===",
		time.Since(start).Round(time.Millisecond), snap.Stats.Nodes,
		snap.Stats.DreamEdges, snap.Stats.StructEdges)
	t.Logf("%-24s %10s %10s %10s   %s", "arm", "p50", "p95", "max", "detail")

	for _, size := range []int{100, 500, 1500} {
		set := benchSpreadSet(snap, size)
		var mapEdges, sortedEdges int
		mp50, mp95, mmax := benchProbe(20, func() {
			r := graphcache.InducedEdgesMapMembershipForTest(snap, set)
			mapEdges = len(r.Dream) + len(r.Struct)
		})
		sp50, sp95, smax := benchProbe(20, func() {
			r := graphcache.InducedEdgesSortedMembershipForTest(snap, set)
			sortedEdges = len(r.Dream) + len(r.Struct)
		})
		t.Logf("%-24s %10s %10s %10s   induced edges=%d",
			fmt.Sprintf("map/set=%d", size), benchDur(mp50), benchDur(mp95), benchDur(mmax), mapEdges)
		t.Logf("%-24s %10s %10s %10s   induced edges=%d",
			fmt.Sprintf("sorted/set=%d", size), benchDur(sp50), benchDur(sp95), benchDur(smax), sortedEdges)
		if mapEdges != sortedEdges {
			t.Errorf("set=%d: map arm returned %d induced edges, sorted arm %d — the probes disagree",
				size, mapEdges, sortedEdges)
		}
	}
}

// benchSpreadSet picks `size` NodeIDs evenly across the universe — deterministic
// and, unlike a contiguous block, not localised into one region of the CSR.
func benchSpreadSet(snap *graphcache.Snapshot, size int) []uint32 {
	step := snap.NumNodes() / size
	set := make([]uint32, 0, size)
	for i := 0; i < size; i++ {
		set = append(set, uint32(i*step))
	}
	return set
}

// benchProbe times n runs of fn and returns p50/p95/max (nearest-rank).
func benchProbe(n int, fn func()) (p50, p95, max time.Duration) {
	fn() // warmup
	ds := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		fn()
		ds[i] = time.Since(start)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[n/2], ds[(n*95-1)/100], ds[n-1]
}

func benchDur(d time.Duration) string { return fmt.Sprintf("%.1fµs", float64(d.Nanoseconds())/1000) }
