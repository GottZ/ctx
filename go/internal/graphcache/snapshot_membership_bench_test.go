package graphcache_test

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/GottZ/ctx/internal/graphcache"
)

// W05.6 membership micro-bench (handover from W05.1, design/05 §4.1): the
// InducedEdges set-membership probe — sorted-slice binary search vs.
// map[uint32]struct{}. The dense-bitset arm is NOT benchmarked: at the 10M
// target it allocates + zeroes ~1.25 MB per request for ≤1500 members, which the
// design already rules out on allocation grounds, not on probe cost.
//
// The shape is the real one: a node universe far larger than the response set
// (the probe's cache behaviour depends on that ratio), an average degree of ~6
// (§6.2), and response sets across the whole allowed range (egoMaxLimit = 1500).
//
// Run:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test ./internal/graphcache/ -run '^$' -bench 'InducedEdgesMembership' -benchmem

func benchSnapshot(tb testing.TB, nodes, degree int) (*graphcache.Snapshot, []uint32) {
	tb.Helper()
	rng := rand.New(rand.NewSource(1))

	u := make([]graphcache.BlockRowT, nodes)
	ids := make([][16]byte, nodes)
	for i := 0; i < nodes; i++ {
		var id [16]byte
		id[0] = byte(i >> 24)
		id[1] = byte(i >> 16)
		id[2] = byte(i >> 8)
		id[3] = byte(i)
		ids[i] = id
		u[i] = graphcache.BlockRowT{ID: id, Scope: "shared", TypeName: "knowledge"}
	}
	d := make([]graphcache.DreamRowT, 0, nodes*degree)
	for i := 0; i < nodes; i++ {
		for k := 0; k < degree; k++ {
			j := rng.Intn(nodes)
			if j == i {
				continue
			}
			c := rng.Float64()
			d = append(d, graphcache.DreamRowT{Src: ids[i], Dst: ids[j], Rel: "topical", Conf: c, RawConf: c})
		}
	}
	snap, err := graphcache.AssembleForTest(u, d, nil)
	if err != nil {
		tb.Fatalf("assemble: %v", err)
	}
	return snap, nil
}

func benchSet(tb testing.TB, snap *graphcache.Snapshot, size int) []uint32 {
	tb.Helper()
	rng := rand.New(rand.NewSource(7))
	seen := map[uint32]bool{}
	set := make([]uint32, 0, size)
	for len(set) < size {
		n := uint32(rng.Intn(snap.NumNodes()))
		if seen[n] {
			continue
		}
		seen[n] = true
		set = append(set, n)
	}
	return set
}

func BenchmarkInducedEdgesMembership(b *testing.B) {
	const nodes, degree = 200000, 6
	snap, _ := benchSnapshot(b, nodes, degree)

	for _, size := range []int{100, 500, 1500} {
		set := benchSet(b, snap, size)
		b.Run(fmt.Sprintf("sorted/set=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = graphcache.InducedEdgesSortedMembershipForTest(snap, set)
			}
		})
		b.Run(fmt.Sprintf("map/set=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = graphcache.InducedEdgesMapMembershipForTest(snap, set)
			}
		})
	}
}
