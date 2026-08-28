//go:build integration

// Wave C6 (Cluster-Topic-Map, design/03 §4.8/§5.7/§6.6 + §7 "C6", Masterplan
// K2 / A03-2) — the STORE half of the `cluster:<handle>` facet.
//
// Four properties:
//
//	(iii) SCOPE PURITY: the handle resolves through graph_cluster_topic JOIN
//	      graph_cluster_node, both under the caller's scopes, and the membership
//	      itself binds the C1 conjunction — a foreign partition's blocks are
//	      unreachable even when the caller knows its handle (risk R3);
//	(v)   PARTITION-SCHARF (K2): a handle names ONE scope-pure topic. The facet
//	      of the private half of a scope-crossing cluster returns the private
//	      members ONLY — never the work half's, even when the caller can read
//	      both. That is the K2 correction of the design's original "Union über
//	      alle sichtbaren Partitionen";
//	(i)   NO EXISTENCE ORACLE: an unknown handle and a known-but-foreign one are
//	      the same empty result set — the store answers with rows, never with a
//	      "not found" the handler could turn into a 404;
//	(iv)  PLAN GATE §6.6 against the WORST case (a cluster whose members all sit
//	      in the oldest corpus decile, i.e. last in the browse order): scanned
//	      heap rows ≤ 100 × outer limit, and no sequential scan on
//	      graph_cluster_member.
//
//	go test -tags=integration ./internal/store/ -run TestClusterFacet -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// c6Fixture builds one cluster with two partitions (private + work), each with
// its own topic handle and one member block, and returns the two handles.
func c6Fixture(t *testing.T, pool *pgxpool.Pool) (privateTopic, workTopic string) {
	t.Helper()
	const cluster = "019e6000-0000-7000-9000-00000000c001"
	privateTopic = "aaaaaaaa-0000-4000-8000-00000000f001"
	workTopic = "bbbbbbbb-0000-4000-8000-00000000f002"

	c5Topic(t, pool, privateTopic, "private", "private Hälfte")
	c5Topic(t, pool, workTopic, "work", "work Hälfte")
	c5Node(t, pool, cluster, "private", 1, "learnings", privateTopic)
	c5Node(t, pool, cluster, "work", 1, "learnings", workTopic)
	c5Member(t, pool, "019e6000-0000-7000-9000-0000000000a1", "private", cluster)
	c5Member(t, pool, "019e6000-0000-7000-9000-0000000000a2", "work", cluster)
	return privateTopic, workTopic
}

func c6Search(t *testing.T, pool *pgxpool.Pool, handle string, scopes []string) []store.BlockPreview {
	t.Helper()
	res, err := store.SearchBlocks(context.Background(), pool, nil, "", scopes, "", nil, 50, true, nil, nil, nil, nil, &handle)
	if err != nil {
		t.Fatalf("SearchBlocks(cluster=%s): %v", handle, err)
	}
	return res
}

// Gate (v) — K2: partition-scharf. The handle of the PRIVATE partition returns
// the private member only, although the caller reads both scopes and both
// partitions belong to the same Louvain cluster.
//
// ROT-PROBE: resolve the handle to the cluster alone — drop `AND n.scope =
// t.scope` (or the MemberOf conjunction) from clustersql.TopicMemberSubquery so
// the semi-join unions every partition of that cluster ⇒ the work block appears
// ⇒ red. That edit is exactly the pre-K2 design ("Union über alle sichtbaren
// Partitionen"), which A03-2 replaced: a handle names a scope-pure topic, so a
// facet that widens it hands the caller a set no handle describes.
func TestClusterFacetIsPartitionScharf(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	privateTopic, workTopic := c6Fixture(t, pool)

	got := c6Search(t, pool, privateTopic, []string{"private", "work"})
	if len(got) != 1 {
		t.Fatalf("private handle returned %d blocks, want exactly the private partition: %+v", len(got), got)
	}
	if got[0].Scope != "private" {
		t.Errorf("block scope = %s, want private", got[0].Scope)
	}

	// Symmetric: the work handle names the other half, and only it.
	if got := c6Search(t, pool, workTopic, []string{"private", "work"}); len(got) != 1 || got[0].Scope != "work" {
		t.Errorf("work handle returned %+v, want exactly the work partition", got)
	}
}

