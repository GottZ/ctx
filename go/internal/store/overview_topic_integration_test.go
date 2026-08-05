//go:build integration

// W7 DB gates — the read path on topic identity (design/01 §4.7 / §7 W7).
//
// Three properties, each with its own failure mode:
//   - a topic never reaches a caller who cannot read its scope (G2);
//   - a scope-crossing cluster is TWO nodes with disjoint categories, not one
//     merged node (G3);
//   - the legacy switch decides per REQUEST and errs towards a COMPLETE map,
//     never towards a complete identity (G4).
package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func w7ID(n int) string { return fmt.Sprintf("019e2222-0000-7000-9000-%012d", n) }

// w7Node writes one (cluster, scope) partition. topic may be "" (legacy row).
func w7Node(t *testing.T, pool *pgxpool.Pool, cluster, scope, topic, category string, size int) {
	t.Helper()
	var tid any
	if topic != "" {
		tid = topic
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO graph_cluster_node (cluster_id, scope, size, category_counts, repr_block_id,
		                                repr_title, repr_quality, topic_id)
		VALUES ($1::uuid, $2, $3, jsonb_build_object($4::text, $3::int), $1::uuid, $5, 1.0, $6::uuid)`,
		cluster, scope, size, category, "repr "+scope, tid); err != nil {
		t.Fatalf("w7Node(%s/%s): %v", cluster, scope, err)
	}
}

// w7Topic creates a labelled living topic in one scope.
func w7Topic(t *testing.T, pool *pgxpool.Pool, scope, label string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO graph_cluster_topic (scope, label, label_source, label_built_at, label_stale)
		VALUES ($1, $2, 'fallback', now(), false) RETURNING topic_id::text`, scope, label).Scan(&id); err != nil {
		t.Fatalf("w7Topic(%s): %v", scope, err)
	}
	return id
}

func w7Params() store.OverviewParams {
	return store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}
}

// ─────────────────────────────────────────────────────────────────────────────

// G2 — a topic whose SCOPE does not match its node row's scope must not serve
// its label. The fixture forces the state a correct assignment can never
// produce, so the read-path predicate is what has to hold.
func TestW7ScopePurityOfTheTopicJoin(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	const scopeA, scopeB = "w7a", "w7b"

	foreign := w7Topic(t, pool, scopeB, "SECRET-B LABEL")
	own := w7Topic(t, pool, scopeA, "own label")
	w7Node(t, pool, w7ID(1), scopeA, own, "learnings", 5)
	w7Node(t, pool, w7ID(2), scopeA, foreign, "learnings", 4) // the forced defect

	res, err := store.GraphOverview(context.Background(), pool, w7Params(), []string{scopeA})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	for _, n := range res.Nodes {
		if n.Label == "SECRET-B LABEL" || n.TopicID == foreign {
			t.Fatalf("a foreign-scope topic reached a scope-A reader: %+v", n)
		}
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("got %d nodes, want 1 — the mismatched row must fall out fail-loud", len(res.Nodes))
	}

	// RED PROBE: drop the t.scope = n.scope backstop.
	prev, restore := store.PatchOverviewNodesTopicSQL(strings.Replace(store.OverviewNodesTopicSQL(),
		"JOIN graph_cluster_topic t ON t.topic_id = n.topic_id AND t.scope = n.scope",
		"JOIN graph_cluster_topic t ON t.topic_id = n.topic_id", 1))
	defer restore()
	if store.OverviewNodesTopicSQL() == prev {
		t.Fatal("red probe did not patch the join")
	}
	red, err := store.GraphOverview(context.Background(), pool, w7Params(), []string{scopeA})
	if err != nil {
		t.Fatalf("overview (red): %v", err)
	}
	leaked := false
	for _, n := range red.Nodes {
		if n.Label == "SECRET-B LABEL" {
			leaked = true
		}
	}
	if !leaked {
		t.Fatal("red probe stayed green — the scope backstop is not what stops the leak")
	}
}

