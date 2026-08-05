//go:build integration

// W3 DB gates — stable topic identity across rebuilds (design/01 §7 W3,
// amendments A01-2 / A01-5 / A01-6 / A01-7).
//
// Most gates drive `persist` with a HAND-BUILT clustering instead of letting
// Louvain decide. That is deliberate: the mechanism under test is the
// assignment, not the community detection, and a split/merge fixture whose
// partition is produced by gonum can only be asserted after the fact. The two
// gates that DO need the whole pipeline (the scope-move isolation G10 and the
// node-cap freeze G12) run the real Rebuild.
//
// Every subtest owns its own SCOPE. Scope is the partition key of the whole
// mechanism, so per-scope isolation is both cheap and closer to production
// than a shared partition would be.
package overview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

var w3Types = []string{"knowledge"}

// w3ID is a stable, readable fixture uuid.
func w3ID(n int) string { return fmt.Sprintf("019e0000-0000-7000-9000-%012d", n) }

// w3Blocks inserts n blocks into one scope and returns their ids.
func w3Blocks(t *testing.T, pool *pgxpool.Pool, scope string, first, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = w3ID(first + i)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO context_blocks (id, scope, category, title, content)
		SELECT u::uuid, $2, 'learnings', 'w3 ' || u, 'w3 fixture'
		  FROM unnest($1::text[]) AS u`, ids, scope); err != nil {
		t.Fatalf("w3Blocks(%s): %v", scope, err)
	}
	return ids
}

// w3Group assigns every id to one cluster (cluster_id = smallest member uuid,
// the production rule) and gives every member the same intra-cluster degree.
type w3Group struct {
	members []string
	degrees []float64 // optional; nil → all 1
}

// w3Run persists ONE generation of one scope. groups is this run's partition.
func w3Run(t *testing.T, pool *pgxpool.Pool, scope string, retention time.Duration, groups ...w3Group) Stats {
	t.Helper()
	assign := map[string]string{}
	scopes := map[string]string{}
	deg := map[string]float64{}
	for _, g := range groups {
		cluster := ""
		for _, m := range g.members {
			if cluster == "" || m < cluster {
				cluster = m
			}
		}
		for i, m := range g.members {
			assign[m] = cluster
			scopes[m] = scope
			deg[m] = 1
			if g.degrees != nil {
				deg[m] = g.degrees[i]
			}
		}
	}
	st, err := persist(context.Background(), pool,
		clustering{blockToCluster: assign, intraDegree: deg, clusterCount: len(groups)},
		Options{Resolution: 1.0, VisibleTypes: w3Types, ScopeFilter: []string{scope}, TombstoneRetention: retention},
		scopes, tallyScopes(scopes))
	if err != nil {
		t.Fatalf("w3Run(%s): %v", scope, err)
	}
	if st.Skipped {
		t.Fatalf("w3Run(%s) skipped: %s", scope, st.SkipReason)
	}
	return st
}

// w3TopicOf returns the topic id of the node row a member currently sits in.
func w3TopicOf(t *testing.T, pool *pgxpool.Pool, scope, member string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		SELECT n.topic_id::text FROM graph_cluster_member m
		  JOIN graph_cluster_node n ON n.cluster_id = m.cluster_id AND n.scope = m.scope
		 WHERE m.block_id = $1::uuid AND m.scope = $2`, member, scope).Scan(&id)
	if err != nil {
		t.Fatalf("w3TopicOf(%s, %s): %v", scope, member, err)
	}
	return id
}

type w3Topic struct {
	scope      string
	originKind string
	origin     *string
	mergedInto *string
	retired    bool
	coreBlocks []string
}

func w3TopicRow(t *testing.T, pool *pgxpool.Pool, id string) w3Topic {
	t.Helper()
	var row w3Topic
	var retiredAt *time.Time
	if err := pool.QueryRow(context.Background(), `
		SELECT scope, origin_kind, origin_topic_id::text, merged_into::text, retired_at,
		       (SELECT array_agg(x::text ORDER BY x::text) FROM unnest(core_blocks) x)
		  FROM graph_cluster_topic WHERE topic_id = $1::uuid`, id).
		Scan(&row.scope, &row.originKind, &row.origin, &row.mergedInto, &retiredAt, &row.coreBlocks); err != nil {
		t.Fatalf("w3TopicRow(%s): %v", id, err)
	}
	row.retired = retiredAt != nil
	return row
}

// ─────────────────────────────────────────────────────────────────────────────

