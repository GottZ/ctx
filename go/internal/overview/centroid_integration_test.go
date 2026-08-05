//go:build integration

// Wave C8 (Cluster-Topic-Map, design/03 §3.2/§4.6 + §7 "C8") — the BUILD half.
//
// Gates covered here:
//
//	(iii) teardown congruence: a scoped run touches no foreign partition and
//	      creates no duplicate;
//	(v)   decoupling from the rebuild budget: an over-budget centroid pass aborts
//	      ONLY itself — members, nodes and graph_overview_meta stay fresh;
//	K7    the incremental diff: an unchanged partition is not recomputed, a
//	      changed one is, and a pure minUUID rename costs one column;
//	      plus the orphan sweep and the ANN threshold.
//
// The cold-start fallback (i), the scope filter (ii) and the one-centroid-per-
// topic guard (vi) are read-path gates and live in internal/rrf.
//
//	go test -tags=integration ./internal/overview/ -run TestCentroid -count=1 -v
package overview

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

func c8Types() []string { return []string{"knowledge", "reference", "audit-trail"} }

func c8Opts() CentroidOptions {
	return CentroidOptions{Batch: 500, WorkMem: "64MB", ANNThreshold: 0, VisibleTypes: c8Types()}
}

func c8BlockID(i int) string   { return fmt.Sprintf("019d9000-0000-7000-9000-%012x", i) }
func c8ClusterID(i int) string { return fmt.Sprintf("019da000-0000-7000-9000-%012x", i) }
func c8TopicID(i int) string   { return fmt.Sprintf("0190bbbb-0000-4000-8000-%012x", i) }

// c8Block writes a block with a ONE-HOT embedding. One-hot vectors make the
// arithmetic checkable by hand: the average of two distinct one-hots is a known
// vector, and the cosine against a third is exactly 0.
func c8Block(t *testing.T, pool *pgxpool.Pool, id, scope string, dim int, embedded bool) {
	t.Helper()
	ctx := context.Background()
	var emb any
	if embedded {
		var v string
		if err := pool.QueryRow(ctx,
			`SELECT (SELECT array_agg(CASE WHEN i = $1 THEN 1.0 ELSE 0.0 END ORDER BY i)
			           FROM generate_series(1, 1024) i)::real[]::vector::text`, dim).Scan(&v); err != nil {
			t.Fatalf("build embedding: %v", err)
		}
		emb = v
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, embedding)
		 VALUES ($1::uuid, 'learnings', $2, 'c8 fixture', $3, $4::vector)`,
		id, "c8-"+id, scope, emb); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

// c8Partition wires one (topic, scope, cluster) partition with the given member
// blocks — the exact shape a persisted rebuild leaves behind.
func c8Partition(t *testing.T, pool *pgxpool.Pool, topicID, clusterID, scope string, blockIDs []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_topic (topic_id, scope) VALUES ($1::uuid, $2)
		 ON CONFLICT (topic_id) DO NOTHING`, topicID, scope); err != nil {
		t.Fatalf("insert topic: %v", err)
	}
	for _, b := range blockIDs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
			 VALUES ($1::uuid, $2::uuid, $3)
			 ON CONFLICT (block_id) DO UPDATE SET cluster_id = EXCLUDED.cluster_id, scope = EXCLUDED.scope`,
			b, clusterID, scope); err != nil {
			t.Fatalf("insert member: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality,
		                                 category_counts, topic_id)
		 VALUES ($1::uuid, $2, $3, $4::uuid, 'repr', 1, '{"learnings":1}'::jsonb, $5::uuid)
		 ON CONFLICT (cluster_id, scope) DO UPDATE SET size = EXCLUDED.size, topic_id = EXCLUDED.topic_id`,
		clusterID, scope, len(blockIDs), blockIDs[0], topicID); err != nil {
		t.Fatalf("insert node: %v", err)
	}
}

