// Topic identity across Louvain rebuilds — wave W3 of the Cluster-Topic-Map
// (design/01 §4.2–§4.4, Amendments A01-2 / A01-6 / A01-7).
//
// The 057 family replaces graph_cluster_member/_node/_edge on EVERY rebuild
// (teardown in cluster.go), and cluster_id is the smallest member uuid of the
// community — it turns over silently the moment that one block leaves. This
// file is the layer that survives: graph_cluster_topic carries the identity a
// map edge and a retrieval signal can point at, and the code below decides per
// run which of last generation's topics continues in which of this
// generation's clusters.
//
// THE THREE RULES, all three the same rule ("half of the substance"):
//
//	Fortführung  (A01-6/E1-01): mutual plurality AND ov*2 >= size_prev — the
//	             new cluster holds at least half of the OLD topic. No absolute
//	             threshold, no tuning key: an absolute 2 would let a stable
//	             singleton (1/1) churn its identity every run while a 300-block
//	             cluster inherits a topic off two bridge blocks (2/300).
//	Substanz-Kern (A01-7/E4-01): the smallest member set, by intra-cluster
//	             degree descending with a uuid tiebreak, whose cumulated degree
//	             carries at least half of the cluster's internal substance. No
//	             K constant: organic knowledge is heavy-tailed (live median 6,
//	             max 133), so a fixed K is either noise or a truncation.
//	Re-Attach    (A01-2/E2-01): a birth candidate that covers at least half of
//	             a tombstone's core within tombstone_retention revives that
//	             identity instead of minting one. This is the batch-import
//	             half of the mechanism; organic growth runs over the living
//	             predecessor generation, both in the SAME run.
//
// SCOPE BINDING IS NOT OPTIONAL (B1b, design/01 §5.3). Every matching query
// carries a scope predicate, and the predicate binds the scope of the TOPIC to
// the scope of the member row — not just the run's partition filter. Without
// it a group of linked blocks moving from scope A to scope B (the
// sweepScopeMoveLinks path, store/blocks.go) hands its scope-A topic — and
// later its scope-A 'llm' label — to a scope-B node row, where every reader of
// B sees it. A topic NEVER crosses a scope boundary: the move is a birth in B
// plus a death in A.
//
// INJECTIVITY has three lines of defence, in this order: the mutual-plurality
// construction itself (each cluster has at most one argmax topic and vice
// versa), the UNIQUE index on ov_match(topic_id) which breaks with 23505 at
// the cause, and uq_gcn_scope_topic (migration 124) which covers any path that
// bypasses ov_match. All three roll the persist tx back and leave the previous
// map readable.
//
// Everything here runs INSIDE the existing advisory-locked persist tx and on
// TEMP tables (ON COMMIT DROP, so they live in temp_buffers/pgsql_tmp, not on
// the Go heap). No Louvain and no resolution search happens in here (K5).
package overview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// coreBatch caps the per-INSERT unnest array size of the ov_core fill, like
// memberBatch does for the member insert. The row count is the CLUSTER count,
// not the member count, so this batch is reached far later — it exists so the
// bind parameter cannot grow unbounded at the node cap either.
const coreBatch = 5000

// coreOf returns the SUBSTANZ-KERN of one (cluster, scope) group and its hash
// (Amendment A01-7 / decision E4-01).
//
// The core is the smallest prefix of the members — ranked by intra-cluster
// weighted degree descending, uuid ascending — whose cumulated degree carries
// at least half of the group's total internal substance. It is self-adaptive:
// a hub cluster whose top three blocks hold most of the internal weight gets a
// core of three, a flat cluster of equals gets a core of half its members.
// That is the property a K constant cannot have, and the reason the decision
// record retired topic_core_size ("organic knowledge hat eigentlich immer
// faktoren die wie golden ratio und pareto aussehen … wieder eine harte
// kante").
//
// Two details are load-bearing:
//
//   - The total is summed in the SAME ranked order the prefix walks, so the
//     final partial sum is bit-identical to the total and the loop is
//     guaranteed to terminate at or before the last member. Float addition is
//     not associative; summing in map order would drift the last ULP and could
//     move the cut by one member between two otherwise identical runs.
//   - The HASH is taken over the core sorted by ID, not by degree. A pure
//     re-ranking inside an unchanged core (two members swap places) must NOT
//     change core_hash — otherwise every rebuild would re-label topics that
//     never moved.
//
// A group with zero internal substance (a singleton, or an edge-free group)
// would satisfy "at least half of zero" with the empty set. It yields the
// single highest-ranked member instead: an empty core would leave the topic
// without label substance AND without tombstone substance, which is the one
// outcome the core exists to prevent.
func coreOf(members []string, deg map[string]float64) (ids []string, hash string) {
	if len(members) == 0 {
		return nil, ""
	}
	ranked := make([]string, len(members))
	copy(ranked, members)
	sort.Slice(ranked, func(i, j int) bool {
		di, dj := deg[ranked[i]], deg[ranked[j]]
		if di != dj {
			return di > dj
		}
		return ranked[i] < ranked[j]
	})

	var total float64
	for _, m := range ranked {
		total += deg[m]
	}
	cut, cum := len(ranked), 0.0
	for i, m := range ranked {
		cum += deg[m]
		if cum*2 >= total {
			cut = i + 1
			break
		}
	}

	ids = make([]string, cut)
	copy(ids, ranked[:cut])
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return ids, hex.EncodeToString(sum[:])
}