// Gate (iii) — scope purity: the same work handle, from a caller who may read
// `private` only, resolves to nothing. Both the node resolution and the
// membership carry the scope conjunction, so neither the topic row nor the
// member row is reachable.
//
// ROT-PROBE, and the honest result of running it: removing NodeVisible ALONE,
// or MemberOf ALONE, does NOT redden this fixture. Each is redundant given the
// other plus the join equality `m.scope = n.scope`, and the outer visibility
// predicate (`scope = ANY($1) OR id = ANY(grants)`) is a third, independent
// barrier that keeps a foreign-scoped BLOCK out no matter what the subquery
// resolves. Both conjunctions stay anyway: one site per table is the C1
// doctrine, the redundancy is deliberate defence in depth, and a future edit
// that drops the join equality (the probe of gate (v) below, which DOES redden)
// leaves them as the only remaining barrier.
func TestClusterFacetScopePurity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	_, workTopic := c6Fixture(t, pool)

	if got := c6Search(t, pool, workTopic, []string{"private"}); len(got) != 0 {
		t.Errorf("a foreign partition's handle returned %d blocks to a private-only caller: %+v", len(got), got)
	}
}

// Gate (i) — no existence oracle at the STORE boundary: an unknown handle, a
// foreign handle and a handle whose partition has no visible members all return
// the same thing — an empty result set, never a distinguishable error.
//
// ROT-PROBE: make the store return a "not found" sentinel for an unresolvable
// handle (so the handler could answer 404) ⇒ the error asserts below go red.
func TestClusterFacetUnknownHandleIsEmptyNotError(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	_, workTopic := c6Fixture(t, pool)
	ctx := context.Background()

	const unknown = "ffffffff-0000-4000-8000-0000000000ff"
	for _, h := range []string{unknown, workTopic} {
		res, err := store.SearchBlocks(ctx, pool, nil, "", []string{"private"}, "", nil, 50, true, nil, nil, nil, nil, &h)
		if err != nil {
			t.Fatalf("handle %s: unresolvable must be empty, not an error: %v", h, err)
		}
		if len(res) != 0 {
			t.Errorf("handle %s returned %d rows, want 0", h, len(res))
		}
	}
}

