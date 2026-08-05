//go:build integration

// Wave C5 (Cluster-Topic-Map, design/03 §4.2/§5.1 + §7 "C5", Masterplan K2 /
// A03-2) — the STORE half of "the ego annotation carries the stable handle".
//
// Four properties, each with its own failure mode:
//
//	(i)   fail-closed identity: a partition WITHOUT a topic row, and a partition
//	      whose topic belongs to another scope, both yield an entry with NO
//	      handle and NO label — never an error, never a foreign label;
//	(ii)  stability: the handle survives a rebuild that renames the cluster.
//	      cluster_id is the smallest member UUID and changes the moment that one
//	      block leaves the community (overview/cluster.go); the topic row does
//	      not, because the 057 teardown never touches it;
//	(iii) K2/A03-2: a cluster spanning two VISIBLE scopes is ONE ego entry (the
//	      wire keys blocks by cluster ordinal) carrying the handles of ALL its
//	      visible partitions, primary = the largest visible size — because a
//	      handle is scope-bound ("ein Handle = ein scope-reines Thema") and the
//	      entry aggregates over the partitions the caller may see;
//	(iv)  scope purity of the size is unchanged (pinned in
//	      cluster_annotation_integration_test.go) — the topic join must not widen
//	      it, which is why it is a LEFT JOIN carrying t.scope = n.scope.
//
//	go test -tags=integration ./internal/store/ -run TestClusterAnnotationTopic -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// c5Topic creates a living topic in one scope with an explicit id, so the
// fixtures can order ids AGAINST size and prove which one the primary rule
// follows.
func c5Topic(t *testing.T, pool *pgxpool.Pool, topicID, scope, label string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO graph_cluster_topic (topic_id, scope, label, label_source, label_built_at, label_stale)
		 VALUES ($1::uuid, $2, $3, 'fallback', now(), false)`,
		topicID, scope, label); err != nil {
		t.Fatalf("c5Topic(%s/%s): %v", topicID, scope, err)
	}
}

// c5Node writes one (cluster, scope) partition; topic "" = no identity yet.
func c5Node(t *testing.T, pool *pgxpool.Pool, clusterID, scope string, size int, cat, topicID string) {
	t.Helper()
	var tid any
	if topicID != "" {
		tid = topicID
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts, topic_id)
		 VALUES ($1::uuid, $2::text, $3::int, $1::uuid, 'repr', 1, jsonb_build_object($4::text, $3::int), $5::uuid)`,
		clusterID, scope, size, cat, tid); err != nil {
		t.Fatalf("c5Node(%s/%s): %v", clusterID, scope, err)
	}
}

// c5Member writes a block plus its membership row.
func c5Member(t *testing.T, pool *pgxpool.Pool, id, scope, clusterID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid, 'learnings', $2, 'c5 fixture', $3)`,
		id, "blk-"+id, scope); err != nil {
		t.Fatalf("c5Member block %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
		 VALUES ($1::uuid, $2::uuid, $3)`,
		id, clusterID, scope); err != nil {
		t.Fatalf("c5Member membership %s: %v", id, err)
	}
}

// Gate (iii) — K2/A03-2. The cluster spans `private` (size 3) and `work`
// (size 7); the caller sees BOTH. The entry carries both handles, and the
// PRIMARY is the one with the larger visible size — deliberately NOT the
// smaller topic id, which the fixture makes the opposite choice.
//
// ROT-PROBE: order the handle aggregation in clustersql.VisibleSizeQuery by
// `t.topic_id` instead of `n.size DESC` ⇒ the private handle becomes primary
// ⇒ red. Second probe: aggregate only one partition (drop the array) ⇒ the
// "both handles" assert goes red.
func TestClusterAnnotationTopicPrimaryIsLargestVisiblePartition(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const cluster = "019e5000-0000-7000-9000-00000000c001"
	// aaa… < bbb…: the SMALL partition owns the SMALLER handle, so an id-ordered
	// primary rule and a size-ordered one disagree.
	const topicPrivate = "aaaaaaaa-0000-4000-8000-000000000001"
	const topicWork = "bbbbbbbb-0000-4000-8000-000000000002"
	const blockPrivate = "019e5000-0000-7000-9000-0000000000a1"
	const blockWork = "019e5000-0000-7000-9000-0000000000a2"

	c5Topic(t, pool, topicPrivate, "private", "kleine Hälfte")
	c5Topic(t, pool, topicWork, "work", "grosse Hälfte")
	c5Node(t, pool, cluster, "private", 3, "learnings", topicPrivate)
	c5Node(t, pool, cluster, "work", 7, "decisions", topicWork)
	c5Member(t, pool, blockPrivate, "private", cluster)
	c5Member(t, pool, blockWork, "work", cluster)

	ann, err := store.ClusterAnnotation(ctx, pool, []string{blockPrivate, blockWork}, []string{"private", "work"})
	if err != nil {
		t.Fatalf("ClusterAnnotation: %v", err)
	}
	if len(ann.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1 (one entry per CLUSTER — cluster_of indexes it): %+v", len(ann.Clusters), ann.Clusters)
	}
	got := ann.Clusters[0]
	if len(got.Topics) != 2 {
		t.Fatalf("topics = %v, want both visible partitions (K2: the entry carries the handles of ALL visible partitions)", got.Topics)
	}
	if got.Topics[0] != topicWork {
		t.Errorf("primary handle = %s, want %s (largest visible size, NOT smallest id)", got.Topics[0], topicWork)
	}
	if got.Topics[1] != topicPrivate {
		t.Errorf("secondary handle = %s, want %s", got.Topics[1], topicPrivate)
	}
	if got.Label != "grosse Hälfte" {
		t.Errorf("label = %q, want the PRIMARY partition's label", got.Label)
	}
	if got.Size != 10 {
		t.Errorf("size = %d, want 10 (both partitions visible)", got.Size)
	}
}

