package overview

import (
	"context"
	"fmt"
	"testing"
)

// bigGraph builds a synthetic node/edge set large enough that computeClustering
// cannot finish before an already-cancelled ctx is observed (B-W1 liveness).
func bigGraph(n int) ([]string, []rawEdge) {
	nodes := make([]string, n)
	for i := range nodes {
		nodes[i] = fmt.Sprintf("01900000-0000-7000-8000-%012d", i)
	}
	edges := make([]rawEdge, 0, n*4)
	for i := 0; i < n; i++ {
		for _, off := range []int{1, 7, 42, 199} {
			j := (i + off) % n
			edges = append(edges, rawEdge{nodes[i], nodes[j], 0.5})
		}
	}
	return nodes, edges
}

// TestClusterWithCtx_AbortsOnCancelledCtx is the B-W1 red probe for the
// secondary liveness guard: a cancelled ctx aborts the compute phase instead
// of waiting for Louvain to converge. The graph is big enough (20k nodes/80k
// edges, seconds of Louvain) that the done channel CANNOT be ready when the
// select runs — the ctx arm deterministically wins; the orphaned goroutine
// drains into the buffered channel (documented leak).
func TestClusterWithCtx_AbortsOnCancelledCtx(t *testing.T) {
	nodes, edges := bigGraph(20000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := clusterWithCtx(ctx, nodes, edges, 1.0)
	if err == nil {
		t.Fatal("expected abort error from cancelled ctx, got nil")
	}
	if ctx.Err() == nil {
		t.Fatal("test invariant broken: ctx not cancelled")
	}
}

// TestClusterWithCtx_CompletesUncancelled is the green counterpart: an active
// ctx yields the same result the direct call produces.
func TestClusterWithCtx_CompletesUncancelled(t *testing.T) {
	cl, err := clusterWithCtx(context.Background(), detNodes, detEdges, 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cl.blockToCluster) != len(detNodes) {
		t.Fatalf("expected %d assignments, got %d", len(detNodes), len(cl.blockToCluster))
	}
}