// Gate (iv) — the §6.6 plan gate against the WORST case.
//
// The expensive plan is not a seq scan on the membership table (the original
// gate checked that and the bad plan PASSED it, Linse 1 / A8): it is the browse
// path's `ORDER BY updated_at DESC, id DESC` index scan probing the semi-join
// row by row. When the cluster holds only OLD blocks, that plan discards the
// whole younger corpus before it finds its first hit.
//
// Fixture: 3.000 blocks, the facet's cluster owning the 30 OLDEST, plus filler
// memberships so the membership table is not so small that a sequential scan
// over it would be the correct plan. Acceptance: heap rows touched on
// context_blocks ≤ 100 × outer limit, plus the seq-scan exclusion as the
// second, weaker condition. Numbers go into the commit body.
//
// The measured answer (see the log lines): the planner drives the join from the
// MEMBERSHIP side — index scan on graph_cluster_member by cluster_id, then the
// blocks by primary key — so the browse ORDER BY never gets to discard the
// younger corpus, and the old-decile case costs the same as the new-decile
// control. That is the outcome §6.6 hoped for; it is reported as a measurement,
// not assumed. The control runs alongside precisely so "the worst case is
// cheap" cannot be confused with "the gate measures nothing".
//
// ROT-PROBE / instrument check: the bad plan cannot be provoked THROUGH the
// production statement (see the note at the instrument check below — Postgres
// flattens every fence back into the same semi-join), so the pathological shape
// is measured directly, with the same metric, at the end of this test. It costs
// 10× the heap rows and blows the ceiling — which is what makes the green above
// a statement about the plan rather than about the fixture.
func TestClusterFacetPlanGate(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const total = 3000
	const clusterOld = "019e6100-0000-7000-9000-00000000c001"
	const clusterNew = "019e6100-0000-7000-9000-00000000c002"
	const topicOld = "aaaaaaaa-0000-4000-8000-00000000f101"
	const topicNew = "bbbbbbbb-0000-4000-8000-00000000f102"
	c5Topic(t, pool, topicOld, "private", "alt")
	c5Topic(t, pool, topicNew, "private", "neu")
	c5Node(t, pool, clusterOld, "private", total/10, "learnings", topicOld)
	c5Node(t, pool, clusterNew, "private", total/10, "learnings", topicNew)

	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("019e6200-0000-7000-9000-%012x", i)
		ts := base.Add(time.Duration(i) * time.Minute)
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, created_at, updated_at)
			 VALUES ($1::uuid, 'plangate', $2, 'plan gate fixture', 'private', $3, $3)`,
			id, fmt.Sprintf("blk %05d", i), ts); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		switch {
		case i < total/10: // the OLDEST DECILE — the §6.6 worst case verbatim
			c6Membership(t, pool, id, clusterOld)
		case i >= total-total/10: // the newest decile → the control
			c6Membership(t, pool, id, clusterNew)
		default:
			// Filler membership in one of 100 topic-less clusters. Without it
			// graph_cluster_member would hold 60 rows and a sequential scan over
			// it would be the CORRECT plan — the seq-scan condition would then
			// measure the fixture size instead of the query.
			c6Membership(t, pool, id, fmt.Sprintf("019e6300-0000-7000-9000-%012x", i%100))
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks; ANALYZE graph_cluster_member; ANALYZE graph_cluster_node`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	const limit = 10
	worst := c6Explain(t, pool, topicOld, limit)
	control := c6Explain(t, pool, topicNew, limit)
	t.Logf("plan gate — worst case (oldest decile): heap rows %d, buffers %d, seq scan on member: %v, plan %v",
		worst.heapRows, worst.buffers, worst.memberSeqScan, worst.shape)
	t.Logf("plan gate — control  (newest blocks) : heap rows %d, buffers %d", control.heapRows, control.buffers)

	if worst.heapRows > 100*limit {
		t.Errorf("worst case touched %d heap rows on context_blocks, ceiling is %d (100 × outer limit)", worst.heapRows, 100*limit)
	}
	if worst.memberSeqScan {
		t.Error("sequential scan on graph_cluster_member — the second, weaker condition of §6.6")
	}
	if worst.rows != limit || control.rows != limit {
		t.Fatalf("fixture no longer returns a full page (worst %d, control %d) — the gate would measure a different query", worst.rows, control.rows)
	}

	// INSTRUMENT CHECK — the gate has to be able to go red, or its green says
	// nothing. Three attempts to provoke the bad plan through the PRODUCTION
	// statement all failed, and that failure is itself the result: PostgreSQL
	// flattens a correlated `EXISTS (… AND m.block_id = context_blocks.id)` and
	// an `IN (… OFFSET 0)` fence back into the same semi-join, and even with
	// enable_sort/enable_nestloop/enable_hashjoin/enable_mergejoin off it keeps
	// driving from the membership side. The browse-driven plan §6.6 warns about
	// is therefore not reachable from this statement — it is reachable from a
	// statement that FENCES the sublink, which is the shape a future refactor
	// could produce by accident (a volatile predicate inside the EXISTS is
	// enough). That statement is measured here: same fixture, same limit, same
	// metric — and it must blow the ceiling.
	fenced := c6ExplainRaw(t, pool, `
		SELECT id FROM context_blocks
		WHERE NOT is_archived AND category = 'plangate'
		  AND scope = ANY($2::text[])
		  AND EXISTS (SELECT 1
		                FROM graph_cluster_topic t
		                JOIN graph_cluster_node n ON n.topic_id = t.topic_id AND n.scope = t.scope
		                JOIN graph_cluster_member m ON m.cluster_id = n.cluster_id AND m.scope = n.scope
		               WHERE t.topic_id = $1::uuid AND m.block_id = context_blocks.id
		                 AND n.scope = ANY($2::text[]) AND m.scope = ANY($2::text[])
		                 AND random() >= 0)
		ORDER BY updated_at DESC, id DESC LIMIT `+fmt.Sprint(limit),
		topicOld, []string{"private"})
	t.Logf("plan gate — instrument check (sublink fenced, the plan §6.6 warns about): heap rows %d, buffers %d, plan %v",
		fenced.heapRows, fenced.buffers, fenced.shape)
	if fenced.heapRows <= 100*limit {
		t.Errorf("the pathological plan touched only %d heap rows — the metric no longer discriminates, so the ceiling assert above proves nothing", fenced.heapRows)
	}
}