func c8CentroidRow(t *testing.T, pool *pgxpool.Pool, topicID string) (clusterID string, memberN, embeddedN int, computedAt time.Time, hash []byte) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT cluster_id::text, member_n, embedded_n, computed_at, member_hash
		   FROM graph_cluster_centroid WHERE topic_id = $1::uuid`, topicID,
	).Scan(&clusterID, &memberN, &embeddedN, &computedAt, &hash); err != nil {
		t.Fatalf("read centroid %s: %v", topicID, err)
	}
	return
}

// K7 — THE INCREMENTAL DIFF, the reason the table is keyed on the stable
// identity at all. A second pass over an UNCHANGED corpus must find nothing to
// do: at 10M members a full pass is ~6,9 GB of embedding I/O, every six hours,
// forever.
//
// ROT-PROBE: drop the `c.member_hash IS DISTINCT FROM cur.member_hash` arm of the
// work-list query (i.e. make every partition dirty) ⇒ the second pass reports
// Recomputed = 2 instead of 0 and computed_at moves — the full-rebuild pathology,
// visible as a failing assert instead of as a bill.
func TestCentroidIncrementalDiff(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	c8Block(t, pool, c8BlockID(1), "private", 1, true)
	c8Block(t, pool, c8BlockID(2), "private", 2, true)
	c8Block(t, pool, c8BlockID(3), "private", 3, true)
	c8Partition(t, pool, c8TopicID(1), c8ClusterID(1), "private", []string{c8BlockID(1), c8BlockID(2)})
	c8Partition(t, pool, c8TopicID(2), c8ClusterID(2), "private", []string{c8BlockID(3)})

	first, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Recomputed != 2 || first.Dirty != 2 {
		t.Fatalf("first pass = %+v, want 2 dirty / 2 recomputed (cold start)", first)
	}
	_, memberN, embeddedN, at1, hash1 := c8CentroidRow(t, pool, c8TopicID(1))
	if memberN != 2 || embeddedN != 2 {
		t.Errorf("member_n/embedded_n = %d/%d, want 2/2", memberN, embeddedN)
	}

	second, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Dirty != 0 || second.Recomputed != 0 || second.Batches != 0 {
		t.Fatalf("second pass over an unchanged corpus = %+v, want all zero (K7)", second)
	}
	_, _, _, at2, hash2 := c8CentroidRow(t, pool, c8TopicID(1))
	if !at1.Equal(at2) {
		t.Errorf("computed_at moved on a no-op pass: %v → %v", at1, at2)
	}
	if string(hash1) != string(hash2) {
		t.Error("member_hash changed without a membership change")
	}

	// A member joins ⇒ exactly THAT partition is dirty, its sibling is not.
	c8Block(t, pool, c8BlockID(4), "private", 4, true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_member (block_id, cluster_id, scope) VALUES ($1::uuid, $2::uuid, 'private')`,
		c8BlockID(4), c8ClusterID(1)); err != nil {
		t.Fatalf("join member: %v", err)
	}
	third, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts())
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if third.Dirty != 1 || third.Recomputed != 1 {
		t.Fatalf("third pass = %+v, want exactly ONE dirty partition", third)
	}
	if _, memberN, _, _, _ = c8CentroidRow(t, pool, c8TopicID(1)); memberN != 3 {
		t.Errorf("member_n = %d after the join, want 3", memberN)
	}
}