func TestW3TopicIdentity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	const retention = 45 * 24 * time.Hour

	// ── G1: continuity. Two runs, no corpus change ⇒ the same topic_id. ──────
	//
	// RED against the pre-W3 state: graph_cluster_node.topic_id stayed NULL,
	// so the very first Scan below fails with "cannot scan NULL into *string";
	// RED against a variant that mints per run: the two ids differ.
	t.Run("G1 continuity across two runs", func(t *testing.T) {
		const scope = "g1"
		ids := w3Blocks(t, pool, scope, 1000, 4)
		w3Run(t, pool, scope, retention, w3Group{members: ids})
		first := w3TopicOf(t, pool, scope, ids[0])

		st := w3Run(t, pool, scope, retention, w3Group{members: ids})
		if got := w3TopicOf(t, pool, scope, ids[0]); got != first {
			t.Fatalf("topic id changed on an unchanged corpus: %s → %s", first, got)
		}
		if st.TopicsCarried != 1 || st.TopicsBorn != 0 || st.TopicsRetired != 0 {
			t.Fatalf("unchanged corpus: carried=%d born=%d retired=%d, want 1/0/0",
				st.TopicsCarried, st.TopicsBorn, st.TopicsRetired)
		}
		if w3TopicRow(t, pool, first).originKind != "birth" {
			t.Fatal("first generation must be a birth")
		}
	})

	// ── G2: injectivity. The 50/50 split is the ONLY shape that can violate
	// it: with the relational containment (A01-6) at most one cluster can hold
	// MORE than half of a topic, so a 6/5 split is already settled by the
	// criterion alone. At 5/5 both halves qualify and only the second half of
	// the mutual plurality (ov_best_topic) keeps the assignment injective.
	// That is the documented 50/50 semantics of decision E1-01.
	t.Run("G2 injectivity — 50/50 split, one continuation", func(t *testing.T) {
		const scope = "g2"
		ids := w3Blocks(t, pool, scope, 2000, 10)
		w3Run(t, pool, scope, retention, w3Group{members: ids})
		mother := w3TopicOf(t, pool, scope, ids[0])

		st := w3Run(t, pool, scope, retention,
			w3Group{members: ids[:5]}, w3Group{members: ids[5:]})
		if st.TopicsCarried != 1 {
			t.Fatalf("50/50 split carried %d topics, want exactly 1 (deterministic tiebreak)", st.TopicsCarried)
		}
		a, b := w3TopicOf(t, pool, scope, ids[0]), w3TopicOf(t, pool, scope, ids[5])
		if a == b {
			t.Fatal("both halves share one topic — injectivity broken")
		}
		if a != mother {
			t.Fatalf("the uuid-smaller half must carry the mother topic: %s vs %s", a, mother)
		}
		other := w3TopicRow(t, pool, b)
		if other.originKind != "split" || other.origin == nil || *other.origin != mother {
			t.Fatalf("losing half: origin_kind=%s origin=%v, want split of %s", other.originKind, other.origin, mother)
		}
	})

	// RED PROBE for G2: drop the ov_best_topic half of the mutual plurality —
	// both halves then claim the mother and ov_match's UNIQUE index breaks
	// with 23505 (the B2 identity fusion, caught at its cause).
	t.Run("G2 red probe — greedy without ov_best_topic ⇒ 23505", func(t *testing.T) {
		const scope = "g2red"
		ids := w3Blocks(t, pool, scope, 2100, 10)
		w3Run(t, pool, scope, retention, w3Group{members: ids})

		restore := carrySQL
		carrySQL = `
INSERT INTO ov_carry (cluster_id, scope, topic_id, ov)
SELECT c.cluster_id, c.scope, c.topic_id, c.ov
  FROM ov_best_cluster c
  JOIN ov_prev_size    s ON s.topic_id = c.topic_id
 WHERE c.ov * 2 >= s.size_prev`
		defer func() { carrySQL = restore }()

		assign, scopes, deg := map[string]string{}, map[string]string{}, map[string]float64{}
		for i, m := range ids {
			cluster := ids[0]
			if i >= 5 {
				cluster = ids[5]
			}
			assign[m], scopes[m], deg[m] = cluster, scope, 1
		}
		_, err := persist(context.Background(), pool,
			clustering{blockToCluster: assign, intraDegree: deg},
			Options{Resolution: 1.0, VisibleTypes: w3Types, ScopeFilter: []string{scope}, TombstoneRetention: retention},
			scopes, tallyScopes(scopes))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("greedy carry: err=%v, want SQLSTATE 23505 on ov_match_topic_uq (B2)", err)
		}
	})

	// ── G2b: merge semantics. Two communities become one ⇒ exactly one topic
	// continues, the other is retired WITH a merged_into pointer.
	t.Run("G2b merge sets merged_into", func(t *testing.T) {
		const scope = "g2b"
		ids := w3Blocks(t, pool, scope, 3000, 6)
		w3Run(t, pool, scope, retention, w3Group{members: ids[:3]}, w3Group{members: ids[3:]})
		left, right := w3TopicOf(t, pool, scope, ids[0]), w3TopicOf(t, pool, scope, ids[3])

		st := w3Run(t, pool, scope, retention, w3Group{members: ids})
		if st.TopicsCarried != 1 || st.TopicsRetired != 1 {
			t.Fatalf("merge: carried=%d retired=%d, want 1/1", st.TopicsCarried, st.TopicsRetired)
		}
		winner := w3TopicOf(t, pool, scope, ids[0])
		loser := left
		if winner == left {
			loser = right
		}
		row := w3TopicRow(t, pool, loser)
		if !row.retired {
			t.Fatalf("absorbed topic %s not retired", loser)
		}
		if row.mergedInto == nil || *row.mergedInto != winner {
			t.Fatalf("merged_into = %v, want %s — a stale reference must be redirectable, not dangling", row.mergedInto, winner)
		}
	})

	// ── G3: scope purity. One community, members in two scopes ⇒ TWO topics
	// with disjoint scopes and scope-pure cores. Global run (nil ScopeFilter)
	// because the fixture spans partitions by construction.
	t.Run("G3 scope-crossing cluster yields two scope-pure topics", func(t *testing.T) {
		alpha := w3Blocks(t, pool, "g3a", 4000, 3)
		beta := w3Blocks(t, pool, "g3b", 4100, 3)
		all := append(append([]string{}, alpha...), beta...)

		assign, scopes, deg := map[string]string{}, map[string]string{}, map[string]float64{}
		for _, m := range all {
			assign[m], deg[m] = all[0], 1 // ONE cluster over both scopes
		}
		for _, m := range alpha {
			scopes[m] = "g3a"
		}
		for _, m := range beta {
			scopes[m] = "g3b"
		}
		if _, err := persist(context.Background(), pool,
			clustering{blockToCluster: assign, intraDegree: deg},
			Options{Resolution: 1.0, VisibleTypes: w3Types, TombstoneRetention: retention},
			scopes, tallyScopes(scopes)); err != nil {
			t.Fatalf("global persist: %v", err)
		}

		ta, tb := w3TopicOf(t, pool, "g3a", alpha[0]), w3TopicOf(t, pool, "g3b", beta[0])
		if ta == tb {
			t.Fatal("one topic across two scopes — that is the B1 label-leak handle (I1)")
		}
		rowA, rowB := w3TopicRow(t, pool, ta), w3TopicRow(t, pool, tb)
		if rowA.scope != "g3a" || rowB.scope != "g3b" {
			t.Fatalf("topic scopes = %s/%s, want g3a/g3b", rowA.scope, rowB.scope)
		}
		for _, id := range rowA.coreBlocks {
			if !strings.Contains(strings.Join(alpha, " "), id) {
				t.Fatalf("core of the g3a topic holds the foreign block %s (I2 broken)", id)
			}
		}
		for _, id := range rowB.coreBlocks {
			if !strings.Contains(strings.Join(beta, " "), id) {
				t.Fatalf("core of the g3b topic holds the foreign block %s (I2 broken)", id)
			}
		}
	})

	// ── G4: AS MATERIALIZED. Every minted topic_id must exist exactly once and
	// the node row must reference a real topic row.
	t.Run("G4 MATERIALIZED — minted ids are written once", func(t *testing.T) {
		const scope = "g4"
		ids := w3Blocks(t, pool, scope, 5000, 6)
		st := w3Run(t, pool, scope, retention,
			w3Group{members: ids[:2]}, w3Group{members: ids[2:4]}, w3Group{members: ids[4:]})
		if st.TopicsBorn != 3 {
			t.Fatalf("born=%d, want 3", st.TopicsBorn)
		}
		var topics, nodes, orphans int
		ctx := context.Background()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_topic WHERE scope = $1`, scope).Scan(&topics); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_node WHERE scope = $1`, scope).Scan(&nodes); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM graph_cluster_node n
			 WHERE n.scope = $1 AND NOT EXISTS (
			   SELECT 1 FROM graph_cluster_topic t WHERE t.topic_id = n.topic_id)`, scope).Scan(&orphans); err != nil {
			t.Fatal(err)
		}
		if topics != 3 || nodes != 3 || orphans != 0 {
			t.Fatalf("topics=%d nodes=%d orphans=%d, want 3/3/0", topics, nodes, orphans)
		}
	})

	// RED PROBE for G4 (B3, gen_random_uuid() double evaluation) — with a
	// MEASURED correction to the design's assumption.
	//
	// design/01 §5.3-B3 expects that merely dropping AS MATERIALIZED lets the
	// planner inline `minted` into both references and produce two different
	// uuids. Measured against PostgreSQL 18 that does NOT happen: since PG 12
	// the planner only auto-inlines a CTE that is referenced EXACTLY ONCE, and
	// `minted` is referenced twice (the graph_cluster_topic insert and the
	// ov_match insert). The first half of this probe records that finding —
	// the un-materialized form still behaves correctly today.
	//
	// The hint therefore is not what stops the defect today; the second
	// reference is. It stays anyway, because it is the only thing that would
	// stop the defect after a refactor that leaves one reference — and the
	// second half of this probe shows what that defect looks like: two
	// independent evaluations, node row pointing at a topic row that does not
	// exist, FK 23503.
	t.Run("G4 red probe — the B3 double evaluation", func(t *testing.T) {
		const scope = "g4red"
		ids := w3Blocks(t, pool, scope, 5100, 6)
		run := func(scope string, ids []string) error {
			assign, scopes, deg := map[string]string{}, map[string]string{}, map[string]float64{}
			for i, m := range ids {
				assign[m], scopes[m], deg[m] = ids[(i/2)*2], scope, 1
			}
			_, err := persist(context.Background(), pool,
				clustering{blockToCluster: assign, intraDegree: deg},
				Options{Resolution: 1.0, VisibleTypes: w3Types, ScopeFilter: []string{scope}, TombstoneRetention: retention},
				scopes, tallyScopes(scopes))
			return err
		}

		restore := mintTemplate
		defer func() { mintTemplate = restore }()

		mintTemplate = strings.Replace(restore, "minted AS MATERIALIZED (", "minted AS (", 1)
		if mintTemplate == restore {
			t.Fatal("probe did not patch the statement")
		}
		if err := run(scope, ids); err != nil {
			t.Fatalf("without AS MATERIALIZED: %v — expected the twice-referenced CTE to stay materialized", err)
		}
		t.Log("MEASURED (PG18): dropping AS MATERIALIZED alone does not reproduce B3 — a twice-referenced CTE is never auto-inlined. The hint guards the single-reference refactor, not today's shape.")

		// The actual defect shape: evaluate the volatile function twice.
		mintTemplate = strings.Replace(restore,
			"SELECT cluster_id, scope, topic_id, 0, false FROM minted",
			"SELECT cluster_id, scope, gen_random_uuid(), 0, false FROM minted", 1)
		if mintTemplate == restore {
			t.Fatal("probe did not patch the ov_match projection")
		}
		err := run("g4red2", w3Blocks(t, pool, "g4red2", 5200, 6))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("double evaluation: err=%v, want SQLSTATE 23503 (topic_id FK, B3)", err)
		}
	})

	// ── G5: split vs birth. A cluster whose majority substance came out of one
	// old topic is a split WITH lineage; a cluster with no meaningful
	// predecessor is a plain birth.
	t.Run("G5 split carries lineage, birth does not", func(t *testing.T) {
		const scope = "g5"
		old := w3Blocks(t, pool, scope, 6000, 11)
		w3Run(t, pool, scope, retention, w3Group{members: old})
		mother := w3TopicOf(t, pool, scope, old[0])

		fresh := w3Blocks(t, pool, scope, 6100, 4)
		w3Run(t, pool, scope, retention,
			w3Group{members: old[:6]}, w3Group{members: old[6:]}, w3Group{members: fresh})

		if got := w3TopicOf(t, pool, scope, old[0]); got != mother {
			t.Fatalf("the 6-of-11 half must continue the mother (6*2 >= 11): %s != %s", got, mother)
		}
		splitRow := w3TopicRow(t, pool, w3TopicOf(t, pool, scope, old[6]))
		if splitRow.originKind != "split" || splitRow.origin == nil || *splitRow.origin != mother {
			t.Fatalf("5-of-11 half: kind=%s origin=%v, want split of %s", splitRow.originKind, splitRow.origin, mother)
		}
		birthRow := w3TopicRow(t, pool, w3TopicOf(t, pool, scope, fresh[0]))
		if birthRow.originKind != "birth" || birthRow.origin != nil {
			t.Fatalf("untouched new community: kind=%s origin=%v, want birth/NULL", birthRow.originKind, birthRow.origin)
		}
	})

	// ── G6 (Amendment A01-6): the relational continuation criterion, in both
	// directions the absolute threshold gets wrong.
	t.Run("G6a stable singleton continues (1 of 1)", func(t *testing.T) {
		const scope = "g6s"
		ids := w3Blocks(t, pool, scope, 7000, 1)
		w3Run(t, pool, scope, retention, w3Group{members: ids})
		first := w3TopicOf(t, pool, scope, ids[0])
		st := w3Run(t, pool, scope, retention, w3Group{members: ids})
		if st.TopicsCarried != 1 {
			t.Fatalf("stable singleton: carried=%d, want 1 (1*2 >= 1)", st.TopicsCarried)
		}
		if got := w3TopicOf(t, pool, scope, ids[0]); got != first {
			t.Fatalf("stable singleton changed identity: %s → %s", first, got)
		}
	})

	t.Run("G6b mega-cluster noise never inherits (2 of 300)", func(t *testing.T) {
		const scope = "g6n"
		ids := w3Blocks(t, pool, scope, 8000, 300)
		w3Run(t, pool, scope, retention, w3Group{members: ids})
		mother := w3TopicOf(t, pool, scope, ids[0])

		// The 300 shatter into 150 pairs: every pair holds 2 of 300, so every
		// pair wins the plurality of NOTHING but the containment fails.
		groups := make([]w3Group, 0, 150)
		for i := 0; i < 300; i += 2 {
			groups = append(groups, w3Group{members: ids[i : i+2]})
		}
		st := w3Run(t, pool, scope, retention, groups...)
		if st.TopicsCarried != 0 {
			t.Fatalf("2-of-300 noise carried %d topics, want 0 (4 >= 300 is false)", st.TopicsCarried)
		}
		if !w3TopicRow(t, pool, mother).retired {
			t.Fatal("the shattered mother topic must retire")
		}
	})

	// RED PROBE for G6: the retired absolute threshold (>= 2) colours BOTH
	// fixtures red at once — the stable singleton loses its identity (1 < 2)
	// and the noise pair inherits a 300-block topic (2 >= 2).
	t.Run("G6 red probe — absolute threshold breaks both directions", func(t *testing.T) {
		restore := carrySQL
		carrySQL = strings.Replace(carrySQL, "WHERE c.ov * 2 >= s.size_prev", "WHERE c.ov >= 2", 1)
		defer func() { carrySQL = restore }()
		if carrySQL == restore {
			t.Fatal("red probe did not patch the criterion")
		}

		single := w3Blocks(t, pool, "g6sr", 9000, 1)
		w3Run(t, pool, "g6sr", retention, w3Group{members: single})
		before := w3TopicOf(t, pool, "g6sr", single[0])
		if st := w3Run(t, pool, "g6sr", retention, w3Group{members: single}); st.TopicsCarried != 0 {
			t.Fatalf("absolute threshold still carried the singleton (%d) — probe ineffective", st.TopicsCarried)
		}
		if w3TopicOf(t, pool, "g6sr", single[0]) == before {
			t.Fatal("absolute threshold kept the singleton identity — probe ineffective")
		}

		noise := w3Blocks(t, pool, "g6nr", 9100, 300)
		w3Run(t, pool, "g6nr", retention, w3Group{members: noise})
		mother := w3TopicOf(t, pool, "g6nr", noise[0])
		groups := make([]w3Group, 0, 150)
		for i := 0; i < 300; i += 2 {
			groups = append(groups, w3Group{members: noise[i : i+2]})
		}
		if st := w3Run(t, pool, "g6nr", retention, groups...); st.TopicsCarried != 1 {
			t.Fatalf("absolute threshold carried %d topics off a 2-of-300 overlap, want 1 — probe ineffective", st.TopicsCarried)
		}
		if w3TopicRow(t, pool, mother).retired {
			t.Fatal("absolute threshold retired the mother anyway — probe ineffective")
		}
	})

	// ── A01-2 / E2-01: the tombstone re-attach. A batch import tears the
	// partition apart for one generation; when the community re-appears it
	// must find its old identity instead of being reborn.
	t.Run("import simulation — partition break over two runs re-attaches", func(t *testing.T) {
		const scope = "imp"
		ids := w3Blocks(t, pool, scope, 10000, 6)
		filler := w3Blocks(t, pool, scope, 10100, 2)
		w3Run(t, pool, scope, retention, w3Group{members: ids}, w3Group{members: filler})
		mother := w3TopicOf(t, pool, scope, ids[0])
		core := w3TopicRow(t, pool, mother).coreBlocks
		if len(core) == 0 {
			t.Fatal("the topic row carries no core — nothing would survive the teardown")
		}

		// Run 2: the import churn drops the six blocks out of the node cut.
		st2 := w3Run(t, pool, scope, retention, w3Group{members: filler})
		if st2.TopicsRetired != 1 {
			t.Fatalf("run 2: retired=%d, want 1 (the mother becomes a tombstone)", st2.TopicsRetired)
		}
		if !w3TopicRow(t, pool, mother).retired {
			t.Fatal("run 2 left the mother alive")
		}

		// Run 3: the import settles and the community re-appears.
		st3 := w3Run(t, pool, scope, retention, w3Group{members: ids}, w3Group{members: filler})
		if st3.TopicsReattached != 1 || st3.TopicsBorn != 0 {
			t.Fatalf("run 3: reattached=%d born=%d, want 1/0", st3.TopicsReattached, st3.TopicsBorn)
		}
		if got := w3TopicOf(t, pool, scope, ids[0]); got != mother {
			t.Fatalf("identity broke across the import: %s != %s", got, mother)
		}
		if w3TopicRow(t, pool, mother).retired {
			t.Fatal("re-attached topic is still retired")
		}
	})

	// ── K2-1 + K2-4: the THREE-WAY TEAR. This is the literal E2-01 case and
	// the one the first W3 build could not reach: the intermediate generation
	// hands out identities, so the reassembled cluster HAS a continuation
	// candidate and never became a birth candidate at all.
	t.Run("three-way tear — the reassembled cluster re-attaches the mother, not a fragment", func(t *testing.T) {
		const scope = "tear"
		ids := w3Blocks(t, pool, scope, 14000, 10)
		w3Run(t, pool, scope, retention, w3Group{members: ids})
		mother := w3TopicOf(t, pool, scope, ids[0])
		motherCore := w3TopicRow(t, pool, mother).coreBlocks

		// Run 2: the import tears the community into 4/4/2. The largest piece
		// holds 4 of 10 — below half — so nothing continues the mother.
		st2 := w3Run(t, pool, scope, retention,
			w3Group{members: ids[:4]}, w3Group{members: ids[4:8]}, w3Group{members: ids[8:]})
		if st2.TopicsCarried != 0 || st2.TopicsRetired != 1 || st2.TopicsBorn != 3 {
			t.Fatalf("tear run: carried=%d retired=%d born=%d split=%d, want 0/1/3/0",
				st2.TopicsCarried, st2.TopicsRetired, st2.TopicsBorn, st2.TopicsSplit)
		}
		// K2-4: three plain births after a topic DEATH — never 'split', which
		// would report a lineage branch where there was a tear. The lineage
		// POINTER is kept, and it is what run 3 navigates by.
		if st2.TopicsSplit != 0 {
			t.Fatalf("tear produced %d splits — a split presupposes a surviving mother", st2.TopicsSplit)
		}
		frag := w3TopicRow(t, pool, w3TopicOf(t, pool, scope, ids[0]))
		if frag.originKind != "birth" {
			t.Fatalf("fragment origin_kind = %s, want birth", frag.originKind)
		}
		if frag.origin == nil || *frag.origin != mother {
			t.Fatalf("fragment origin_topic_id = %v, want the torn mother %s", frag.origin, mother)
		}

		// Run 3: the community reassembles. The reassembled cluster continues
		// a FRAGMENT by the ordinary rule (4 of 4) — the tombstone must
		// outrank it, because 100 % of the mother's core lies inside.
		st3 := w3Run(t, pool, scope, retention, w3Group{members: ids})
		if st3.TopicsReattached != 1 || st3.TopicsCarried != 0 || st3.TopicsBorn != 0 {
			t.Fatalf("reassembly: reattached=%d carried=%d born=%d, want 1/0/0",
				st3.TopicsReattached, st3.TopicsCarried, st3.TopicsBorn)
		}
		if got := w3TopicOf(t, pool, scope, ids[0]); got != mother {
			t.Fatalf("the reassembled community kept a fragment identity (%s) instead of the mother %s", got, mother)
		}
		back := w3TopicRow(t, pool, mother)
		if back.retired {
			t.Fatal("the mother is still a tombstone")
		}
		if len(back.coreBlocks) == 0 || len(motherCore) == 0 {
			t.Fatal("fixture: the mother carried no core")
		}
		// All three fragments are gone, each pointing at the mother.
		var frags, pointing int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*), count(*) FILTER (WHERE merged_into = $2::uuid)
			  FROM graph_cluster_topic
			 WHERE scope = $1 AND topic_id <> $2::uuid AND retired_at IS NOT NULL`,
			scope, mother).Scan(&frags, &pointing); err != nil {
			t.Fatal(err)
		}
		if frags != 3 || pointing != 3 {
			t.Fatalf("fragments: %d retired, %d with merged_into=mother, want 3/3", frags, pointing)
		}
	})

	// RED PROBE for the tear gate: drop the tombstone precedence — the
	// reassembled cluster then keeps a fragment identity and the mother stays
	// a tombstone forever.
	t.Run("three-way tear red probe — without tombstone precedence", func(t *testing.T) {
		const scope = "tearred"
		ids := w3Blocks(t, pool, scope, 15000, 10)
		w3Run(t, pool, scope, retention, w3Group{members: ids})
		mother := w3TopicOf(t, pool, scope, ids[0])
		w3Run(t, pool, scope, retention,
			w3Group{members: ids[:4]}, w3Group{members: ids[4:8]}, w3Group{members: ids[8:]})

		restore := tombWinsSQLForProbe()
		defer restore()

		st3 := w3Run(t, pool, scope, retention, w3Group{members: ids})
		if st3.TopicsReattached != 0 || st3.TopicsCarried != 1 {
			t.Fatalf("probe ineffective: reattached=%d carried=%d, want 0/1", st3.TopicsReattached, st3.TopicsCarried)
		}
		if got := w3TopicOf(t, pool, scope, ids[0]); got == mother {
			t.Fatal("probe ineffective: the mother was re-attached anyway")
		}
		if !w3TopicRow(t, pool, mother).retired {
			t.Fatal("probe ineffective: the mother is not a tombstone")
		}
	})

	// K2-1 counter-gate: the precedence must NOT let a tombstone steal a
	// cluster from an INDEPENDENT living topic. Organic growth outranks old
	// graves — that is the other half of decision E2-01.
	t.Run("independent living topic outranks an unrelated tombstone", func(t *testing.T) {
		const scope = "indep"
		grave := w3Blocks(t, pool, scope, 16000, 4)
		other := w3Blocks(t, pool, scope, 16100, 4)
		w3Run(t, pool, scope, retention, w3Group{members: grave}, w3Group{members: other})
		buried := w3TopicOf(t, pool, scope, grave[0])

		// The grave's community disappears entirely ⇒ tombstone with core.
		w3Run(t, pool, scope, retention, w3Group{members: other})
		if !w3TopicRow(t, pool, buried).retired {
			t.Fatal("fixture: the grave topic did not retire")
		}
		living := w3TopicOf(t, pool, scope, other[0])

		// Now the grave's blocks come back INSIDE the living community. The
		// living topic keeps the cluster (4 of 4 continuation) — the tombstone
		// has no ancestry claim on it.
		st := w3Run(t, pool, scope, retention, w3Group{members: append(append([]string{}, other...), grave...)})
		if st.TopicsCarried != 1 || st.TopicsReattached != 0 {
			t.Fatalf("carried=%d reattached=%d, want 1/0 — an unrelated tombstone must not outrank a living topic",
				st.TopicsCarried, st.TopicsReattached)
		}
		if got := w3TopicOf(t, pool, scope, other[0]); got != living {
			t.Fatalf("the living topic lost its cluster to a tombstone: %s → %s", living, got)
		}
	})

	// RED PROBE for the import gate: switch the tombstone window off — the
	// same three runs then mint a fresh identity and the map's continuity
	// across the import is gone.
	t.Run("import red probe — no tombstone window ⇒ identity breaks", func(t *testing.T) {
		const scope = "impred"
		ids := w3Blocks(t, pool, scope, 11000, 6)
		filler := w3Blocks(t, pool, scope, 11100, 2)
		w3Run(t, pool, scope, 0, w3Group{members: ids}, w3Group{members: filler})
		mother := w3TopicOf(t, pool, scope, ids[0])
		w3Run(t, pool, scope, 0, w3Group{members: filler})
		st3 := w3Run(t, pool, scope, 0, w3Group{members: ids}, w3Group{members: filler})

		if st3.TopicsReattached != 0 || st3.TopicsBorn != 1 {
			t.Fatalf("probe ineffective: reattached=%d born=%d, want 0/1", st3.TopicsReattached, st3.TopicsBorn)
		}
		if got := w3TopicOf(t, pool, scope, ids[0]); got == mother {
			t.Fatal("identity survived without the tombstone check — probe ineffective")
		}
	})

	// ── G7: determinism of the assignment. Two independent generations built
	// from the same input must produce the same STRUCTURE — the topic uuids
	// are random by design and cannot be compared, the core hashes and the
	// lineage kinds are the observable contract.
	t.Run("G7 determinism of the assignment", func(t *testing.T) {
		const scope = "g7"
		ctx := context.Background()
		ids := w3Blocks(t, pool, scope, 12000, 8)
		sequence := func() string {
			w3Run(t, pool, scope, retention, w3Group{members: ids[:4]}, w3Group{members: ids[4:]})
			w3Run(t, pool, scope, retention,
				w3Group{members: ids[:3]}, w3Group{members: ids[3:6]}, w3Group{members: ids[6:]})
			var out string
			if err := pool.QueryRow(ctx, `
				SELECT COALESCE(string_agg(n.cluster_id::text || '|' || n.size::text || '|' || n.core_hash
				                           || '|' || t.origin_kind || '|' || COALESCE(o.core_hash, '-'),
				                E'\n' ORDER BY n.cluster_id), '')
				  FROM graph_cluster_node n
				  JOIN graph_cluster_topic t ON t.topic_id = n.topic_id
				  LEFT JOIN graph_cluster_node o ON o.topic_id = t.origin_topic_id
				 WHERE n.scope = $1`, scope).Scan(&out); err != nil {
				t.Fatal(err)
			}
			return out
		}
		first := sequence()

		// Wipe the identity layer and replay from a virgin state — the topic
		// uuids are gen_random_uuid() by design and cannot be compared, but
		// the STRUCTURE (which cluster, which size, which core, which lineage
		// kind) must reproduce exactly.
		for _, sql := range []string{
			`DELETE FROM graph_cluster_node WHERE scope = $1`,
			`DELETE FROM graph_cluster_member WHERE scope = $1`,
			`DELETE FROM graph_cluster_topic WHERE scope = $1`,
		} {
			if _, err := pool.Exec(ctx, sql, scope); err != nil {
				t.Fatalf("wipe: %v", err)
			}
		}
		if second := sequence(); second != first {
			t.Fatalf("same input produced different assignments:\n%s\nvs\n%s", first, second)
		}
	})
}

