// W7 unit gate — the topic projection of the meta-edges (design/01 §4.7, G5).
//
// The mapping lives in Go and not in SQL, so this is a pure unit test: the SQL
// variant would have had to join graph_cluster_node twice and could only apply
// its filter post-join, which removes the prefilter on graph_cluster_edge — the
// one table of the family that grows with the dream-link count instead of the
// cluster count.
package store

import "testing"

func TestProjectEdgesOntoTopics(t *testing.T) {
	// The cluster order deliberately CONTRADICTS the topic order: the 057 CHECK
	// guarantees cluster_a < cluster_b, which says nothing about the topics they
	// map to.
	nodes := []OverviewNode{
		{ClusterID: "c-aaa", TopicID: "t-zzz", ScopeMix: []string{"private"}},
		{ClusterID: "c-bbb", TopicID: "t-mmm", ScopeMix: []string{"private"}},
		{ClusterID: "c-ccc", TopicID: "t-mmm", ScopeMix: []string{"private"}},
	}

	t.Run("endpoints are re-normalised", func(t *testing.T) {
		got := projectEdgesOntoTopics([]OverviewEdge{
			{A: "c-aaa", B: "c-bbb", ScopeA: "private", ScopeB: "private", LinkCount: 3, Weight: 1.5}}, nodes[:2])
		if len(got) != 1 {
			t.Fatalf("got %d edges, want 1", len(got))
		}
		if got[0].A != "t-mmm" || got[0].B != "t-zzz" {
			t.Fatalf("edge = %s→%s, want t-mmm→t-zzz (LEAST/GREATEST on the TOPICS)", got[0].A, got[0].B)
		}
	})

	t.Run("pairs collapsing onto one topic pair are summed", func(t *testing.T) {
		// c-bbb and c-ccc both carry t-mmm: two cluster edges, one topic edge.
		got := projectEdgesOntoTopics([]OverviewEdge{
			{A: "c-aaa", B: "c-bbb", ScopeA: "private", ScopeB: "private", LinkCount: 3, Weight: 1.5},
			{A: "c-aaa", B: "c-ccc", ScopeA: "private", ScopeB: "private", LinkCount: 4, Weight: 2.5},
		}, nodes)
		if len(got) != 1 {
			t.Fatalf("got %d edges, want 1 merged", len(got))
		}
		if got[0].LinkCount != 7 || got[0].Weight != 4.0 {
			t.Fatalf("merged edge = %+v, want link_count 7 / weight 4", got[0])
		}
	})

	t.Run("a self-loop after projection is dropped", func(t *testing.T) {
		got := projectEdgesOntoTopics([]OverviewEdge{
			{A: "c-bbb", B: "c-ccc", ScopeA: "private", ScopeB: "private", LinkCount: 9, Weight: 1}}, nodes)
		if len(got) != 0 {
			t.Fatalf("two clusters of ONE topic produced %d edges, want 0", len(got))
		}
	})

	t.Run("an edge into an unknown cluster is dropped", func(t *testing.T) {
		got := projectEdgesOntoTopics([]OverviewEdge{
			{A: "c-aaa", B: "c-unknown", ScopeA: "private", ScopeB: "private", LinkCount: 1, Weight: 1}}, nodes)
		if len(got) != 0 {
			t.Fatalf("got %d edges, want 0 — an unmappable endpoint has no ordinal", len(got))
		}
	})

	// A scope-crossing cluster maps to TWO topics, and the SCOPE PAIR of the edge
	// row is what says which half an edge touches. That is the reason the
	// identity path keeps scope_s/scope_t in the aggregation: without them the
	// endpoint would be unresolvable and the edge would have to be dropped, i.e.
	// a real connection would disappear from the map.
	t.Run("a scope-crossing cluster resolves per half", func(t *testing.T) {
		amb := []OverviewNode{
			{ClusterID: "c-split", TopicID: "t-priv", ScopeMix: []string{"private"}},
			{ClusterID: "c-split", TopicID: "t-shar", ScopeMix: []string{"shared"}},
			{ClusterID: "c-aaa", TopicID: "t-zzz", ScopeMix: []string{"private"}},
		}
		got := projectEdgesOntoTopics([]OverviewEdge{
			{A: "c-aaa", B: "c-split", ScopeA: "private", ScopeB: "shared", LinkCount: 1, Weight: 1},
			{A: "c-aaa", B: "c-split", ScopeA: "private", ScopeB: "private", LinkCount: 2, Weight: 2},
		}, amb)
		if len(got) != 2 {
			t.Fatalf("got %+v, want one edge per resolved half", got)
		}
		seen := map[string]int{}
		for _, e := range got {
			seen[e.A+"|"+e.B] = e.LinkCount
		}
		if seen["t-shar|t-zzz"] != 1 || seen["t-priv|t-zzz"] != 2 {
			t.Fatalf("halves not resolved separately: %+v", seen)
		}
	})

	// An endpoint whose partition is not in the result — below min_cluster_size,
	// past the node limit — has no ordinal, so its edge is dropped.
	t.Run("an endpoint scope that is not in the result is dropped", func(t *testing.T) {
		got := projectEdgesOntoTopics([]OverviewEdge{
			{A: "c-aaa", B: "c-bbb", ScopeA: "private", ScopeB: "work", LinkCount: 1, Weight: 1}}, nodes)
		if len(got) != 0 {
			t.Fatalf("got %+v, want 0 — c-bbb has no work partition in the result", got)
		}
	})

	// RED DIRECTION for the normalisation: without LEAST/GREATEST the two
	// cluster edges of the merge case would stay two edges with swapped
	// endpoints, and the handler would emit a duplicate pair.
	t.Run("output order is deterministic", func(t *testing.T) {
		in := []OverviewEdge{
			{A: "c-aaa", B: "c-bbb", ScopeA: "private", ScopeB: "private", LinkCount: 1, Weight: 1},
			{A: "c-aaa", B: "c-ccc", ScopeA: "private", ScopeB: "private", LinkCount: 1, Weight: 1},
		}
		first := projectEdgesOntoTopics(in, nodes)
		for i := 0; i < 20; i++ {
			again := projectEdgesOntoTopics(in, nodes)
			if len(again) != len(first) || again[0].A != first[0].A || again[0].B != first[0].B {
				t.Fatal("projection output order is not stable across runs")
			}
		}
	})
}