// The diff carrier covers EMBEDDING COVERAGE, not only membership. A block whose
// embedding is backfilled later changes the centroid without changing the member
// set — a hash blind to that would skip the recompute forever and serve an
// average that silently ignores a member.
//
// ROT-PROBE: drop the `CASE WHEN b.embedding IS NULL` marker from memberSetExpr
// ⇒ the pass after the backfill reports Dirty 0 and embedded_n stays 1.
func TestCentroidDiffSeesEmbeddingBackfill(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	c8Block(t, pool, c8BlockID(11), "private", 11, true)
	c8Block(t, pool, c8BlockID(12), "private", 12, false) // no embedding yet
	c8Partition(t, pool, c8TopicID(11), c8ClusterID(11), "private", []string{c8BlockID(11), c8BlockID(12)})

	if _, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	_, memberN, embeddedN, _, _ := c8CentroidRow(t, pool, c8TopicID(11))
	if memberN != 2 || embeddedN != 1 {
		t.Fatalf("member_n/embedded_n = %d/%d, want 2/1 — embedded_n is the honesty counter", memberN, embeddedN)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET embedding = (
		     SELECT array_agg(CASE WHEN i = 12 THEN 1.0 ELSE 0.0 END ORDER BY i)
		       FROM generate_series(1,1024) i)::real[]::vector
		  WHERE id = $1::uuid`, c8BlockID(12)); err != nil {
		t.Fatalf("backfill embedding: %v", err)
	}
	st, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if st.Recomputed != 1 {
		t.Fatalf("pass after an embedding backfill = %+v, want 1 recomputed", st)
	}
	if _, _, embeddedN, _, _ = c8CentroidRow(t, pool, c8TopicID(11)); embeddedN != 2 {
		t.Errorf("embedded_n = %d after backfill, want 2", embeddedN)
	}
}

// K13 — THE minUUID RENAME IS NOT A RECOMPUTE. A newcomer with a smaller uuid
// renames a whole community without a single member moving. cluster_id is a
// run-local USE column here, so the case costs one UPDATE, not an avg() over
// unchanged embeddings.
//
// ROT-PROBE: drop the `hash_match` split and treat every dirty row as a
// recompute ⇒ Renamed 0 / Recomputed 1, and computed_at moves for a partition
// whose centroid is bit-identical.
func TestCentroidRenameIsNotRecompute(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	c8Block(t, pool, c8BlockID(21), "private", 21, true)
	c8Partition(t, pool, c8TopicID(21), c8ClusterID(21), "private", []string{c8BlockID(21)})
	if _, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	_, _, _, at1, hash1 := c8CentroidRow(t, pool, c8TopicID(21))

	// Same members, new run-local cluster id — exactly the minUUID rename.
	if _, err := pool.Exec(ctx,
		`UPDATE graph_cluster_member SET cluster_id = $1::uuid WHERE cluster_id = $2::uuid`,
		c8ClusterID(22), c8ClusterID(21)); err != nil {
		t.Fatalf("rename members: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE graph_cluster_node SET cluster_id = $1::uuid WHERE cluster_id = $2::uuid`,
		c8ClusterID(22), c8ClusterID(21)); err != nil {
		t.Fatalf("rename node: %v", err)
	}

	st, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts())
	if err != nil {
		t.Fatalf("rename pass: %v", err)
	}
	if st.Renamed != 1 || st.Recomputed != 0 || st.Batches != 0 {
		t.Fatalf("rename pass = %+v, want 1 renamed / 0 recomputed / 0 batches (K13)", st)
	}
	cid, _, _, at2, hash2 := c8CentroidRow(t, pool, c8TopicID(21))
	if cid != c8ClusterID(22) {
		t.Errorf("cluster_id = %s, want the renamed %s", cid, c8ClusterID(22))
	}
	if !at1.Equal(at2) {
		t.Error("computed_at moved on a pure rename — nothing was computed")
	}
	if string(hash1) != string(hash2) {
		t.Error("member_hash changed on a pure rename")
	}
}

// Gate (iii) — TEARDOWN CONGRUENCE. A scoped run must not delete a foreign
// partition's centroid and must not create a duplicate. The removal lives in the
// arm's own transaction (an anti-join sweep) rather than in the persist teardown,
// because a delete-all there would erase the very rows the K7 diff compares
// against — but the scope congruence rule is the same one B1-C1 states.
//
// ROT-PROBE: drop `AND c.scope = ANY($1)` from sweepOrphanCentroids ⇒ the
// private run deletes the work partition's centroid and the assert below fails,
// which is the cross-partition data loss B1-C1 forbids.
func TestCentroidScopedSweepSparesForeignPartition(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	c8Block(t, pool, c8BlockID(31), "private", 31, true)
	c8Block(t, pool, c8BlockID(32), "work", 32, true)
	c8Partition(t, pool, c8TopicID(31), c8ClusterID(31), "private", []string{c8BlockID(31)})
	c8Partition(t, pool, c8TopicID(32), c8ClusterID(32), "work", []string{c8BlockID(32)})

	if _, err := BuildCentroids(ctx, pool, nil, c8Opts()); err != nil {
		t.Fatalf("global pass: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_centroid`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("global pass wrote %d centroids, want 2", n)
	}

	// The private partition disappears (its node row is torn down); a scoped
	// private run must sweep exactly that one.
	if _, err := pool.Exec(ctx, `DELETE FROM graph_cluster_node WHERE scope = 'private'`); err != nil {
		t.Fatalf("teardown private node: %v", err)
	}
	st, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts())
	if err != nil {
		t.Fatalf("scoped pass: %v", err)
	}
	if st.Swept != 1 {
		t.Errorf("swept = %d, want 1", st.Swept)
	}
	var scopes []string
	if err := pool.QueryRow(ctx,
		`SELECT array_agg(scope ORDER BY scope) FROM graph_cluster_centroid`).Scan(&scopes); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0] != "work" {
		t.Errorf("surviving centroid scopes = %v, want [work] — a scoped run must not touch a foreign partition", scopes)
	}

	// And no duplicate: repeating the global pass upserts, never re-inserts.
	if _, err := BuildCentroids(ctx, pool, nil, c8Opts()); err != nil {
		t.Fatalf("repeat global pass: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_centroid`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("centroid rows after a repeat pass = %d, want 1 (upsert, not insert)", n)
	}
}