// ── K2-2: a member that disappears from the visibility cut BETWEEN the Louvain
// load and persist must not take the whole rebuild down. The window is the full
// Louvain runtime, so a live system loses this race regularly.
func TestW3ConcurrentArchivingDoesNotBreakThePersist(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "arch"
	const retention = 45 * 24 * time.Hour

	solo := w3Blocks(t, pool, scope, 40000, 1)
	pair := w3Blocks(t, pool, scope, 40100, 2)
	w3Run(t, pool, scope, retention, w3Group{members: solo}, w3Group{members: pair})

	// The race: the block is archived AFTER the clustering saw it. persist is
	// then handed the same partition the Louvain run produced.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, solo[0]); err != nil {
		t.Fatal(err)
	}
	st := w3Run(t, pool, scope, retention, w3Group{members: solo}, w3Group{members: pair})
	if st.Skipped {
		t.Fatalf("run skipped: %s", st.SkipReason)
	}

	var nodes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_node WHERE scope = $1`, scope).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if nodes != 1 {
		t.Fatalf("node rows = %d, want 1 (the archived solo cluster has no visible member left)", nodes)
	}
	// The surviving cluster kept its identity — the race cost one topic its
	// node row, not the whole map.
	if st.TopicsCarried != 2 {
		t.Fatalf("carried=%d, want 2 — both assignments still happened", st.TopicsCarried)
	}

	// Control: a REAL identity defect must still be a loud rollback. Forcing
	// the aggregation to drop a cluster that HAS visible members is exactly
	// the state the check exists for.
	t.Run("a cluster with visible members that produces no node row still fails loud", func(t *testing.T) {
		restore := nodeAggScopedSQL
		nodeAggScopedSQL = strings.Replace(restore,
			"JOIN ov_match mt ON mt.cluster_id = r.cluster_id AND mt.scope = r.scope",
			"JOIN ov_match mt ON mt.cluster_id = r.cluster_id AND mt.scope = r.scope AND r.size > 99", 1)
		defer func() { nodeAggScopedSQL = restore }()
		if nodeAggScopedSQL == restore {
			t.Fatal("probe did not patch the aggregation")
		}
		assign, scopes, deg := map[string]string{}, map[string]string{}, map[string]float64{}
		for _, m := range pair {
			assign[m], scopes[m], deg[m] = pair[0], scope, 1
		}
		_, err := persist(ctx, pool,
			clustering{blockToCluster: assign, intraDegree: deg},
			Options{Resolution: 1.0, VisibleTypes: w3Types, ScopeFilter: []string{scope}, TombstoneRetention: retention},
			scopes, tallyScopes(scopes))
		if err == nil {
			t.Fatal("a dropped cluster with visible members did not fail the persist")
		}
		if !strings.Contains(err.Error(), "identity join dropped a cluster") {
			t.Fatalf("wrong diagnosis: %v", err)
		}
	})
}

// ── G10: scope-move isolation (B1b). Runs the REAL Rebuild, because the leak
// path is the interaction of loadNodes/loadEdges with the matching, not the
// matching alone.
func TestW3ScopeMoveIsolation(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const retention = 45 * 24 * time.Hour

	ids := w3Blocks(t, pool, "moveA", 20000, 3)
	for i := 0; i < len(ids)-1; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			VALUES ($1::uuid, $2::uuid, 'topical', 0.9, 0.9, 'moveA')`, ids[i], ids[i+1]); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	opts := Options{Resolution: 1.0, VisibleTypes: w3Types, OverviewTypes: w3Types, TombstoneRetention: retention}
	if _, err := Rebuild(ctx, pool, opts); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	topicA := w3TopicOf(t, pool, "moveA", ids[0])
	if _, err := pool.Exec(ctx, `
		UPDATE graph_cluster_topic SET label = 'SECRET-A', label_source = 'llm', label_built_at = now()
		 WHERE topic_id = $1::uuid`, topicA); err != nil {
		t.Fatalf("label the scope-A topic: %v", err)
	}

	move := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET scope = 'moveB' WHERE id = ANY($1::uuid[])`, ids); err != nil {
			t.Fatalf("scope move: %v", err)
		}
		if _, err := Rebuild(ctx, pool, opts); err != nil {
			t.Fatalf("run 2: %v", err)
		}
	}

	t.Run("the topic does NOT follow the blocks into the new scope", func(t *testing.T) {
		move(t)
		topicB := w3TopicOf(t, pool, "moveB", ids[0])
		if topicB == topicA {
			t.Fatal("the scope-A topic followed its blocks into scope B — B1b label leak")
		}
		rowB := w3TopicRow(t, pool, topicB)
		if rowB.scope != "moveB" || rowB.originKind != "birth" {
			t.Fatalf("new topic: scope=%s kind=%s, want moveB/birth", rowB.scope, rowB.originKind)
		}
		if !w3TopicRow(t, pool, topicA).retired {
			t.Fatal("the abandoned scope-A topic must retire")
		}
		// Since W5 a fresh topic is never label-LESS — the deterministic
		// fallback names it in the same transaction it is born in. The leak
		// signal is therefore not "a label exists" but "THIS label exists":
		// the scope-B row must carry a name built from scope-B's own tags and
		// categories ('fallback'), never the 'llm' name minted over scope A.
		var label, source *string
		if err := pool.QueryRow(ctx, `
			SELECT t.label, t.label_source FROM graph_cluster_node n JOIN graph_cluster_topic t ON t.topic_id = n.topic_id
			 WHERE n.scope = 'moveB'`).Scan(&label, &source); err != nil {
			t.Fatal(err)
		}
		if label != nil && *label == "SECRET-A" {
			t.Fatal("the scope-B node row serves the scope-A label — a private label reached a foreign scope")
		}
		if source == nil || *source != "fallback" {
			t.Fatalf("scope-B label_source = %v, want fallback (a locally minted name, not an inherited one)", source)
		}
	})

	// RED PROBE: remove the topic_scope binding from ov_overlap. The scope-A
	// topic — including its 'llm' label — then rides onto the scope-B node row.
	t.Run("G10 red probe — without p.topic_scope = m.scope", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET scope = 'moveA' WHERE id = ANY($1::uuid[])`, ids); err != nil {
			t.Fatal(err)
		}
		if _, err := Rebuild(ctx, pool, opts); err != nil {
			t.Fatal(err)
		}
		leakTopic := w3TopicOf(t, pool, "moveA", ids[0])
		if _, err := pool.Exec(ctx, `
			UPDATE graph_cluster_topic SET label = 'SECRET-A2', label_source = 'llm', label_built_at = now()
			 WHERE topic_id = $1::uuid`, leakTopic); err != nil {
			t.Fatal(err)
		}

		restore := overviewGlobalSQLForProbe()
		defer restore()
		move(t)

		var nodeScope, topicScope string
		var label *string
		if err := pool.QueryRow(ctx, `
			SELECT n.scope, t.scope, t.label
			  FROM graph_cluster_node n JOIN graph_cluster_topic t ON t.topic_id = n.topic_id
			 WHERE n.scope = 'moveB'`).Scan(&nodeScope, &topicScope, &label); err != nil {
			t.Fatalf("red probe produced no scope-B node row: %v", err)
		}
		if topicScope == nodeScope {
			t.Fatal("red probe did not reproduce the leak — the gate would not prove anything")
		}
		if label == nil || *label != "SECRET-A2" {
			t.Fatalf("red probe: scope-B row carries label %v, expected the scope-A secret", label)
		}
	})
}

