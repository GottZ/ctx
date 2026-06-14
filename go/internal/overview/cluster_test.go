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
