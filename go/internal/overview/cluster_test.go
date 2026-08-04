package overview

import (
	"math"
	"reflect"
	"testing"
)

// six nodes: two fully-connected triangles joined by one weak bridge.
var detNodes = []string{
	"01900000-0000-7000-8000-000000000001",
	"01900000-0000-7000-8000-000000000002",
	"01900000-0000-7000-8000-000000000003",
	"01900000-0000-7000-8000-000000000004",
	"01900000-0000-7000-8000-000000000005",
	"01900000-0000-7000-8000-000000000006",
}

var detEdges = []rawEdge{
	{detNodes[0], detNodes[1], 0.9}, {detNodes[1], detNodes[2], 0.9}, {detNodes[0], detNodes[2], 0.9},
	{detNodes[3], detNodes[4], 0.9}, {detNodes[4], detNodes[5], 0.9}, {detNodes[3], detNodes[5], 0.9},
	{detNodes[2], detNodes[3], 0.05}, // weak bridge between the two triangles
}

// TestComputeClustering_Deterministic is the W1 gate: same input + fixed seed ⇒
// byte-identical PARTITION across many runs (determinism axis 1+2, design §3.4).
// The partition is what gets persisted as cluster_id, so it must be stable; the
// Q-score is only a telemetry/smoke value and is reproducible solely within
// float tolerance (gonum's community.Q sums in a map-iteration order, so the
// last ULP drifts — verified, harmless).
func TestComputeClustering_Deterministic(t *testing.T) {
	base := computeClustering(detNodes, detEdges, 1.0)
	if len(base.blockToCluster) != len(detNodes) {
		t.Fatalf("expected %d assignments, got %d", len(detNodes), len(base.blockToCluster))
	}
	// 50 runs — a single repeat would be too small a sample to call it stable.
	for i := 0; i < 50; i++ {
		got := computeClustering(detNodes, detEdges, 1.0)
		if !reflect.DeepEqual(base.blockToCluster, got.blockToCluster) {
			t.Fatalf("partition non-deterministic on run %d:\n  base=%v\n  got =%v", i, base.blockToCluster, got.blockToCluster)
		}
		if math.Abs(base.modularity-got.modularity) > 1e-9 {
			t.Fatalf("modularity diverged beyond float tolerance on run %d: %v vs %v", i, base.modularity, got.modularity)
		}
		// W2: the core is derived from intraDegree, and the core drives both
		// the label and the re-label trigger — a wobbling degree map would
		// re-label a topic that never changed.
		if !reflect.DeepEqual(base.intraDegree, got.intraDegree) {
			t.Fatalf("intraDegree non-deterministic on run %d:\n  base=%v\n  got =%v", i, base.intraDegree, got.intraDegree)
		}
	}
}

// TestIntraDegree_ExcludesCrossCommunityEdges is the W2 gate: the intra-cluster
// weighted degree describes what holds a cluster TOGETHER, not what pulls at
// it. In the two-triangle fixture every node carries exactly two 0.9 edges
// inside its own triangle; the 0.05 bridge belongs to neither community and
// must not reach the two nodes it touches. Without the b2c equality filter
// those two would come out at 1.85 and outrank their peers — the core would
// then be picked by the bridge, i.e. by the very edge the partition rejected.
func TestIntraDegree_ExcludesCrossCommunityEdges(t *testing.T) {
	c := computeClustering(detNodes, detEdges, 1.0)
	if len(c.intraDegree) != len(detNodes) {
		t.Fatalf("expected %d degrees, got %d: %v", len(detNodes), len(c.intraDegree), c.intraDegree)
	}
	for _, u := range detNodes {
		if got := c.intraDegree[u]; math.Abs(got-1.8) > 1e-9 {
			t.Errorf("intraDegree[%s] = %v, want 1.8 (two intra-triangle edges of 0.9; the 0.05 bridge must not count)", u, got)
		}
	}
}

// TestIntraDegree_OrderAndDirectionInvariant: the degree map is built over the
// symmetrized edge map, not over the raw []rawEdge, so neither the load order
// nor the stored direction of a link may move a single weight. Both are real
// axes — loadEdges returns whatever the query yields, and a dream link is
// directed while the graph is not.
func TestIntraDegree_OrderAndDirectionInvariant(t *testing.T) {
	base := computeClustering(detNodes, detEdges, 1.0)

	flipped := make([]rawEdge, len(detEdges))
	for i, e := range detEdges {
		flipped[len(detEdges)-1-i] = rawEdge{src: e.dst, dst: e.src, weight: e.weight}
	}
	got := computeClustering(detNodes, flipped, 1.0)

	if !reflect.DeepEqual(base.intraDegree, got.intraDegree) {
		t.Errorf("intraDegree changed under edge permutation + direction flip:\n  base=%v\n  got =%v", base.intraDegree, got.intraDegree)
	}
}

