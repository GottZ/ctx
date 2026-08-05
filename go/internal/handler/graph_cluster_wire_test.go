// Wave C2 (Cluster-Topic-Map, design/03 §4.2 + §7 "C2") — the WIRE half of the
// ego cluster annotation. The store half (scope purity, no truncation) is
// store/cluster_annotation_integration_test.go; everything here is pure and
// runs under -short.
//
// Three properties are pinned:
//
//	(ii)  no oracle — the cluster projection carries ORDINALS, never cluster_id
//	      (which is a block UUID, and block ids are uuidv7 ⇒ existence + time
//	      oracle, design/03 §5.1);
//	(iii) pausability — every negative outcome (feature off, ceiling tripped,
//	      probe failed) renders the SAME empty-but-present arrays;
//	(iv)  no dangling ordinal — every value in cluster_of resolves in clusters[].
package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// Fixture ids. The cluster ids are deliberately shaped like real ones (they ARE
// block UUIDs in production) so the oracle probe below has something to find.
const (
	c2NodeA    = "019c9629-0000-7000-9000-00000000a001"
	c2NodeB    = "019c9629-0000-7000-9000-00000000a002"
	c2NodeC    = "019c9629-0000-7000-9000-00000000a003"
	c2NodeD    = "019c9629-0000-7000-9000-00000000a004"
	c2ClusterX = "019c1111-0000-7000-9000-00000000c0a1"
	c2ClusterY = "019c2222-0000-7000-9000-00000000c0b2"
	c2ClusterZ = "019c3333-0000-7000-9000-00000000c0c3"
)

func c2Result() *store.EgoResult {
	mk := func(id string) store.GraphNode {
		return store.GraphNode{
			ID: id, Title: "n", Category: "learnings", Scope: "private",
			Degree: 1, Hop: 1, CreatedAt: time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC),
		}
	}
	return &store.EgoResult{
		Focus: c2NodeA,
		Rels:  store.GraphRels,
		Nodes: []store.GraphNode{mk(c2NodeA), mk(c2NodeB), mk(c2NodeC), mk(c2NodeD)},
	}
}

// c2Annotation: X holds A+B (2 hits), Y holds C (1 hit), D sits in Z — a cluster
// with NO visible aggregate row, the shape that produces -1.
func c2Annotation() *store.ClusterAnnotationResult {
	return &store.ClusterAnnotationResult{
		Clusters: []store.ClusterAnnotationEntry{
			{ClusterID: c2ClusterX, Size: 133, TopCategories: []string{"learnings"}, ScopeMix: []string{"private"}, InResponse: 2},
			{ClusterID: c2ClusterY, Size: 7, TopCategories: []string{"decisions"}, ScopeMix: []string{"private"}, InResponse: 1},
		},
		MemberOf: map[string]string{
			c2NodeA: c2ClusterX,
			c2NodeB: c2ClusterX,
			c2NodeC: c2ClusterY,
			c2NodeD: c2ClusterZ, // no entry in Clusters
		},
	}
}