// Gate (i) — fail-closed identity, two shapes in one fixture:
//
//   - cluster L has a node row WITHOUT topic_id (the pre-W3 / mid-rollout state):
//     the entry exists, carries no handle and no label, and no error is raised;
//   - cluster M's node row points at a topic of ANOTHER scope (the state a
//     correct assignment can never produce — forced here): the label must not
//     travel. The read is a LEFT JOIN on (topic_id, scope), so the mismatch
//     degrades to "no handle" instead of serving scope B's label to a scope A
//     reader.
//
// ROT-PROBE: drop `AND t.scope = n.scope` from the topic join in
// clustersql.VisibleSizeQuery ⇒ "SECRET-B LABEL" reaches the scope-A caller
// ⇒ red.
func TestClusterAnnotationTopicFailClosed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const clusterL = "019e5100-0000-7000-9000-00000000c001"
	const clusterM = "019e5100-0000-7000-9000-00000000c002"
	const foreignTopic = "cccccccc-0000-4000-8000-000000000003"
	const blockL = "019e5100-0000-7000-9000-0000000000a1"
	const blockM = "019e5100-0000-7000-9000-0000000000a2"

	c5Topic(t, pool, foreignTopic, "work", "SECRET-B LABEL")
	c5Node(t, pool, clusterL, "private", 4, "learnings", "")           // no identity yet
	c5Node(t, pool, clusterM, "private", 4, "learnings", foreignTopic) // forced defect
	c5Member(t, pool, blockL, "private", clusterL)
	c5Member(t, pool, blockM, "private", clusterM)

	ann, err := store.ClusterAnnotation(ctx, pool, []string{blockL, blockM}, []string{"private"})
	if err != nil {
		t.Fatalf("ClusterAnnotation must not fail on a partition without identity: %v", err)
	}
	if len(ann.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2: %+v", len(ann.Clusters), ann.Clusters)
	}
	for _, c := range ann.Clusters {
		if len(c.Topics) != 0 {
			t.Errorf("cluster %s carries handles %v — neither partition may resolve one", c.ClusterID, c.Topics)
		}
		if c.Label != "" {
			t.Errorf("cluster %s carries label %q — a foreign-scope topic must never serve its label", c.ClusterID, c.Label)
		}
		if c.Size != 4 {
			t.Errorf("cluster %s size = %d, want 4 — the topic join must not change the size", c.ClusterID, c.Size)
		}
	}
}

// Gate (ii) — stability across a rebuild. The rebuild is simulated exactly as
// the real one behaves: the 057 tables are torn down and rewritten (the cluster
// gets a NEW cluster_id because its smallest member left), while
// graph_cluster_topic is untouched and the new node row re-attaches the SAME
// topic_id (W3's continuation path).
//
// ROT-PROBE: derive the handle from cluster_id (e.g. select n.cluster_id as the
// handle) ⇒ the two reads disagree ⇒ red. That is §5.1 in one assert: a
// cluster-derived handle is not an identity, it is a per-run accident.
func TestClusterAnnotationTopicStableAcrossRebuild(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const clusterBefore = "019e5200-0000-7000-9000-00000000c001"
	const clusterAfter = "019e5200-0000-7000-9000-00000000c999"
	const topic = "dddddddd-0000-4000-8000-000000000004"
	const block = "019e5200-0000-7000-9000-0000000000a1"

	c5Topic(t, pool, topic, "private", "stabiles Thema")
	c5Node(t, pool, clusterBefore, "private", 2, "learnings", topic)
	c5Member(t, pool, block, "private", clusterBefore)

	first, err := store.ClusterAnnotation(ctx, pool, []string{block}, []string{"private"})
	if err != nil {
		t.Fatalf("ClusterAnnotation (before): %v", err)
	}
	if len(first.Clusters) != 1 || len(first.Clusters[0].Topics) != 1 {
		t.Fatalf("before rebuild: %+v", first.Clusters)
	}
	before := first.Clusters[0].Topics[0]

	// The teardown + re-persist of a rebuild: 057 rows go, the identity stays.
	for _, sql := range []string{
		`DELETE FROM graph_cluster_member`,
		`DELETE FROM graph_cluster_node`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("simulated teardown (%s): %v", sql, err)
		}
	}
	c5Node(t, pool, clusterAfter, "private", 2, "learnings", topic)
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_member (block_id, cluster_id, scope) VALUES ($1::uuid, $2::uuid, 'private')`,
		block, clusterAfter); err != nil {
		t.Fatalf("simulated re-persist: %v", err)
	}

	second, err := store.ClusterAnnotation(ctx, pool, []string{block}, []string{"private"})
	if err != nil {
		t.Fatalf("ClusterAnnotation (after): %v", err)
	}
	if len(second.Clusters) != 1 || len(second.Clusters[0].Topics) != 1 {
		t.Fatalf("after rebuild: %+v", second.Clusters)
	}
	if after := second.Clusters[0].Topics[0]; after != before {
		t.Errorf("handle changed across the rebuild: %s → %s (the cluster id did — that is the point)", before, after)
	}
	if second.Clusters[0].ClusterID == first.Clusters[0].ClusterID {
		t.Fatal("the fixture no longer renames the cluster — the stability assert proves nothing")
	}
}
