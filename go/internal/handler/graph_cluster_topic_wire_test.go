// Wave C5 (Cluster-Topic-Map, design/03 §4.2/§5.1 + §7 "C5", Masterplan K2 /
// A03-2) — the WIRE half of the stable cluster handle. Pure, runs under -short;
// the DB half is store/cluster_topic_annotation_integration_test.go.
//
// Three properties are pinned:
//
//	(i)   no oracle — the handle is the v4 topic id, NEVER cluster_id (a block
//	      UUID, and block ids are uuidv7 ⇒ existence + time oracle, §5.1);
//	(ii)  K2/A03-2 — one entry per cluster, `topic` = the handle of the LARGEST
//	      visible partition, `topics` = all of them (only when there is more than
//	      one, so the live single-partition shape pays no extra bytes);
//	(iii) fail-closed + pausable — a cluster without a resolvable identity keeps
//	      the C2 shape byte for byte, and so does the whole envelope when the
//	      annotation declines.
package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// Handles are v4-shaped on purpose: a caller must be able to tell them apart
// from the uuidv7 block/cluster ids at a glance, and the oracle assert below
// checks the cluster ids specifically.
const (
	c5TopicPrimary   = "aaaaaaaa-0000-4000-8000-000000000001"
	c5TopicSecondary = "bbbbbbbb-0000-4000-8000-000000000002"
	c5Label          = "Retrieval-Architektur"
)

// c5Annotation mirrors c2Annotation (same clusters, same hit counts) and adds
// the C5 identity: X spans two visible partitions, Y has exactly one, and the
// unresolved cluster Z stays absent so the -1 arm keeps its meaning.
func c5Annotation() *store.ClusterAnnotationResult {
	return &store.ClusterAnnotationResult{
		Clusters: []store.ClusterAnnotationEntry{
			{
				ClusterID: c2ClusterX, Size: 133, TopCategories: []string{"learnings"},
				ScopeMix: []string{"private", "work"}, InResponse: 2,
				Topics: []string{c5TopicPrimary, c5TopicSecondary}, Label: c5Label,
			},
			{
				ClusterID: c2ClusterY, Size: 7, TopCategories: []string{"decisions"},
				ScopeMix: []string{"private"}, InResponse: 1,
			},
		},
		MemberOf: map[string]string{
			c2NodeA: c2ClusterX,
			c2NodeB: c2ClusterX,
			c2NodeC: c2ClusterY,
			c2NodeD: c2ClusterZ,
		},
	}
}

func c5Envelope(t *testing.T, ann *store.ClusterAnnotationResult) (egoResponse, string) {
	t.Helper()
	p := store.EgoParams{Hops: 1, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000}
	env := buildEgoResponse(c2Result(), p, nil, 1, ann)
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return env, string(raw)
}

// Gate (ii): primary + full handle list, and the cluster ordinal stays the key
// cluster_of points at — the handle JOINS the ordinal, it does not replace it.
//
// ROT-PROBE: emit `Topic: c.Topics[len(c.Topics)-1]` (or sort the handles by id
// in the store query) ⇒ the primary assert goes red.
func TestEgoClusterWire_TopicPrimaryAndAllPartitions(t *testing.T) {
	env, raw := c5Envelope(t, c5Annotation())

	if len(env.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2", len(env.Clusters))
	}
	x := env.Clusters[0]
	if x.Topic != c5TopicPrimary {
		t.Errorf("topic = %q, want the primary handle %q", x.Topic, c5TopicPrimary)
	}
	if x.Label != c5Label {
		t.Errorf("label = %q, want %q", x.Label, c5Label)
	}
	if len(x.Topics) != 2 || x.Topics[0] != c5TopicPrimary || x.Topics[1] != c5TopicSecondary {
		t.Errorf("topics = %v, want both visible partitions in primary-first order", x.Topics)
	}
	if x.Cluster != 0 || env.Clusters[1].Cluster != 1 {
		t.Errorf("the request ordinal must stay the cluster_of key: %+v", env.Clusters)
	}
	// A single-partition cluster carries the handle but NOT the redundant list —
	// at 1500 nodes an always-present one-element array is dead wire weight, the
	// same argument the edge tuples already carry (§6.5).
	if !strings.Contains(raw, `"topic":"`+c5TopicPrimary+`"`) {
		t.Errorf("primary handle missing from the wire: %s", raw)
	}
}

// Gate (i): NO cluster_id on the wire, and the handle is not derived from one.
//
// ROT-PROBE: set `Topic: c.ClusterID` in egoClusterProjection ⇒ red (that is
// exactly the §5.1 leak: the smallest member UUID of the community, uuidv7, so
// existence AND approximate creation time of an invisible block).
func TestEgoClusterWire_HandleIsNeverAClusterID(t *testing.T) {
	_, raw := c5Envelope(t, c5Annotation())
	for _, cid := range []string{c2ClusterX, c2ClusterY, c2ClusterZ} {
		if strings.Contains(raw, cid) {
			t.Errorf("cluster_id %s leaked onto the wire: %s", cid, raw)
		}
	}
	if !strings.Contains(raw, c5TopicPrimary) {
		t.Fatalf("the fixture no longer carries a handle — the probe proves nothing: %s", raw)
	}
}

// Gate (iii): a cluster the identity layer has not reached keeps the exact C2
// shape — no empty `topic`, no `label`, no `topics` key — and the disabled
// feature keeps the C2 envelope byte for byte.
//
// ROT-PROBE: drop the omitempty on Topic/Label/Topics ⇒ the second entry grows
// three empty keys ⇒ red, and the C2 golden (graph_cluster_wire_test.go) goes
// red with it.
func TestEgoClusterWire_UnidentifiedClusterKeepsC2Shape(t *testing.T) {
	_, raw := c5Envelope(t, c5Annotation())

	// The Y entry (no identity) must be exactly the C2 object.
	const wantY = `{"cluster":1,"size":7,"top_categories":["decisions"],"scope_mix":["private"],"in_response":1}`
	if !strings.Contains(raw, wantY) {
		t.Errorf("unidentified cluster changed shape.\n want substring %s\n got %s", wantY, raw)
	}

	// And the whole envelope with the annotation OFF stays the C2 golden.
	_, off := c5Envelope(t, nil)
	if !strings.Contains(off, `"clusters":[],"cluster_of":[]`) {
		t.Errorf("disabled annotation must keep both keys as empty arrays: %s", off)
	}
	if strings.Contains(off, `"topic"`) || strings.Contains(off, `"topics"`) {
		t.Errorf("disabled annotation must not mention handles at all: %s", off)
	}
}