// Gate (iv): every ordinal in cluster_of resolves in clusters[], and the array
// is positionally parallel to nodes. A block whose cluster has no visible
// aggregate row is -1 — never an index into nothing.
//
// ROT-PROBE: let egoClusterProjection fall through to `o = len(clusters)` (or
// any non-(-1) default) for the unresolved cluster ⇒ the resolve assert fails.
func TestEgoClusterProjection_NoDanglingOrdinal(t *testing.T) {
	res := c2Result()
	p := store.EgoParams{Hops: 1, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}
	env := buildEgoResponse(res, p, nil, 1, c2Annotation())

	if len(env.ClusterOf) != len(env.Nodes) {
		t.Fatalf("cluster_of length %d, nodes %d — must be positionally parallel", len(env.ClusterOf), len(env.Nodes))
	}
	for i, o := range env.ClusterOf {
		if o == -1 {
			continue
		}
		if o < 0 || o >= len(env.Clusters) {
			t.Errorf("cluster_of[%d] = %d does not resolve in clusters[] (len %d)", i, o, len(env.Clusters))
		}
	}
	want := []int{0, 0, 1, -1}
	for i := range want {
		if env.ClusterOf[i] != want[i] {
			t.Errorf("cluster_of = %v, want %v", env.ClusterOf, want)
			break
		}
	}
	if env.Stats.Clusters != 2 {
		t.Errorf("stats.clusters = %d, want 2", env.Stats.Clusters)
	}
	if env.Clusters[0].Cluster != 0 || env.Clusters[1].Cluster != 1 {
		t.Errorf("ordinals must be dense and positional: %+v", env.Clusters)
	}
	if env.Clusters[0].Size != 133 || env.Clusters[0].InResponse != 2 {
		t.Errorf("cluster 0 = %+v, want size 133 / in_response 2", env.Clusters[0])
	}
}

// Gate (ii): NO cluster_id reaches the wire. cluster_id is the smallest member
// UUID of the community and ids are uuidv7 — emitting it tells a caller that an
// invisible block exists and roughly when it was created (§5.1).
//
// ROT-PROBE: set `Cluster: i` → emit the cluster id instead (e.g. add a
// `"cluster_id"` field or key the ordinal by the raw id) ⇒ this test goes red.
func TestEgoResponse_NoClusterIDOnWire(t *testing.T) {
	res := c2Result()
	p := store.EgoParams{Hops: 1, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}
	got, err := json.Marshal(buildEgoResponse(res, p, nil, 1, c2Annotation()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(got)
	for _, cid := range []string{c2ClusterX, c2ClusterY, c2ClusterZ} {
		if strings.Contains(s, cid) {
			t.Errorf("cluster_id %s leaked onto the wire: %s", cid, s)
		}
	}
	// The node ids DO belong on the wire (nodes[].id is the drill-down key) —
	// this test is about the cluster projection, not a blanket UUID ban.
	if !strings.Contains(s, c2NodeA) {
		t.Fatalf("node ids must stay on the wire: %s", s)
	}
}

// Gate (iii): pausability. All three negative outcomes are the SAME bytes, and
// they are the same bytes the golden pins — so a client never has to tell
// "feature off" from "ceiling tripped" from "probe failed", and flipping the
// flag off is a guaranteed return to the Ist envelope.
//
// ROT-PROBE: render the projection unconditionally (drop the `ann == nil` arm in
// egoClusterProjection and build from a zero result) ⇒ the shapes diverge.
func TestEgoResponse_ClusterPausable(t *testing.T) {
	res := c2Result()
	p := store.EgoParams{Hops: 1, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}

	marshal := func(ann *store.ClusterAnnotationResult) string {
		b, err := json.Marshal(buildEgoResponse(res, p, nil, 1, ann))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}

	off := marshal(nil) // feature off / ceiling tripped / probe failed
	// An annotation that resolved to nothing (all nodes unclustered) must render
	// identically too — "no cluster information" has ONE wire shape.
	emptyAnn := marshal(&store.ClusterAnnotationResult{
		Clusters: []store.ClusterAnnotationEntry{},
		MemberOf: map[string]string{},
	})
	if off != emptyAnn {
		t.Errorf("empty annotation must render like the disabled feature:\n off %s\n ann %s", off, emptyAnn)
	}
	if !strings.Contains(off, `"clusters":[],"cluster_of":[]`) {
		t.Errorf("disabled feature must still carry both keys as empty arrays: %s", off)
	}
	if !strings.Contains(off, `"clusters":0`) {
		t.Errorf("stats.clusters must be 0 when the feature is off: %s", off)
	}
	if strings.Contains(marshal(c2Annotation()), `"clusters":[],"cluster_of":[]`) {
		t.Error("enabled annotation rendered as empty — the test fixture no longer proves anything")
	}
}