// tombWinsSQLForProbe reduces the tombstone precedence to the pre-K2-1
// behaviour — a tombstone only takes clusters that have no continuation
// candidate at all — and returns the restore func. Test-only.
func tombWinsSQLForProbe() func() {
	prev := tombWinsSQL
	tombWinsSQL = `
INSERT INTO ov_match (cluster_id, scope, topic_id, ov, carried)
SELECT tm.cluster_id, tm.scope, tm.topic_id, tm.ov, true
  FROM ov_tomb_match tm
  LEFT JOIN ov_carry c ON c.cluster_id = tm.cluster_id AND c.scope = tm.scope
 WHERE c.topic_id IS NULL`
	return func() { tombWinsSQL = prev }
}

// overviewGlobalSQLForProbe strips the B1b scope binding out of ov_overlap and
// returns the restore func. Test-only.
func overviewGlobalSQLForProbe() func() {
	prevG, prevS := overlapGlobalSQL, overlapScopedSQL
	overlapGlobalSQL = strings.Replace(prevG, " AND p.topic_scope = m.scope", "", 1)
	overlapScopedSQL = strings.Replace(prevS, " AND p.topic_scope = m.scope", "", 1)
	return func() { overlapGlobalSQL, overlapScopedSQL = prevG, prevS }
}