// TestIntraDegree_SkipsDanglingAndSelfLoops: the degrees inherit the two
// exclusions of the symmetrization pass. A self-loop would inflate its own
// node's centrality out of nothing, and a dangling endpoint (link into an
// archived or invisible block) is not part of this cluster's substance at all.
func TestIntraDegree_SkipsDanglingAndSelfLoops(t *testing.T) {
	nodes := []string{"aaa", "bbb"}
	edges := []rawEdge{
		{"aaa", "bbb", 0.9},
		{"aaa", "ghost", 0.5}, // dangling endpoint
		{"aaa", "aaa", 0.4},   // self-loop
	}
	c := computeClustering(nodes, edges, 1.0)
	for _, u := range nodes {
		if got := c.intraDegree[u]; math.Abs(got-0.9) > 1e-9 {
			t.Errorf("intraDegree[%s] = %v, want 0.9 (dangling + self-loop excluded)", u, got)
		}
	}
	if _, ok := c.intraDegree["ghost"]; ok {
		t.Errorf("dangling endpoint got a degree entry: %v", c.intraDegree)
	}
}

// TestIntraDegree_ZeroEdgeGraph: an edgeless corpus yields singletons with
// degree 0 — an entry per node, never a missing key. The core selection reads
// this map by block id; a nil entry and a 0 entry must not be distinguishable
// at that call site.
func TestIntraDegree_ZeroEdgeGraph(t *testing.T) {
	nodes := []string{"aaa", "bbb", "ccc"}
	c := computeClustering(nodes, nil, 1.0)
	if len(c.intraDegree) != len(nodes) {
		t.Fatalf("expected %d degree entries, got %v", len(nodes), c.intraDegree)
	}
	for _, u := range nodes {
		if c.intraDegree[u] != 0 {
			t.Errorf("intraDegree[%s] = %v, want 0", u, c.intraDegree[u])
		}
	}
}

// TestIntraDegree_EmptyInput: no nodes ⇒ an empty, non-nil map, mirroring the
// blockToCluster early return.
func TestIntraDegree_EmptyInput(t *testing.T) {
	c := computeClustering(nil, nil, 1.0)
	if c.intraDegree == nil {
		t.Errorf("intraDegree is nil on empty input, want an empty map")
	}
	if len(c.intraDegree) != 0 {
		t.Errorf("intraDegree = %v on empty input, want empty", c.intraDegree)
	}
}

// TestComputeClustering_ClusterIDIsMinMember verifies the content-stable
// cluster_id rule: every node's cluster_id is the lexicographically smallest
// UUID among its own community's members (design §3.4 — not the gonum index).
func TestComputeClustering_ClusterIDIsMinMember(t *testing.T) {
	c := computeClustering(detNodes, detEdges, 1.0)
	members := map[string][]string{}
	for b, cid := range c.blockToCluster {
		members[cid] = append(members[cid], b)
	}
	for cid, mem := range members {
		minM := mem[0]
		for _, m := range mem {
			if m < minM {
				minM = m
			}
		}
		if cid != minM {
			t.Errorf("cluster_id %s is not the min member %s of %v", cid, minM, mem)
		}
	}
}

// TestComputeClustering_SeparatesCommunities is a sanity check: the two
// triangles joined by a weak bridge land in different clusters.
func TestComputeClustering_SeparatesCommunities(t *testing.T) {
	c := computeClustering(detNodes, detEdges, 1.0)
	if c.blockToCluster[detNodes[0]] != c.blockToCluster[detNodes[1]] {
		t.Errorf("triangle A split across clusters")
	}
	if c.blockToCluster[detNodes[0]] == c.blockToCluster[detNodes[3]] {
		t.Errorf("triangles A and B merged despite the weak bridge")
	}
}

func TestComputeClustering_Empty(t *testing.T) {
	if c := computeClustering(nil, nil, 1.0); len(c.blockToCluster) != 0 {
		t.Errorf("empty input should produce empty clustering, got %v", c.blockToCluster)
	}
}

// TestComputeClustering_IsolatedNode: a node with no edges still gets a cluster
// (its own singleton) — AddNode covers it, so it appears on the map.
func TestComputeClustering_IsolatedNode(t *testing.T) {
	nodes := []string{"aaa", "bbb", "ccc"}
	edges := []rawEdge{{"aaa", "bbb", 0.9}}
	c := computeClustering(nodes, edges, 1.0)
	if _, ok := c.blockToCluster["ccc"]; !ok {
		t.Errorf("isolated node got no cluster assignment: %v", c.blockToCluster)
	}
}

// TestComputeClustering_SkipsDanglingAndSelfLoops: edges to unknown nodes and
// self-loops are dropped without panicking gonum.
func TestComputeClustering_SkipsDanglingAndSelfLoops(t *testing.T) {
	nodes := []string{"aaa", "bbb"}
	edges := []rawEdge{
		{"aaa", "bbb", 0.9},
		{"aaa", "ghost", 0.9}, // dangling endpoint
		{"aaa", "aaa", 0.9},   // self-loop
	}
	c := computeClustering(nodes, edges, 1.0)
	if len(c.blockToCluster) != 2 {
		t.Errorf("expected 2 assignments, got %d: %v", len(c.blockToCluster), c.blockToCluster)
	}
}