// coreArrays is the ov_core payload as four parallel arrays — the same
// unnest-bind shape the member insert uses.
type coreArrays struct {
	clusters []string
	scopes   []string
	hashes   []string
	blocks   []string // PostgreSQL array literals, cast to uuid[] in SQL
}

func (c coreArrays) len() int { return len(c.clusters) }

// buildCores derives the substance core of every (cluster, scope) group of
// this run. It lives in persist and not in computeClustering because the core
// is per (cluster, SCOPE) (design/01 §3.2) and the per-block scope is a
// persist parameter — keeping computeClustering DB- and scope-free is what
// makes it unit-testable.
//
// Benannte Restunschärfe (design/01 §4.4): intraDegree itself is
// community-pure, not scope-pure — computeClustering deliberately knows no
// scopes. In a scope-crossing cluster (structurally possible, live not
// instantiated) the foreign half's edges influence the RANKING of the own
// half. The core MEMBERSHIP stays single-scope because the grouping below is
// keyed by scope, so no foreign block ever lands in core_blocks and no foreign
// title ever reaches a prompt (invariant I2 holds); what a foreign scope can
// move is which of the OWN blocks make the cut.
func buildCores(cl clustering, nodeScopes map[string]string) coreArrays {
	type group struct{ cluster, scope string }
	groups := make(map[group][]string, len(cl.blockToCluster))
	for block, cluster := range cl.blockToCluster {
		g := group{cluster, nodeScopes[block]}
		groups[g] = append(groups[g], block)
	}
	keys := make([]group, 0, len(groups))
	for g := range groups {
		keys = append(keys, g)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].cluster != keys[j].cluster {
			return keys[i].cluster < keys[j].cluster
		}
		return keys[i].scope < keys[j].scope
	})

	out := coreArrays{
		clusters: make([]string, 0, len(keys)),
		scopes:   make([]string, 0, len(keys)),
		hashes:   make([]string, 0, len(keys)),
		blocks:   make([]string, 0, len(keys)),
	}
	for _, g := range keys {
		ids, hash := coreOf(groups[g], cl.intraDegree)
		out.clusters = append(out.clusters, g.cluster)
		out.scopes = append(out.scopes, g.scope)
		out.hashes = append(out.hashes, hash)
		// uuids need no quoting inside a PostgreSQL array literal.
		out.blocks = append(out.blocks, "{"+strings.Join(ids, ",")+"}")
	}
	return out
}

// topicStats are the lifecycle counters of one identity phase. They ride into
// overview.Stats and thereby across the worker IPC boundary — without them a
// churn problem in the identity layer is invisible from outside the child
// process, and the identity layer is precisely the thing whose silent turnover
// this wave exists to end.
type topicStats struct {
	carried    int // continued from the living predecessor generation
	reattached int // revived from a tombstone (A01-2)
	born       int // fresh identity, origin_kind='birth'
	split      int // fresh identity, origin_kind='split' (of the born ones)
	retired    int // died this run (merged or plain death)

	// membersChanged/membersReassigned are the K13 measurement (masterplan
	// §2): a newcomer with a smaller uuid renames a whole community, so a
	// delta-persist would rewrite every member row of a community that did not
	// move. membersReassigned counts exactly that class — the member changed
	// cluster_id while its TOPIC stayed the same. Pure measurement, no
	// behaviour attached; the decision whether the topic identity should
	// replace cluster_id as the delta key belongs to Achse 04 (S9b).
	membersChanged    int
	membersReassigned int
}

