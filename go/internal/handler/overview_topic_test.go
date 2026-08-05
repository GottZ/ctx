// W7 wire gates — topic + label on GET /api/graph/overview
// (Cluster-Topic-Map design/01 §4.7, decisions E6-01 / E8-01, risk R2).
package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

const (
	ovTopicA = "7f3a1c22-0000-4000-8000-00000000aa01" // v4 — no block reference, no timestamp
	ovTopicB = "7f3a1c22-0000-4000-8000-00000000bb02"
)

// sampleTopicResult is the same map as sampleOverviewResult, with identities.
func sampleTopicResult() *store.OverviewResult {
	res := sampleOverviewResult()
	res.Nodes[0].TopicID, res.Nodes[0].Label = ovTopicA, "Retrieval-Pipeline & RRF-Tuning"
	res.Nodes[1].TopicID, res.Nodes[1].Label = ovTopicB, "Betrieb & Deployments"
	res.Edges = []store.OverviewEdge{{A: ovTopicA, B: ovTopicB, LinkCount: 318, Weight: 0.7399}}
	return res
}

// G1 — a map without identities answers EXACTLY as it did before W7. That is
// what makes the wave deployable ahead of the first rebuild.
func TestW7WireNeutralWithoutTopics(t *testing.T) {
	p := store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}
	raw, err := json.Marshal(buildOverviewResponse(sampleOverviewResult(), p, 7))
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	if strings.Contains(js, `"topic"`) || strings.Contains(js, `"label"`) {
		t.Fatalf("omitempty did not hold — a topic-less map grew fields:\n%s", js)
	}
	// The pre-W7 node shape, field for field and in order.
	const want = `{"cluster":0,"size":142,"top_categories":["learnings","decisions"],` +
		`"repr_id":"019d0000-0000-7000-9000-0000000000c1","repr_title":"Alpha",` +
		`"scope_mix":["private","shared"]}`
	if !strings.Contains(js, want) {
		t.Fatalf("node shape drifted:\n%s", js)
	}
}

// The additive half of G1: with identities the two fields appear, and
// repr_title/repr_id stay (decision E6-01 — label ACCOMPANIES, it does not
// replace; the drill-down hangs off repr_id).
func TestW7WireCarriesTopicAndLabel(t *testing.T) {
	p := store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}
	raw, _ := json.Marshal(buildOverviewResponse(sampleTopicResult(), p, 7))
	js := string(raw)
	for _, want := range []string{
		`"topic":"` + ovTopicA + `"`,
		`"label":"Retrieval-Pipeline \u0026 RRF-Tuning"`,
		`"repr_id":"` + ovReprA + `"`,
		`"repr_title":"Alpha"`,
		`"cluster":0`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("wire is missing %s:\n%s", want, js)
		}
	}
	// The ordinal is still what the edge tuples point at.
	resp := buildOverviewResponse(sampleTopicResult(), p, 7)
	if len(resp.Edges) != 1 || resp.Edges[0].Src != 0 || resp.Edges[0].Dst != 1 {
		t.Fatalf("edges lost their ordinal mapping: %+v", resp.Edges)
	}
}

// R2 (masterplan §6) — the property, not an example: NO wire field carries a
// context_blocks uuid except repr_id.
//
// The handle is the thing this could go wrong on, because the obvious
// implementation — "just emit cluster_id, it is already unique" — is exactly
// the existence oracle the endpoint was built to avoid: cluster_id IS a block
// uuid (the smallest member id), so emitting it discloses the existence and
// identity of a possibly invisible block.
func TestW7NoBlockUUIDOnTheWireExceptReprID(t *testing.T) {
	p := store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}
	blockUUIDs := []string{ovClusterA, ovClusterB}
	allowed := map[string]bool{ovReprA: true, ovReprB: true}

	assertNoLeak := func(t *testing.T, res *store.OverviewResult) {
		t.Helper()
		raw, _ := json.Marshal(buildOverviewResponse(res, p, 7))
		for _, u := range blockUUIDs {
			if allowed[u] {
				continue
			}
			if strings.Contains(string(raw), u) {
				t.Fatalf("block uuid %s reached the wire:\n%s", u, raw)
			}
		}
		if !strings.Contains(string(raw), ovReprA) {
			t.Fatal("repr_id vanished — the drill-down is the one legitimate block reference")
		}
	}

	t.Run("identity path", func(t *testing.T) { assertNoLeak(t, sampleTopicResult()) })
	t.Run("legacy path", func(t *testing.T) { assertNoLeak(t, sampleOverviewResult()) })

	// RED PROBE: the handle IS the cluster id. The property test has to fail.
	t.Run("red probe — handle := clusterID", func(t *testing.T) {
		res := sampleTopicResult()
		for i := range res.Nodes {
			res.Nodes[i].TopicID = res.Nodes[i].ClusterID
		}
		raw, _ := json.Marshal(buildOverviewResponse(res, p, 7))
		if !strings.Contains(string(raw), ovClusterA) {
			t.Fatal("red probe stayed green — the assertion does not actually look at the handle")
		}
	})
}

// The ordinal map has to be built on the SAME identifier space the edges use.
// Getting that wrong drops every edge silently instead of failing.
func TestW7EdgeKeySpaceMatchesNodeKeySpace(t *testing.T) {
	p := store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}
	res := sampleTopicResult()
	// Edges keyed by CLUSTER while the nodes carry topics: nothing may map.
	res.Edges = []store.OverviewEdge{{A: ovClusterA, B: ovClusterB, LinkCount: 5, Weight: 1}}
	if resp := buildOverviewResponse(res, p, 7); len(resp.Edges) != 0 {
		t.Fatalf("a cluster-keyed edge resolved against topic-keyed nodes: %+v", resp.Edges)
	}
	if resp := buildOverviewResponse(sampleTopicResult(), p, 7); len(resp.Edges) != 1 {
		t.Fatal("the matching key space must resolve")
	}
}

// E8-01 — the read cap stays 2000. A reduction would be a behaviour break for
// an existing, explicitly requested parameter; the wire growth of ≈82–102 B per
// node is the priced-in cost of the decision, not a reason to move the cap.
func TestW7ReadCapUnchanged(t *testing.T) {
	if overviewMaxNodeLimit != 2000 {
		t.Fatalf("overviewMaxNodeLimit = %d, want 2000 (decision E8-01)", overviewMaxNodeLimit)
	}
	if overviewDefaultNodeLimit != 500 {
		t.Fatalf("overviewDefaultNodeLimit = %d, want 500", overviewDefaultNodeLimit)
	}
}