func c6Membership(t *testing.T, pool *pgxpool.Pool, blockID, clusterID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO graph_cluster_member (block_id, cluster_id, scope) VALUES ($1::uuid, $2::uuid, 'private')`,
		blockID, clusterID); err != nil {
		t.Fatalf("membership %s: %v", blockID, err)
	}
}

type c6Plan struct {
	heapRows      int
	buffers       int
	rows          int
	memberSeqScan bool
	shape         []string
}

// c6Explain EXPLAIN (ANALYZE, BUFFERS)s the PRODUCTION statement — built by the
// very function the handler calls, not a copy — and folds the plan tree into
// the three numbers §6.6 asks for.
func c6Explain(t *testing.T, pool *pgxpool.Pool, handle string, limit int) c6Plan {
	t.Helper()
	sql, args := store.SearchBlocksSQLForTest(store.SearchBlocksParams{
		ReadScopes: []string{"private"}, Category: "plangate", Limit: limit, Compact: true, Cluster: &handle,
	})
	return c6ExplainRaw(t, pool, sql, args...)
}

// c6ExplainRaw is the measuring instrument itself: it explains whatever
// statement it is handed, so the gate can measure the production statement AND
// the pathological one with the SAME metric.
func c6ExplainRaw(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) c6Plan {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("explain json: %v", err)
	}
	out := c6Plan{}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		num := func(key string) int {
			if v, ok := node[key].(float64); ok {
				return int(v)
			}
			return 0
		}
		rel, _ := node["Relation Name"].(string)
		nodeType, _ := node["Node Type"].(string)
		out.shape = append(out.shape, strings.TrimSpace(nodeType+" "+rel))
		if rel == "context_blocks" {
			// Actual Rows is the PER-LOOP average — an inner scan of a nested
			// loop reports 1 while running N times. Multiplying by Actual Loops
			// is what turns it into "heap rows this query touched".
			loops := num("Actual Loops")
			if loops == 0 {
				loops = 1
			}
			out.heapRows += (num("Actual Rows") + num("Rows Removed by Filter")) * loops
		}
		if rel == "graph_cluster_member" && strings.Contains(nodeType, "Seq Scan") {
			out.memberSeqScan = true
		}
		out.buffers += num("Shared Hit Blocks") + num("Shared Read Blocks")
		if kids, ok := node["Plans"].([]any); ok {
			for _, k := range kids {
				walk(k.(map[string]any))
			}
		}
	}
	walk(plans[0].Plan)
	if v, ok := plans[0].Plan["Actual Rows"].(float64); ok {
		out.rows = int(v)
	}
	return out
}