// ── K2-3: an empty node cut in front of living topics is a config anomaly, not
// an instruction to erase the partition's identity layer.
func TestW3EmptyNodeCutDoesNotRetireThePartition(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const retention = 45 * 24 * time.Hour

	ids := w3Blocks(t, pool, "empty", 50000, 3)
	for i := 0; i < len(ids)-1; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			VALUES ($1::uuid, $2::uuid, 'topical', 0.9, 0.9, 'empty')`, ids[i], ids[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{Resolution: 1.0, VisibleTypes: w3Types, OverviewTypes: w3Types, TombstoneRetention: retention}
	if _, err := Rebuild(ctx, pool, opts); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	topic := w3TopicOf(t, pool, "empty", ids[0])

	// The anomaly: BOTH allowlists are non-empty (so the loud wiring check
	// passes) but they do not intersect — the node cut collapses to zero rows.
	broken := opts
	broken.OverviewTypes = []string{"issue"}
	st, err := Rebuild(ctx, pool, broken)
	if err != nil {
		t.Fatalf("empty cut returned an error instead of a fail-safe skip: %v", err)
	}
	if !st.Skipped || st.SkipReason != "empty-node-cut" {
		t.Fatalf("empty cut: skipped=%v reason=%q, want true/empty-node-cut", st.Skipped, st.SkipReason)
	}
	var retired, nodes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_topic WHERE retired_at IS NOT NULL`).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_node`).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if retired != 0 || nodes == 0 {
		t.Fatalf("the empty cut retired %d topics and left %d node rows — want 0 retired, map intact", retired, nodes)
	}
	if got := w3TopicOf(t, pool, "empty", ids[0]); got != topic {
		t.Fatalf("identity changed across the frozen run: %s → %s", topic, got)
	}

	// The stamp has to be writable — an unstampable skip is an INVISIBLE
	// freeze, which is the state migration 123 abolished (migration 126 adds
	// the vocabulary entry).
	if err := StampAttempt(ctx, pool, nil, st.SkipReason, st.CandidateCount, time.Now()); err != nil {
		t.Fatalf("stamping the empty-cut skip: %v — chk_gom_skip_reason rejects the value", err)
	}

	// Control: a partition WITHOUT identities must still persist its honest
	// empty map instead of freezing forever.
	t.Run("a partition with no identities still persists an empty map", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM graph_cluster_node`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM graph_cluster_topic`); err != nil {
			t.Fatal(err)
		}
		st, err := Rebuild(ctx, pool, broken)
		if err != nil {
			t.Fatalf("empty cut on a virgin partition: %v", err)
		}
		if st.Skipped {
			t.Fatalf("a partition without identities must not freeze: reason=%q", st.SkipReason)
		}
	})
}

// ── G12 (Amendment A01-5): a frozen map keeps its identities. The node cap is
// the only freeze reason that exists today; the TODO in Rebuild names the two
// that Achse 04 adds.
func TestW3NodeCapFreezeKeepsIdentities(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const retention = 45 * 24 * time.Hour

	ids := w3Blocks(t, pool, "freeze", 30000, 4)
	for i := 0; i < len(ids)-1; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			VALUES ($1::uuid, $2::uuid, 'topical', 0.9, 0.9, 'freeze')`, ids[i], ids[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{Resolution: 1.0, VisibleTypes: w3Types, OverviewTypes: w3Types, TombstoneRetention: retention}
	if _, err := Rebuild(ctx, pool, opts); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	snapshot := func() string {
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(string_agg(t.topic_id::text || '|' || t.last_seen_at::text || '|' ||
			                COALESCE(t.retired_at::text, '-') || '|' || n.core_hash, E'\n' ORDER BY t.topic_id), '')
			  FROM graph_cluster_topic t JOIN graph_cluster_node n ON n.topic_id = t.topic_id`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	if before == "" {
		t.Fatal("fixture: no identities after run 1")
	}

	capped := opts
	capped.MaxNodes = 2 // below the 4 candidates
	st, err := Rebuild(ctx, pool, capped)
	if err != nil {
		t.Fatalf("capped rebuild returned an error instead of a fail-safe skip: %v", err)
	}
	if !st.Skipped || st.SkipReason != "node-cap" {
		t.Fatalf("capped rebuild: skipped=%v reason=%q, want true/node-cap", st.Skipped, st.SkipReason)
	}
	if after := snapshot(); after != before {
		t.Fatalf("the node-cap freeze touched the identity layer:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	var retired int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_topic WHERE retired_at IS NOT NULL`).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("the freeze retired %d topics — a liveness guard must never shred identities", retired)
	}
}