// topicPhase carries the run-shape of the identity phase: whether this is a
// partition run and, if so, its filter, plus the tombstone window.
type topicPhase struct {
	scoped      bool
	scopeFilter []string
	tombstone   time.Duration
}

// exec runs the global or the scoped variant of a statement. The scope filter
// is always the LAST bind parameter of the scoped variant, so the two
// templates share the numbering of every other parameter.
func (p topicPhase) exec(ctx context.Context, tx pgx.Tx, label, globalSQL, scopedSQL string, args ...any) (int64, error) {
	sql := globalSQL
	if p.scoped {
		sql = scopedSQL
		args = append(args, p.scopeFilter)
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("topic identity: %s: %w", label, err)
	}
	return tag.RowsAffected(), nil
}

// execPlain runs a statement that needs no scope predicate — either because it
// only reads temp tables that are already partition-cut, or because it is DDL.
func execPlain(ctx context.Context, tx pgx.Tx, label, sql string, args ...any) (int64, error) {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("topic identity: %s: %w", label, err)
	}
	return tag.RowsAffected(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase 0 — the predecessor snapshot, BEFORE the teardown.

// createPrevSQL and the snapshot below MUST run before teardown:
// graph_cluster_member is the only record of the old assignment and the
// teardown deletes it.
const createPrevSQL = `
CREATE TEMP TABLE ov_prev (
    block_id        UUID PRIMARY KEY,
    topic_id        UUID NOT NULL,
    topic_scope     TEXT NOT NULL,
    prev_cluster_id UUID NOT NULL
) ON COMMIT DROP`

// topic_scope is the scope of the TOPIC, not of the member row. They are equal
// today because both come from the same run — they diverge exactly when blocks
// move scope BETWEEN two runs, which is the B1b leak path, and carrying the
// column is what lets ov_overlap bind them (design/01 §4.2).
//
// n.topic_id IS NOT NULL catches exactly one situation: the first run after
// migration 124, where no assignment exists yet. ov_prev is then empty and
// every cluster is a birth — the documented non-backfill.
const prevSnapshotTemplate = `
INSERT INTO ov_prev (block_id, topic_id, topic_scope, prev_cluster_id)
SELECT m.block_id, n.topic_id, t.scope, m.cluster_id
  FROM graph_cluster_member m
  JOIN graph_cluster_node  n ON n.cluster_id = m.cluster_id AND n.scope = m.scope
  JOIN graph_cluster_topic t ON t.topic_id   = n.topic_id
 WHERE n.topic_id IS NOT NULL%s`

var (
	prevSnapshotGlobalSQL = fmt.Sprintf(prevSnapshotTemplate, "")
	prevSnapshotScopedSQL = fmt.Sprintf(prevSnapshotTemplate, "\n   AND m.scope = ANY($1)")
)

// snapshotPrevTopics materializes last generation's block→topic assignment.
// Called from persist BEFORE teardown, inside the same transaction.
func (p topicPhase) snapshotPrevTopics(ctx context.Context, tx pgx.Tx) error {
	if _, err := execPlain(ctx, tx, "create ov_prev", createPrevSQL); err != nil {
		return err
	}
	_, err := p.exec(ctx, tx, "predecessor snapshot", prevSnapshotGlobalSQL, prevSnapshotScopedSQL)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase 1 — temps.

// topicTempDDL creates the working set of the identity phase. Every table is
// ON COMMIT DROP, so a rollback (including the SIGKILL-mid-persist case the
// persistkill gate pins) leaves nothing behind.
//
// ov_best_cluster/ov_best_topic are TABLES, not CTEs: the Revision-1 design
// had them as WITH clauses of the ov_match insert and referenced them from two
// further statements — a CTE lives exactly one statement, so both follow-ups
// would have broken with 42P01 and the core mechanism was not executable.
//
// Their primary keys are not accelerators (DISTINCT ON sorts the input
// anyway); they are the join keys of the follow-up statements AND the
// declaration of injectivity in the structure itself: ov_best_topic CANNOT
// hold a topic twice.
var topicTempDDL = []string{
	`CREATE TEMP TABLE ov_prev_size (
	     topic_id UUID PRIMARY KEY, size_prev INT NOT NULL) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_overlap (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, topic_id UUID NOT NULL,
	     ov INT NOT NULL) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_best_cluster (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, topic_id UUID NOT NULL, ov INT NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_best_topic (
	     topic_id UUID PRIMARY KEY, cluster_id UUID NOT NULL, scope TEXT NOT NULL,
	     ov INT NOT NULL) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_match (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, topic_id UUID NOT NULL,
	     ov INT NOT NULL, carried BOOL NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
	// First line of defence against B2 (identity fusion): it breaks with 23505
	// at the CAUSE instead of carrying the defect down to uq_gcn_scope_topic.
	`CREATE UNIQUE INDEX ov_match_topic_uq ON ov_match (topic_id)`,
	`CREATE TEMP TABLE ov_unmatched (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, size_new INT NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_tomb (
	     topic_id UUID NOT NULL, scope TEXT NOT NULL, block_id UUID NOT NULL,
	     core_n INT NOT NULL) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_tomb_overlap (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, topic_id UUID NOT NULL,
	     ov INT NOT NULL, core_n INT NOT NULL) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_tomb_best_cluster (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, topic_id UUID NOT NULL,
	     ov INT NOT NULL, core_n INT NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_tomb_best_topic (
	     topic_id UUID PRIMARY KEY, cluster_id UUID NOT NULL, scope TEXT NOT NULL,
	     ov INT NOT NULL) ON COMMIT DROP`,
	`CREATE TEMP TABLE ov_core (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, core_hash TEXT NOT NULL,
	     core_blocks UUID[] NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
}

func createTopicTemps(ctx context.Context, tx pgx.Tx) error {
	for _, ddl := range topicTempDDL {
		if _, err := execPlain(ctx, tx, "create temps", ddl); err != nil {
			return err
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase 2 — overlap and the continuation match.

const prevSizeSQL = `
INSERT INTO ov_prev_size (topic_id, size_prev)
SELECT topic_id, count(*)::int FROM ov_prev GROUP BY topic_id`

// p.topic_scope = m.scope is THE B1b predicate (design/01 §5.3): it binds the
// topic's scope to the member's scope, so a community that moved from scope A
// to scope B produces no overlap at all and is correctly reborn in B.
const overlapTemplate = `
INSERT INTO ov_overlap (cluster_id, scope, topic_id, ov)
SELECT m.cluster_id, m.scope, p.topic_id, count(*)::int
  FROM graph_cluster_member m
  JOIN ov_prev p ON p.block_id = m.block_id AND p.topic_scope = m.scope%s
 GROUP BY 1, 2, 3`

// Both ORDER BY chains end on a uuid column and are therefore TOTAL: a tie is
// decided by the smaller uuid, never by chance. Same tiebreak discipline as
// the representative choice in nodeAggTemplate.
const bestClusterSQL = `
INSERT INTO ov_best_cluster (cluster_id, scope, topic_id, ov)
SELECT DISTINCT ON (cluster_id, scope) cluster_id, scope, topic_id, ov
  FROM ov_overlap
 ORDER BY cluster_id, scope, ov DESC, topic_id`

const bestTopicSQL = `
INSERT INTO ov_best_topic (topic_id, cluster_id, scope, ov)
SELECT DISTINCT ON (topic_id) topic_id, cluster_id, scope, ov
  FROM ov_overlap
 ORDER BY topic_id, ov DESC, cluster_id, scope`

// carrySQL is the Fortführungs-Kriterium (Amendment A01-6, decision E1-01):
// mutual plurality AND containment — the new cluster holds at least half of
// the old topic's substance. Integer arithmetic (ov*2 >= size_prev), so the
// criterion is exact and reproducible; size_prev is aggregated from ov_prev,
// i.e. it is the topic's LAST OBSERVED size, not a stored counter that could
// drift.
//
// Why no absolute threshold: it is not scale-invariant. At 2 a stable
// singleton (overlap 1 of 1) loses its identity every single run; at 1 a
// 300-block cluster inherits a topic off two bridge blocks. The relational
// form gets both right — 1*2 >= 1 continues, 2*2 >= 300 does not — and it has
// no knob to mis-set.
//
// The 50/50 case (both halves satisfy the containment) is settled by the
// mutual plurality plus the uuid tiebreak of the two DISTINCT ON orderings:
// exactly ONE half becomes the continuation, deterministically. That is
// documented semantics, not a coin flip.
// Declared as a VAR, not a const, so the W3-G6 gate can swap in the
// retired absolute-threshold criterion and prove it goes red on BOTH
// fixtures. Production never writes to it.
var carrySQL = `
INSERT INTO ov_match (cluster_id, scope, topic_id, ov, carried)
SELECT c.cluster_id, c.scope, c.topic_id, c.ov, true
  FROM ov_best_cluster c
  JOIN ov_best_topic   t ON t.topic_id   = c.topic_id
                        AND t.cluster_id = c.cluster_id
                        AND t.scope      = c.scope
  JOIN ov_prev_size    s ON s.topic_id   = c.topic_id
 WHERE c.ov * 2 >= s.size_prev`

const unmatchedTemplate = `
INSERT INTO ov_unmatched (cluster_id, scope, size_new)
SELECT m.cluster_id, m.scope, count(*)::int
  FROM graph_cluster_member m
 WHERE NOT EXISTS (SELECT 1 FROM ov_match x
                    WHERE x.cluster_id = m.cluster_id AND x.scope = m.scope)%s
 GROUP BY 1, 2`

var (
	overlapGlobalSQL   = fmt.Sprintf(overlapTemplate, "")
	overlapScopedSQL   = fmt.Sprintf(overlapTemplate, "\n WHERE m.scope = ANY($1)")
	unmatchedGlobalSQL = fmt.Sprintf(unmatchedTemplate, "")
	unmatchedScopedSQL = fmt.Sprintf(unmatchedTemplate, "\n   AND m.scope = ANY($1)")
)

func (p topicPhase) matchCarry(ctx context.Context, tx pgx.Tx, st *topicStats) error {
	for _, s := range []struct{ label, sql string }{
		{"prev sizes", prevSizeSQL},
	} {
		if _, err := execPlain(ctx, tx, s.label, s.sql); err != nil {
			return err
		}
	}
	if _, err := p.exec(ctx, tx, "overlap", overlapGlobalSQL, overlapScopedSQL); err != nil {
		return err
	}
	for _, s := range []struct{ label, sql string }{
		{"argmax per cluster", bestClusterSQL},
		{"argmax per topic", bestTopicSQL},
	} {
		if _, err := execPlain(ctx, tx, s.label, s.sql); err != nil {
			return err
		}
	}
	carried, err := execPlain(ctx, tx, "carry-over", carrySQL)
	if err != nil {
		return err
	}
	st.carried = int(carried)
	_, err = p.exec(ctx, tx, "unmatched clusters", unmatchedGlobalSQL, unmatchedScopedSQL)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase 3 — tombstone re-attach (A01-2 / E2-01).

// tombExpandTemplate unnests the core of every tombstone inside the retention
// window into one probe row per (topic, block). Expanded rather than probed
// with `block_id = ANY(core_blocks)`, because the latter is a nested loop over
// tombstones × members and the whole point of the tombstone path is that a
// batch import may leave a LOT of tombstones behind.
//
// The scope predicate here restricts the tombstone set to the run's partition;
// the per-row scope binding against the member row happens in the overlap
// statement below (that one is the B1b half — a tombstone of scope A must not
// be probed against a member row of scope B).
const tombExpandTemplate = `
INSERT INTO ov_tomb (topic_id, scope, block_id, core_n)
SELECT t.topic_id, t.scope, cb, cardinality(t.core_blocks)
  FROM graph_cluster_topic t
  CROSS JOIN LATERAL unnest(t.core_blocks) AS cb
 WHERE t.retired_at IS NOT NULL
   AND t.retired_at >= now() - make_interval(secs => $1)
   AND cardinality(t.core_blocks) > 0%s`

const tombIndexSQL = `CREATE INDEX ov_tomb_block ON ov_tomb (block_id)`

// g.scope = m.scope is the B1b predicate of the tombstone path. Without it a
// tombstone from a foreign scope could be revived onto this scope's node row,
// which is the same leak the living-generation path closes in ov_overlap.
const tombOverlapSQL = `
INSERT INTO ov_tomb_overlap (cluster_id, scope, topic_id, ov, core_n)
SELECT u.cluster_id, u.scope, g.topic_id, count(*)::int, min(g.core_n)
  FROM ov_unmatched u
  JOIN graph_cluster_member m ON m.cluster_id = u.cluster_id AND m.scope = u.scope
  JOIN ov_tomb            g ON g.block_id   = m.block_id   AND g.scope = m.scope
 GROUP BY 1, 2, 3`

const tombBestClusterSQL = `
INSERT INTO ov_tomb_best_cluster (cluster_id, scope, topic_id, ov, core_n)
SELECT DISTINCT ON (cluster_id, scope) cluster_id, scope, topic_id, ov, core_n
  FROM ov_tomb_overlap
 ORDER BY cluster_id, scope, ov DESC, topic_id`

const tombBestTopicSQL = `
INSERT INTO ov_tomb_best_topic (topic_id, cluster_id, scope, ov)
SELECT DISTINCT ON (topic_id) topic_id, cluster_id, scope, ov
  FROM ov_tomb_overlap
 ORDER BY topic_id, ov DESC, cluster_id, scope`

// reattachSQL is the same "half of the substance" rule as the continuation,
// measured against the tombstone's core instead of the topic's last size —
// mutual plurality plus ov*2 >= core_n. It is also the criterion the S-line's
// engine-switch gate quotes (">= 50 % Kern-Overlap", UD-03-04).
//
// carried = true on purpose: a re-attached topic is a continuation of the same
// identity, so it takes the same last_seen_at bump the living path takes.
const reattachSQL = `
INSERT INTO ov_match (cluster_id, scope, topic_id, ov, carried)
SELECT c.cluster_id, c.scope, c.topic_id, c.ov, true
  FROM ov_tomb_best_cluster c
  JOIN ov_tomb_best_topic   t ON t.topic_id   = c.topic_id
                             AND t.cluster_id = c.cluster_id
                             AND t.scope      = c.scope
 WHERE c.ov * 2 >= c.core_n`

// reviveSQL is the resurrection design/01 Revision 2 struck out — and the
// decision record deliberately reinstated for the tombstone form (A01-2). The
// Rev-2 argument (a topic without an ov_match gets no node row, so it can
// never reappear in ov_prev) still holds for the LIVING generation; it does
// not hold here, because the substance now lives on the TOPIC row and survives
// the teardown. merged_into is cleared with it: a revived topic is alive, not
// absorbed, and gct_merge_implies_retired would reject the pair otherwise.
const reviveSQL = `
UPDATE graph_cluster_topic t
   SET retired_at = NULL, merged_into = NULL
  FROM ov_match m
 WHERE m.topic_id = t.topic_id AND t.retired_at IS NOT NULL`

var (
	tombExpandGlobalSQL = fmt.Sprintf(tombExpandTemplate, "")
	tombExpandScopedSQL = fmt.Sprintf(tombExpandTemplate, "\n   AND t.scope = ANY($2)")
)

func (p topicPhase) reattach(ctx context.Context, tx pgx.Tx, st *topicStats) error {
	// tombstone_retention <= 0 disables the re-attach path: the run then
	// behaves like pure one-generation matching and mints fresh identities.
	// That is the fail-closed direction — a missing re-attach costs identity
	// continuity, a wrong one would hand an old label to a new community.
	if p.tombstone <= 0 {
		return nil
	}
	if _, err := p.exec(ctx, tx, "tombstone cores", tombExpandGlobalSQL, tombExpandScopedSQL,
		p.tombstone.Seconds()); err != nil {
		return err
	}
	for _, s := range []struct{ label, sql string }{
		{"tombstone index", tombIndexSQL},
		{"tombstone overlap", tombOverlapSQL},
		{"tombstone argmax per cluster", tombBestClusterSQL},
		{"tombstone argmax per topic", tombBestTopicSQL},
	} {
		if _, err := execPlain(ctx, tx, s.label, s.sql); err != nil {
			return err
		}
	}
	n, err := execPlain(ctx, tx, "re-attach", reattachSQL)
	if err != nil {
		return err
	}
	st.reattached = int(n)
	_, err = execPlain(ctx, tx, "revive", reviveSQL)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase 4 — birth, split, continuation stamp, death and merge.

// mintTemplate creates the identities of every cluster that neither continued
// a living topic nor revived a tombstone.
//
// AS MATERIALIZED is the one place where a PostgreSQL planner detail is
// correctness-bearing: gen_random_uuid() is volatile, and without the hint the
// planner may inline `minted` into BOTH references and produce two DIFFERENT
// uuids — the graph_cluster_topic row and the ov_match row would then point
// past each other and the node insert would break the topic_id foreign key
// (23503), fail-loud but for the wrong reason and hard to read. Negative probe:
// W3-G4.
//
// SPLIT vs BIRTH (Amendment A01-6 derivation): with topic_min_overlap gone,
// the split marker uses the MIRROR containment — at least half of the NEW
// cluster's substance came out of that one old topic (origin_ov*2 >=
// size_new). The forward containment cannot serve here: it is exactly the
// continuation criterion, so by construction no unmatched cluster could ever
// satisfy it and origin_kind='split' would be dead code. The mirror form is
// the same "half of the substance" principle read from the other side and
// needs no parameter either: the classic split (a topic of 11 breaking into 6
// and 5) marks the losing half as 'split' with the mother as origin, while a
// fresh cluster of 10 that happens to share one block with an old topic is a
// plain birth.
// Declared as a VAR so the W3-G4 gate can strip AS MATERIALIZED and show the
// double evaluation of gen_random_uuid(). Production never writes to it.
var mintTemplate = `
WITH unmatched AS (
    SELECT u.cluster_id, u.scope, u.size_new,
           b.topic_id AS origin, COALESCE(b.ov, 0) AS origin_ov
      FROM ov_unmatched u
      LEFT JOIN ov_match        m ON m.cluster_id = u.cluster_id AND m.scope = u.scope
      LEFT JOIN ov_best_cluster b ON b.cluster_id = u.cluster_id AND b.scope = u.scope
     WHERE m.topic_id IS NULL
), minted AS MATERIALIZED (
    SELECT gen_random_uuid() AS topic_id, u.cluster_id, u.scope,
           CASE WHEN u.origin IS NOT NULL AND u.origin_ov * 2 >= u.size_new
                THEN 'split' ELSE 'birth' END AS origin_kind,
           CASE WHEN u.origin IS NOT NULL AND u.origin_ov * 2 >= u.size_new
                THEN u.origin END              AS origin_topic_id
      FROM unmatched u
), ins AS (
    INSERT INTO graph_cluster_topic
           (topic_id, scope, created_at, last_seen_at, origin_kind, origin_topic_id)
    SELECT topic_id, scope, now(), now(), origin_kind, origin_topic_id FROM minted
)
INSERT INTO ov_match (cluster_id, scope, topic_id, ov, carried)
SELECT cluster_id, scope, topic_id, 0, false FROM minted`

// splitCountSQL counts how many of the freshly minted identities carry a
// lineage pointer. Bounded by the birth count, not by the corpus.
const splitCountSQL = `
SELECT count(*)::int
  FROM graph_cluster_topic t
  JOIN ov_match m ON m.topic_id = t.topic_id AND NOT m.carried
 WHERE t.origin_kind = 'split'`

// seenSQL is the continuation stamp. There is deliberately NO retired_at=NULL
// here — the living path cannot resurrect (design/01 §4.3); the ONLY revival
// is the explicit tombstone re-attach above, and it is its own statement so
// the two paths never blur.
const seenSQL = `
UPDATE graph_cluster_topic t SET last_seen_at = now()
  FROM ov_match m WHERE m.topic_id = t.topic_id AND m.carried`

// retireSQL kills every topic of the predecessor generation that got no match.
//
// merged_into answers "where did the majority of this topic go": ov_best_topic
// names the destination cluster, ov_match names that cluster's (inherited or
// freshly minted) identity. NULL means plain death — the blocks were archived,
// fell out of the type cut, or scattered so widely that no cluster holds a
// plurality. NOT NULL is the pointer a stale reference can be redirected along
// instead of dangling.
//
// COALESCE(t.retired_at, now()) is unreachable by construction (a tombstone
// has no node row and therefore never reappears in ov_prev) and stays anyway:
// unlike a resurrection it claims no semantics it does not have, it only
// prevents one — a death timestamp creeping forward every run would push the
// retention horizon along with it.
const retireSQL = `
WITH dead AS (
    SELECT DISTINCT p.topic_id FROM ov_prev p
     WHERE NOT EXISTS (SELECT 1 FROM ov_match m WHERE m.topic_id = p.topic_id)
), absorber AS (
    SELECT d.topic_id, m.topic_id AS into_topic
      FROM dead d
      LEFT JOIN ov_best_topic b ON b.topic_id   = d.topic_id
      LEFT JOIN ov_match      m ON m.cluster_id = b.cluster_id AND m.scope = b.scope
)
UPDATE graph_cluster_topic t
   SET retired_at  = COALESCE(t.retired_at, now()),
       merged_into = a.into_topic
  FROM absorber a
 WHERE a.topic_id = t.topic_id`

func mintAndRetire(ctx context.Context, tx pgx.Tx, st *topicStats) error {
	minted, err := execPlain(ctx, tx, "birth/split", mintTemplate)
	if err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, splitCountSQL).Scan(&st.split); err != nil {
		return fmt.Errorf("topic identity: split count: %w", err)
	}
	st.born = int(minted) - st.split
	if _, err := execPlain(ctx, tx, "carry stamp", seenSQL); err != nil {
		return err
	}
	retired, err := execPlain(ctx, tx, "retire", retireSQL)
	if err != nil {
		return err
	}
	st.retired = int(retired)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase 5 — cores and the K13 measurement.

const coreInsertSQL = `
INSERT INTO ov_core (cluster_id, scope, core_hash, core_blocks)
SELECT c::uuid, s, h, cb::uuid[]
  FROM unnest($1::text[], $2::text[], $3::text[], $4::text[]) AS t(c, s, h, cb)`

// topicCoreSyncSQL copies this run's core onto the TOPIC row. That is what
// makes the tombstone re-attach possible at all: graph_cluster_node dies in
// the next teardown, graph_cluster_topic does not (migration 124 header). A
// topic that dies next run therefore leaves its last core behind as the
// substance a re-appearing community can be recognised by.
//
// The scope predicate keeps a partition run off foreign topic rows — writing
// their unchanged value back would be a no-op semantically but a dead tuple
// per foreign topic per run.
const topicCoreSyncTemplate = `
UPDATE graph_cluster_topic t
   SET core_blocks = oc.core_blocks
  FROM ov_match m
  JOIN ov_core oc ON oc.cluster_id = m.cluster_id AND oc.scope = m.scope
 WHERE m.topic_id = t.topic_id%s`

// membersChangedTemplate is the K13 measurement (masterplan §2, pure counting).
// membersChanged = member sits in a different cluster_id than last run;
// membersReassigned = the subset of those whose TOPIC did not change — i.e.
// pure minUUID renaming, the churn amplifier a delta-persist would pay for.
const membersChangedTemplate = `
SELECT count(*) FILTER (WHERE m.cluster_id <> p.prev_cluster_id)::int,
       count(*) FILTER (WHERE m.cluster_id <> p.prev_cluster_id
                          AND mt.topic_id = p.topic_id)::int
  FROM graph_cluster_member m
  JOIN ov_prev  p ON p.block_id   = m.block_id   AND p.topic_scope = m.scope
  JOIN ov_match mt ON mt.cluster_id = m.cluster_id AND mt.scope = m.scope%s`

var (
	topicCoreSyncGlobalSQL  = fmt.Sprintf(topicCoreSyncTemplate, "")
	topicCoreSyncScopedSQL  = fmt.Sprintf(topicCoreSyncTemplate, "\n   AND t.scope = ANY($1)")
	membersChangedGlobalSQL = fmt.Sprintf(membersChangedTemplate, "")
	membersChangedScopedSQL = fmt.Sprintf(membersChangedTemplate, "\n WHERE m.scope = ANY($1)")
)

func (p topicPhase) writeCores(ctx context.Context, tx pgx.Tx, cores coreArrays) error {
	for i := 0; i < cores.len(); i += coreBatch {
		end := min(i+coreBatch, cores.len())
		if _, err := execPlain(ctx, tx, "core insert", coreInsertSQL,
			cores.clusters[i:end], cores.scopes[i:end], cores.hashes[i:end], cores.blocks[i:end]); err != nil {
			return err
		}
	}
	_, err := p.exec(ctx, tx, "topic core sync", topicCoreSyncGlobalSQL, topicCoreSyncScopedSQL)
	return err
}

func (p topicPhase) measure(ctx context.Context, tx pgx.Tx, st *topicStats) error {
	sql, args := membersChangedGlobalSQL, []any{}
	if p.scoped {
		sql, args = membersChangedScopedSQL, []any{p.scopeFilter}
	}
	if err := tx.QueryRow(ctx, sql, args...).Scan(&st.membersChanged, &st.membersReassigned); err != nil {
		return fmt.Errorf("topic identity: member churn measurement: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// The phase, in the K5 order.

// assignTopics runs the whole identity phase between the member insert and the
// node aggregation. Order is the masterplan's K5 order — teardown, members,
// IDENTITY, aggregation, meta — and nothing in here computes a partition or a
// resolution: Louvain stays outside the transaction.
func assignTopics(ctx context.Context, tx pgx.Tx, p topicPhase, cores coreArrays) (topicStats, error) {
	var st topicStats
	if err := createTopicTemps(ctx, tx); err != nil {
		return st, err
	}
	if err := p.matchCarry(ctx, tx, &st); err != nil {
		return st, err
	}
	if err := p.reattach(ctx, tx, &st); err != nil {
		return st, err
	}
	if err := mintAndRetire(ctx, tx, &st); err != nil {
		return st, err
	}
	if err := p.writeCores(ctx, tx, cores); err != nil {
		return st, err
	}
	if err := p.measure(ctx, tx, &st); err != nil {
		return st, err
	}
	return st, nil
}