// Gate (v) — DECOUPLING FROM THE REBUILD BUDGET. An over-budget centroid pass
// aborts ONLY itself. The persisted map — members, nodes, meta — is untouched and
// stays exactly as fresh as the rebuild left it.
//
// The budget is simulated the way it bites in production: an already-expired
// context, i.e. the state a centroid_timeout leaves behind mid-pass.
//
// ROT-PROBE: move the step back into the persist transaction ⇒ the abort takes
// the whole rebuild with it and graph_cluster_member comes back EMPTY, which is
// what this assert catches.
func TestCentroidTimeoutDoesNotTakeTheRebuild(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	c8Block(t, pool, c8BlockID(41), "private", 41, true)
	c8Partition(t, pool, c8TopicID(41), c8ClusterID(41), "private", []string{c8BlockID(41)})
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_overview_meta (scope, computed_at, last_attempt_at, node_n, edge_n, cluster_n, modularity, candidate_n)
		VALUES ('private', now(), now(), 1, 0, 1, 0, 1)`); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	dead, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	if _, err := BuildCentroids(dead, pool, []string{"private"}, c8Opts()); err == nil {
		t.Fatal("an expired budget must surface as an error, not as a silent no-op")
	}

	var members, nodes int
	var computedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_member`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_node`).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT computed_at FROM graph_overview_meta WHERE scope='private'`).Scan(&computedAt); err != nil {
		t.Fatal(err)
	}
	if members != 1 || nodes != 1 {
		t.Errorf("members/nodes = %d/%d after a failed centroid pass, want 1/1 — the rebuild must survive", members, nodes)
	}
	if computedAt == nil {
		t.Error("graph_overview_meta.computed_at was cleared by a centroid failure")
	}
}

