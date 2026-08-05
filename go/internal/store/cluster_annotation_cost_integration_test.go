//go:build integration

// Wave C2 measurement gate (Cluster-Topic-Map, design/03 §6.4 + §7 "C2" (vi)).
//
// §6.4 asks for a p95 increase of at most 40 ms on each of three paths — ego
// SQL arm, ego cache arm, /api/graph/all at its default (ceiling) limit. On all
// three the code around the annotation is UNCHANGED; the increment is this one
// probe pair. Measuring the increment directly is therefore both what the gate
// is about and the tighter measurement: two end-to-end p95 samples subtracted
// from each other carry the variance of everything they have in common.
//
// It is measured AT THE CEILING (cluster.ego_annotate_max_nodes = 500), which is
// the worst annotated case by construction — above it the annotation declines
// instead of running (TravClusterAnnotateCapped), which is exactly why the route
// ceiling of 1500 never has to be lowered for this feature.
//
// COLD is the first call against a freshly filled database (nothing in shared
// buffers — §6.3's worst case, "directly after a rebuild"); WARM is the p95 over
// the following runs.
//
//	go test -tags=integration ./internal/store/ -run TestClusterAnnotationCost -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// acceptance is the §6.4 absolute bar. Absolute, not a percentage: the relative
// gate of the design's first draft (~15 % of a ~290 ms ego p95 ≈ 43 ms) was
// planned to be broken by the document's own cost estimate — a gate whose most
// likely outcome is failure plans an unplanned exit, not a wave.
const clusterAnnotateAcceptanceMs = 40

func TestClusterAnnotationCostAtCeiling(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// 500 nodes = the annotation ceiling, spread over 25 clusters — the live
	// density (20,2 members/cluster, §3.3). The cluster count is what the second
	// read scales with, the node count what the membership probe scales with.
	const nodes, clusters = 500, 25
	blockIDs := make([]string, 0, nodes)
	for c := 0; c < clusters; c++ {
		cluster := fmt.Sprintf("019d5000-0000-7000-9000-%012x", c)
		c2Node(t, pool, cluster, "private", nodes/clusters, "learnings")
		for i := 0; i < nodes/clusters; i++ {
			block := fmt.Sprintf("019d6000-0000-7000-9000-%06x%06x", c, i)
			c2Member(t, pool, block, "private", cluster)
			blockIDs = append(blockIDs, block)
		}
	}

	run := func() time.Duration {
		start := time.Now()
		if _, err := store.ClusterAnnotation(ctx, pool, blockIDs, []string{"private"}); err != nil {
			t.Fatalf("ClusterAnnotation: %v", err)
		}
		return time.Since(start)
	}

	cold := run()

	const iterations = 40
	samples := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		samples = append(samples, run())
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)/2]
	p95 := samples[(len(samples)*95)/100]

	t.Logf("cluster annotation @ %d nodes / %d clusters: cold %.1f ms | warm p50 %.1f ms | warm p95 %.1f ms (bar %d ms)",
		nodes, clusters, float64(cold.Microseconds())/1000,
		float64(p50.Microseconds())/1000, float64(p95.Microseconds())/1000,
		clusterAnnotateAcceptanceMs)

	for name, d := range map[string]time.Duration{"cold": cold, "warm p95": p95} {
		if d > clusterAnnotateAcceptanceMs*time.Millisecond {
			t.Errorf("%s = %.1f ms exceeds the §6.4 acceptance of %d ms",
				name, float64(d.Microseconds())/1000, clusterAnnotateAcceptanceMs)
		}
	}
}