// G3 — a scope-crossing cluster is two topics, and their categories must stay
// disjoint. Keying the category fill by cluster_id alone would give one of the
// two nothing and the other the merged counts of both halves.
func TestW7CrossScopeClusterIsTwoNodes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	const scopeA, scopeB = "w7xa", "w7xb"
	cluster := w7ID(10)

	ta := w7Topic(t, pool, scopeA, "half A")
	tb := w7Topic(t, pool, scopeB, "half B")
	w7Node(t, pool, cluster, scopeA, ta, "learnings", 6)
	w7Node(t, pool, cluster, scopeB, tb, "infrastructure", 4)

	res, err := store.GraphOverview(context.Background(), pool, w7Params(), []string{scopeA, scopeB})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 — one scope-pure topic per half", len(res.Nodes))
	}
	byLabel := map[string]store.OverviewNode{}
	for _, n := range res.Nodes {
		byLabel[n.Label] = n
	}
	a, b := byLabel["half A"], byLabel["half B"]
	if len(a.ScopeMix) != 1 || a.ScopeMix[0] != scopeA || len(b.ScopeMix) != 1 || b.ScopeMix[0] != scopeB {
		t.Fatalf("scope_mix not disjoint: %v / %v", a.ScopeMix, b.ScopeMix)
	}
	if strings.Join(a.TopCategories, ",") != "learnings" {
		t.Fatalf("half A categories = %v — the fill must not merge the scope halves", a.TopCategories)
	}
	if strings.Join(b.TopCategories, ",") != "infrastructure" {
		t.Fatalf("half B categories = %v", b.TopCategories)
	}
	if a.Size != 6 || b.Size != 4 {
		t.Fatalf("sizes = %d/%d, want 6/4 (per partition, not summed)", a.Size, b.Size)
	}

	// RED PROBE: the legacy shape merges the two halves into one node whose
	// categories carry both — exactly the state the topic identity ends.
	legacyNodes, _, err := store.OverviewNodesForTest(context.Background(), pool, w7Params(), []string{scopeA, scopeB}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyNodes) != 1 {
		t.Fatalf("red probe: legacy GROUP BY produced %d nodes, want 1", len(legacyNodes))
	}
	if err := store.FillTopCategoriesForTest(context.Background(), pool, legacyNodes, []string{scopeA, scopeB}, true); err != nil {
		t.Fatal(err)
	}
	if len(legacyNodes[0].TopCategories) != 2 {
		t.Fatalf("red probe: merged node has %v — it should carry BOTH halves' categories",
			legacyNodes[0].TopCategories)
	}
}

// G4 — the legacy switch. The mixed state is real and lasts N × 6 h; a caller
// spanning both partitions must not get a silently halved map.
func TestW7LegacySwitch(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("no identities at all ⇒ the pre-W7 answer", func(t *testing.T) {
		const scope = "w7legacy"
		w7Node(t, pool, w7ID(20), scope, "", "learnings", 3)
		w7Node(t, pool, w7ID(21), scope, "", "decisions", 2)

		res, err := store.GraphOverview(ctx, pool, w7Params(), []string{scope})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Nodes) != 2 {
			t.Fatalf("got %d nodes, want 2", len(res.Nodes))
		}
		for _, n := range res.Nodes {
			if n.TopicID != "" || n.Label != "" {
				t.Fatalf("legacy node carries identity fields: %+v", n)
			}
			if n.Key() != n.ClusterID {
				t.Fatal("a legacy node must key on its cluster")
			}
			if len(n.TopCategories) == 0 {
				t.Fatalf("legacy categories missing: %+v", n)
			}
		}
	})

	t.Run("mixed state ⇒ the map stays COMPLETE", func(t *testing.T) {
		const done, pending = "w7mixdone", "w7mixpending"
		tid := w7Topic(t, pool, done, "rebuilt")
		w7Node(t, pool, w7ID(30), done, tid, "learnings", 5)
		w7Node(t, pool, w7ID(31), pending, "", "decisions", 4) // not rebuilt yet

		res, err := store.GraphOverview(ctx, pool, w7Params(), []string{done, pending})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Nodes) != 2 {
			t.Fatalf("got %d nodes, want 2 — a mixed read must not lose the un-rebuilt partition", len(res.Nodes))
		}
		for _, n := range res.Nodes {
			if n.TopicID != "" {
				t.Fatalf("the request fell back to legacy, so NO node may carry a topic: %+v", n)
			}
		}

		// RED PROBE: the Revision-1 criterion — "legacy only if NO row has a
		// topic_id" — takes the identity path here, and the fail-closed JOIN
		// silently halves the map.
		red, _, err := store.OverviewNodesForTest(ctx, pool, w7Params(), []string{done, pending}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(red) != 1 {
			t.Fatalf("red probe: identity path returned %d nodes, want 1 (the halved map)", len(red))
		}
	})
}

// The rollout's own transition: migration applied, no rebuild yet. Every node
// row has topic_id NULL and the map must be readable and complete.
func TestW7NullTopicTransition(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	const scope = "w7null"
	for i := 40; i < 43; i++ {
		w7Node(t, pool, w7ID(i), scope, "", "learnings", i-39)
	}
	res, err := store.GraphOverview(context.Background(), pool, w7Params(), []string{scope})
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if len(res.Nodes) != 3 {
		t.Fatalf("got %d nodes, want 3 — the map before the first identity run", len(res.Nodes))
	}
	if res.Nodes[0].Size < res.Nodes[2].Size {
		t.Fatal("ordering lost")
	}
}