// The W8 retention purge must not leave centroid corpses behind: a purged
// tombstone takes its centroid with it, through the FK, without the arm having
// to know about retention at all.
func TestCentroidFollowsTopicRetention(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	c8Block(t, pool, c8BlockID(51), "private", 51, true)
	c8Partition(t, pool, c8TopicID(51), c8ClusterID(51), "private", []string{c8BlockID(51)})
	if _, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts()); err != nil {
		t.Fatalf("build: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM graph_cluster_node WHERE scope='private'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE graph_cluster_topic SET retired_at = now() - interval '90 days' WHERE topic_id = $1::uuid`,
		c8TopicID(51)); err != nil {
		t.Fatal(err)
	}
	if _, err := PurgeTombstones(ctx, pool, []string{"private"}, 24*time.Hour); err != nil {
		t.Fatalf("purge: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_centroid`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("centroid rows after the tombstone purge = %d, want 0 (FK cascade)", n)
	}
}

// A partition WITHOUT a single embedded member gets NO row — never a zero
// vector. A zero vector is cosine-neutral: it would neither win nor lose, i.e.
// noise wearing the face of a signal, and it would make embedded_n a lie.
func TestCentroidSkipsUnembeddedPartition(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	c8Block(t, pool, c8BlockID(61), "private", 0, false)
	c8Partition(t, pool, c8TopicID(61), c8ClusterID(61), "private", []string{c8BlockID(61)})

	st, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if st.Dirty != 0 || st.Recomputed != 0 {
		t.Errorf("pass = %+v, want nothing to do for an unembedded partition", st)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_centroid`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("centroid rows = %d, want 0", n)
	}
}

// UD-02-03 — the ANN index is a declared RESOURCE limit, applied by the arm and
// not by the migration: below the threshold the read path is an exact scan.
// Hysteresis keeps a corpus sitting on the boundary from paying an index build
// every cycle.
func TestCentroidANNThreshold(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		c8Block(t, pool, c8BlockID(70+i), "private", 70+i, true)
		c8Partition(t, pool, c8TopicID(70+i), c8ClusterID(70+i), "private", []string{c8BlockID(70 + i)})
	}
	indexed := func() bool {
		var ok bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes
			                 WHERE tablename='graph_cluster_centroid' AND indexname=$1)`,
			centroidHNSWIndex).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	opts := c8Opts()
	opts.ANNThreshold = 0 // off — the shipped default
	if st, err := BuildCentroids(ctx, pool, []string{"private"}, opts); err != nil {
		t.Fatalf("build: %v", err)
	} else if st.IndexState != "absent" {
		t.Errorf("index state = %q with the threshold off, want absent", st.IndexState)
	}
	if indexed() {
		t.Fatal("threshold off must leave the exact scan in place")
	}

	opts.ANNThreshold = 2 // 4 rows > 2 ⇒ build
	if st, err := BuildCentroids(ctx, pool, []string{"private"}, opts); err != nil {
		t.Fatalf("build over threshold: %v", err)
	} else if st.IndexState != "created" {
		t.Errorf("index state = %q over the threshold, want created", st.IndexState)
	}
	if !indexed() {
		t.Fatal("crossing the threshold must build the index")
	}

	// Still over the threshold ⇒ present, NOT rebuilt: an index rebuilt every
	// cycle is exactly the maintenance churn the exact-scan default avoids.
	if st, err := BuildCentroids(ctx, pool, []string{"private"}, opts); err != nil {
		t.Fatalf("repeat: %v", err)
	} else if st.IndexState != "present" {
		t.Errorf("index state = %q on a repeat pass, want present", st.IndexState)
	}

	// Hysteresis: 4 rows against a threshold of 6 is still above 6/2 ⇒ keep.
	// Without it a corpus sitting on the boundary would pay an index build every
	// six hours, forever.
	opts.ANNThreshold = 6
	if st, err := BuildCentroids(ctx, pool, []string{"private"}, opts); err != nil {
		t.Fatalf("hysteresis pass: %v", err)
	} else if st.IndexState != "present" {
		t.Errorf("index state = %q just under the threshold, want present (hysteresis)", st.IndexState)
	}
	opts.ANNThreshold = 100 // 4 <= 50 ⇒ drop
	if st, err := BuildCentroids(ctx, pool, []string{"private"}, opts); err != nil {
		t.Fatalf("drop pass: %v", err)
	} else if st.IndexState != "dropped" {
		t.Errorf("index state = %q well under the threshold, want dropped", st.IndexState)
	}
}

// The SET LOCAL whitelist. work_mem is a GUC name and its value a memory
// literal — neither can be a bind parameter, so the value HAS to be
// interpolated, and the whitelist IS the injection barrier the C0 key comment
// demands. Fail-closed: a bad literal is an error, never a silently unset knob.
func TestCentroidWorkMemWhitelist(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, bad := range []string{"256MB'; DROP TABLE context_blocks; --", "256 MB", "lots", "0MB", "256mb"} {
		opts := c8Opts()
		opts.WorkMem = bad
		if _, err := BuildCentroids(ctx, pool, nil, opts); err == nil {
			t.Errorf("work_mem %q was accepted — the whitelist is the injection barrier", bad)
		}
	}
	for _, good := range []string{"64MB", "4GB", "512kB"} {
		opts := c8Opts()
		opts.WorkMem = good
		if _, err := BuildCentroids(ctx, pool, nil, opts); err != nil {
			t.Errorf("work_mem %q rejected: %v", good, err)
		}
	}
}
