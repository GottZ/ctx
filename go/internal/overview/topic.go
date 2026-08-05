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
//	Re-Attach    (A01-2/E2-01): a cluster that covers at least half of a
//	             tombstone's core within tombstone_retention revives that
//	             identity. This is the batch-import half of the mechanism;
//	             organic growth runs over the living predecessor generation,
//	             both in the SAME run.
//
// PRECEDENCE between the last two (K2-1). The tombstone probe runs for EVERY
// cluster, not just for birth candidates, because the case decision E2-01 was
// written for produces a continuation candidate: a topic that tears into three
// clusters (none holding half) dies and leaves three fragment topics behind, so
// the cluster that later reassembles the community HAS something to continue —
// the fragment — and would keep the fragment's identity forever while the real
// predecessor stayed a tombstone. A tombstone therefore takes a cluster from a
// continuation candidate iff that candidate is a FRAGMENT of this very
// tombstone (its origin_topic_id chain reaches it) and was born no earlier than
// the tombstone died. Against any INDEPENDENT living topic the continuation
// wins — that is the "organic growth and batch imports at the same time" half
// of the decision.
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
// versa), the UNIQUE indexes on ov_carry(topic_id) and ov_match(topic_id),
// which break with 23505 at the cause and hold ACROSS scopes, and
// uq_gcn_scope_topic (migration 124), which catches any path that bypasses the
// temps but is (scope, topic_id) — it therefore guarantees uniqueness WITHIN a
// scope only. That is not a gap: a topic belongs to exactly one scope by
// construction (I1), so a second scope claiming the same topic_id would be a
// different, and louder, defect. All three roll the persist tx back and leave
// the previous map readable.
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
	"log/slog"
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
	// members is this run's member count — the size signal the ANALYZE floor
	// reads (see analyzeFloor).
	members int
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

// analyzeFloor is the member count below which the identity phase does NOT
// collect temp-table statistics.
//
// A declared resource threshold, and a MEASURED one (W3-G11, 2026-08-05, same
// host, same image, 50 clusters):
//
//	20.000 members   ANALYZE costs ~150 ms of a ~480 ms transaction  (+31 %)
//	200.000 members  ANALYZE costs ~ 80 ms of a ~4.84 s transaction  (+1.6 %)
//
// The cost is roughly CONSTANT — ANALYZE samples a fixed number of rows once
// the table exceeds the sample size — so it is a rounding error at the target
// scale and a third of the transaction at the live one. The benefit could not
// be measured at either size in this fixture; the argument for it is the
// asymmetry, not a measured speed-up: below the floor a misestimate cannot
// choose an expensive enough plan to matter, above it one can, and there the
// statistics are nearly free. Spending a third of the lock hold time on a run
// whose plan cannot go wrong is the trade this floor refuses.
const analyzeFloor = 50000

// analyze collects statistics on a freshly filled temp table.
//
// Autovacuum NEVER touches temp tables — they live in the session's own
// namespace and the autovacuum worker cannot see them. Without an explicit
// ANALYZE the planner works from its hardcoded default for EVERY one of them,
// no matter whether the run holds 60 clusters or 200.000 members; at the target
// scale that can pick nested loops over hash joins inside the advisory lock,
// which is the one place in this system where a bad plan is paid for in lock
// hold time.
//
// A failure here is NOT fatal: statistics are an optimisation, and a rebuild
// that dies because it could not collect them would trade a slow map for no
// map. It is logged and the run continues.
func (p topicPhase) analyze(ctx context.Context, tx pgx.Tx, table string) {
	if p.members < analyzeFloor {
		return
	}
	if _, err := tx.Exec(ctx, "ANALYZE "+table); err != nil {
		slog.Warn("overview: could not analyze identity temp table — the planner keeps its default estimate",
			"table", table, "error", err)
	}
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
	if _, err := p.exec(ctx, tx, "predecessor snapshot", prevSnapshotGlobalSQL, prevSnapshotScopedSQL); err != nil {
		return err
	}
	p.analyze(ctx, tx, "ov_prev")
	return nil
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
	// ov_carry holds the CANDIDATE continuations. They only become assignments
	// in the resolution step, because a tombstone can outrank them (K2-1).
	`CREATE TEMP TABLE ov_carry (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, topic_id UUID NOT NULL, ov INT NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
	`CREATE UNIQUE INDEX ov_carry_topic_uq ON ov_carry (topic_id)`,
	`CREATE TEMP TABLE ov_match (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, topic_id UUID NOT NULL,
	     ov INT NOT NULL, carried BOOL NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
	// First line of defence against B2 (identity fusion): it breaks with 23505
	// at the CAUSE instead of carrying the defect down to uq_gcn_scope_topic —
	// which guards the same property but only WITHIN a scope, because the
	// persistent index is (scope, topic_id), not (topic_id). This one is
	// global over the run.
	`CREATE UNIQUE INDEX ov_match_topic_uq ON ov_match (topic_id)`,
	`CREATE TEMP TABLE ov_unmatched (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, size_new INT NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
	// ov_run_scopes is the scope set this run actually produced clusters in.
	// It bounds the tombstone expansion in BOTH run shapes — the global run
	// has no ScopeFilter to bound it with, and without this it would unnest
	// the cores of every tombstone in the database (K3-2).
	`CREATE TEMP TABLE ov_run_scopes (scope TEXT PRIMARY KEY) ON COMMIT DROP`,
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
	// ov_tomb_match is the tombstone side of the assignment, mutually plural
	// and past the half-of-the-core bar — a CANDIDATE like ov_carry, resolved
	// against it in the step below.
	`CREATE TEMP TABLE ov_tomb_match (
	     cluster_id UUID NOT NULL, scope TEXT NOT NULL, topic_id UUID NOT NULL,
	     ov INT NOT NULL, core_n INT NOT NULL,
	     PRIMARY KEY (cluster_id, scope)) ON COMMIT DROP`,
	`CREATE UNIQUE INDEX ov_tomb_match_topic_uq ON ov_tomb_match (topic_id)`,
	// ov_frag is the ancestry closure of the carry candidates: (carry topic,
	// any topic on its origin_topic_id chain). It answers the one question the
	// resolution needs — "is this living topic a FRAGMENT of that tombstone".
	`CREATE TEMP TABLE ov_frag (
	     carry_topic UUID NOT NULL, ancestor UUID NOT NULL,
	     PRIMARY KEY (carry_topic, ancestor)) ON COMMIT DROP`,
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
// It writes CANDIDATES (ov_carry), not assignments. Since K2-1 a tombstone can
// outrank a continuation, and that decision needs both sides on the table.
//
// Declared as a VAR, not a const, so the W3-G6 gate can swap in the retired
// absolute-threshold criterion and prove it goes red on BOTH fixtures.
// Production never writes to it.
var carrySQL = `
INSERT INTO ov_carry (cluster_id, scope, topic_id, ov)
SELECT c.cluster_id, c.scope, c.topic_id, c.ov
  FROM ov_best_cluster c
  JOIN ov_best_topic   t ON t.topic_id   = c.topic_id
                        AND t.cluster_id = c.cluster_id
                        AND t.scope      = c.scope
  JOIN ov_prev_size    s ON s.topic_id   = c.topic_id
 WHERE c.ov * 2 >= s.size_prev`

const runScopesTemplate = `
INSERT INTO ov_run_scopes (scope)
SELECT DISTINCT scope FROM graph_cluster_member%s`

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
	runScopesGlobalSQL = fmt.Sprintf(runScopesTemplate, "")
	runScopesScopedSQL = fmt.Sprintf(runScopesTemplate, "\n WHERE scope = ANY($1)")
	unmatchedGlobalSQL = fmt.Sprintf(unmatchedTemplate, "")
	unmatchedScopedSQL = fmt.Sprintf(unmatchedTemplate, "\n   AND m.scope = ANY($1)")
)

func (p topicPhase) matchCarry(ctx context.Context, tx pgx.Tx) error {
	if _, err := execPlain(ctx, tx, "prev sizes", prevSizeSQL); err != nil {
		return err
	}
	if _, err := p.exec(ctx, tx, "overlap", overlapGlobalSQL, overlapScopedSQL); err != nil {
		return err
	}
	p.analyze(ctx, tx, "ov_overlap")
	for _, s := range []struct{ label, sql string }{
		{"argmax per cluster", bestClusterSQL},
		{"argmax per topic", bestTopicSQL},
	} {
		if _, err := execPlain(ctx, tx, s.label, s.sql); err != nil {
			return err
		}
	}
	if _, err := execPlain(ctx, tx, "carry candidates", carrySQL); err != nil {
		return err
	}
	p.analyze(ctx, tx, "ov_carry")
	_, err := p.exec(ctx, tx, "run scopes", runScopesGlobalSQL, runScopesScopedSQL)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase 3 — tombstone re-attach (A01-2 / E2-01).

// tombExpandSQL unnests the core of every tombstone inside the retention
// window into one probe row per (topic, block). Expanded rather than probed
// with `block_id = ANY(core_blocks)`, because the latter is a nested loop over
// tombstones × members and the whole point of the tombstone path is that a
// batch import may leave a LOT of tombstones behind.
//
// The tombstone set is bounded by ov_run_scopes — the scopes this run actually
// produced clusters in. That is the correct bound in BOTH run shapes: the
// scoped run's filter is a superset of it, and the GLOBAL run has no filter at
// all and would otherwise unnest every tombstone in the database (K3-2).
//
// core_n IS cardinality(core_blocks), i.e. the size of the core AT DEATH — it
// keeps counting core blocks that have since been DELETED from context_blocks
// (core_blocks is a plain uuid[] with no foreign key, by design: the array has
// to survive the block, that is what makes it a tombstone). That is deliberate
// strictness, not an oversight. The bar reads "at least half of the substance
// this topic had when it died came back"; a block that was deleted is substance
// that did not come back, and counting it as neutral would let a single
// survivor of a twelve-block core resurrect the identity. Named consequence: a
// tombstone whose core was largely deleted becomes unreattachable and the
// re-appearing community is a birth — the conservative direction, since a
// missed re-attach costs continuity while a wrong one hands an old label to a
// new community.
const tombExpandSQL = `
INSERT INTO ov_tomb (topic_id, scope, block_id, core_n)
SELECT t.topic_id, t.scope, cb, cardinality(t.core_blocks)
  FROM graph_cluster_topic t
  JOIN ov_run_scopes s ON s.scope = t.scope
  CROSS JOIN LATERAL unnest(t.core_blocks) AS cb
 WHERE t.retired_at IS NOT NULL
   AND t.retired_at >= now() - make_interval(secs => $1)
   AND cardinality(t.core_blocks) > 0`

// tombOverlapSQL probes the tombstone cores against THIS run's members.
//
// Since K2-1 it runs over ALL clusters, not only the unmatched ones: a cluster
// that reassembles a torn-apart community WILL usually have a continuation
// candidate (one of the fragments the tear created), and restricting the probe
// to birth candidates made the E2-01 case unreachable exactly when it mattered.
// The resolution step decides who wins.
//
// ov_tomb DRIVES the join and graph_cluster_member is probed by its primary
// key. The work is therefore bounded by the tombstone core volume — the small
// side — not by the member count, which is what makes the widened probe
// cheaper than the ov_unmatched-driven one it replaces.
//
// g.scope = m.scope is the B1b predicate of the tombstone path. Without it a
// tombstone from a foreign scope could be revived onto this scope's node row,
// which is the same leak the living-generation path closes in ov_overlap.
const tombOverlapSQL = `
INSERT INTO ov_tomb_overlap (cluster_id, scope, topic_id, ov, core_n)
SELECT m.cluster_id, m.scope, g.topic_id, count(*)::int, min(g.core_n)
  FROM ov_tomb g
  JOIN graph_cluster_member m ON m.block_id = g.block_id AND m.scope = g.scope
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

// tombMatchSQL is the same "half of the substance" rule as the continuation,
// measured against the tombstone's core instead of the topic's last size —
// mutual plurality plus ov*2 >= core_n. It is also the criterion the S-line's
// engine-switch gate quotes (">= 50 % Kern-Overlap", UD-03-04).
const tombMatchSQL = `
INSERT INTO ov_tomb_match (cluster_id, scope, topic_id, ov, core_n)
SELECT c.cluster_id, c.scope, c.topic_id, c.ov, c.core_n
  FROM ov_tomb_best_cluster c
  JOIN ov_tomb_best_topic   t ON t.topic_id   = c.topic_id
                             AND t.cluster_id = c.cluster_id
                             AND t.scope      = c.scope
 WHERE c.ov * 2 >= c.core_n`

// fragChainSQL builds the ancestry closure of the carry candidates by walking
// origin_topic_id upwards. It is the evidence the resolution needs for the one
// question that separates "a tear artefact" from "an independent topic": did
// this living topic descend from that tombstone?
//
// The depth cap is a resource bound, not a semantic one: a chain longer than
// eight generations simply is not followed, so the tombstone loses and the
// carry wins — the conservative direction. It also makes the walk structurally
// cycle-proof; gct_no_self_origin only rules out the one-step cycle.
const fragChainSQL = `
WITH RECURSIVE chain(carry_topic, ancestor, depth) AS (
    SELECT c.topic_id, t.origin_topic_id, 1
      FROM ov_carry c
      JOIN graph_cluster_topic t ON t.topic_id = c.topic_id
     WHERE t.origin_topic_id IS NOT NULL
    UNION ALL
    SELECT ch.carry_topic, p.origin_topic_id, ch.depth + 1
      FROM chain ch
      JOIN graph_cluster_topic p ON p.topic_id = ch.ancestor
     WHERE p.origin_topic_id IS NOT NULL AND ch.depth < 8
)
INSERT INTO ov_frag (carry_topic, ancestor)
SELECT DISTINCT carry_topic, ancestor FROM chain`

// tombWinsSQL is the K2-1 precedence rule. A tombstone takes a cluster from a
// living continuation candidate in exactly two situations:
//
//  1. There is no candidate at all — the plain re-attach (the import that
//     dropped its blocks out of the node cut for a generation).
//  2. The candidate is a FRAGMENT of this very tombstone AND was born no
//     earlier than the tombstone died. That is the three-way tear: a topic of
//     ten blocks breaks into three clusters, none of them holding half, so it
//     retires and three fragment topics are born in its place; when the
//     community reassembles, the reassembled cluster carries one fragment and
//     would keep the fragment's identity forever while the real predecessor —
//     whose core is fully contained in it — stayed a tombstone.
//
// Everything else keeps the carry: an INDEPENDENT living topic outranks an old
// tombstone, which is the "organic growth and batch imports at the same time"
// half of decision E2-01. The two conditions together are what makes the rule
// safe — without the ancestry test a tombstone could steal a cluster from a
// topic it never had anything to do with.
//
// created_at >= retired_at, not >: the tear and the births happen in ONE
// transaction, so the fragment's created_at and the tombstone's retired_at are
// the same now().
// Declared as a VAR so the tear gate can reduce it to the pre-K2-1 behaviour
// and prove the reassembled community loses its mother. Production never writes.
var tombWinsSQL = `
INSERT INTO ov_match (cluster_id, scope, topic_id, ov, carried)
SELECT tm.cluster_id, tm.scope, tm.topic_id, tm.ov, true
  FROM ov_tomb_match tm
  LEFT JOIN ov_carry c ON c.cluster_id = tm.cluster_id AND c.scope = tm.scope
 WHERE c.topic_id IS NULL
    OR EXISTS (SELECT 1
                 FROM ov_frag f
                 JOIN graph_cluster_topic ct ON ct.topic_id = c.topic_id
                 JOIN graph_cluster_topic tt ON tt.topic_id = tm.topic_id
                WHERE f.carry_topic  = c.topic_id
                  AND f.ancestor     = tm.topic_id
                  AND ct.created_at >= tt.retired_at)`

// carryWinsSQL promotes every carry candidate a tombstone did not take. The
// losing candidates are NOT special-cased here: they fall out of ov_match, and
// the ordinary death statement retires them with merged_into pointing at the
// cluster's winning identity — which is the revived tombstone. The fragment
// therefore ends up as "aufgegangen in T", which is exactly what happened.
const carryWinsSQL = `
INSERT INTO ov_match (cluster_id, scope, topic_id, ov, carried)
SELECT c.cluster_id, c.scope, c.topic_id, c.ov, true
  FROM ov_carry c
 WHERE NOT EXISTS (SELECT 1 FROM ov_match m
                    WHERE m.cluster_id = c.cluster_id AND m.scope = c.scope)`

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

// probeTombstones fills the tombstone candidate side. It writes no assignment
// — resolve does that.
//
// tombstone_retention <= 0 disables the whole path: the run then behaves like
// pure one-generation matching and mints fresh identities. That is the
// fail-closed direction — a missing re-attach costs identity continuity, a
// wrong one would hand an old label to a new community.
func (p topicPhase) probeTombstones(ctx context.Context, tx pgx.Tx) error {
	if p.tombstone <= 0 {
		return nil
	}
	if _, err := execPlain(ctx, tx, "tombstone cores", tombExpandSQL, p.tombstone.Seconds()); err != nil {
		return err
	}
	p.analyze(ctx, tx, "ov_tomb")
	for _, s := range []struct{ label, sql string }{
		{"tombstone overlap", tombOverlapSQL},
		{"tombstone argmax per cluster", tombBestClusterSQL},
		{"tombstone argmax per topic", tombBestTopicSQL},
		{"tombstone candidates", tombMatchSQL},
		{"fragment ancestry", fragChainSQL},
	} {
		if _, err := execPlain(ctx, tx, s.label, s.sql); err != nil {
			return err
		}
	}
	return nil
}

// resolve turns the two candidate sets into THE assignment: tombstones first
// (they only ever take a cluster under the two conditions of tombWinsSQL),
// carries for everything left, then the revival of whatever tombstone won.
func (p topicPhase) resolve(ctx context.Context, tx pgx.Tx, st *topicStats) error {
	reattached, err := execPlain(ctx, tx, "tombstone precedence", tombWinsSQL)
	if err != nil {
		return err
	}
	st.reattached = int(reattached)
	carried, err := execPlain(ctx, tx, "carry-over", carryWinsSQL)
	if err != nil {
		return err
	}
	st.carried = int(carried)
	p.analyze(ctx, tx, "ov_match")
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
// LINEAGE (origin_topic_id) vs KIND (birth/split), split apart in K2-4:
//
//   - origin_topic_id is set whenever the MIRROR containment holds: at least
//     half of the NEW cluster's substance came out of that one old topic
//     (origin_ov*2 >= size_new). The forward containment cannot serve here —
//     it IS the continuation criterion, and in the 50/50 case both halves
//     satisfy it, so using it would either be dead code or reward whichever
//     half lost the tiebreak. The mirror form is the same "half of the
//     substance" principle read from the other side and needs no parameter.
//   - origin_kind is 'split' ONLY if that mother is still alive in this run's
//     assignment. Design §4.3 defines a split as "the topic went to ANOTHER
//     cluster", which presupposes a survivor. A topic that shatters into three
//     pieces, none of them holding half, is DEAD — calling its three
//     successors 'split' would report a lineage branch where there was a
//     tear, and Stats would read split=3/retired=1 for three plain births.
//
// The lineage pointer survives that distinction on purpose: it is exactly what
// lets the tombstone precedence (tombWinsSQL) recognise the fragments of a torn
// topic when the community reassembles. Migration 124 permits it — the FK is
// nullable, gct_no_self_origin is untouched (the mother is an older row), and
// origin_kind's CHECK vocabulary is unaffected.
//
// AS MATERIALIZED is the one place where a PostgreSQL planner detail is
// correctness-bearing: gen_random_uuid() is volatile, and with a SINGLE
// reference the planner would inline `minted` and produce two DIFFERENT uuids
// — the graph_cluster_topic row and the ov_match row would point past each
// other and the node insert would break the topic_id foreign key (23503).
// Measured on PG18: the two references alone already prevent the inlining, so
// the hint guards a future single-reference refactor rather than today's shape
// (W3-G4 records both halves).
//
// Declared as a VAR so the W3-G4 gate can patch it. Production never writes.
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
                     AND EXISTS (SELECT 1 FROM ov_match m WHERE m.topic_id = u.origin)
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

func (p topicPhase) mintAndRetire(ctx context.Context, tx pgx.Tx, st *topicStats) error {
	// ov_unmatched is filled HERE, after the resolution: since K2-1 a cluster
	// can lose its carry candidate to a tombstone, so "unmatched" is only
	// knowable once ov_match is final.
	if _, err := p.exec(ctx, tx, "unmatched clusters", unmatchedGlobalSQL, unmatchedScopedSQL); err != nil {
		return err
	}
	p.analyze(ctx, tx, "ov_unmatched")
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
//
// Inside the phase the order is: gather BOTH candidate sets (living
// continuations and tombstones), then decide between them, then mint what is
// left over, then bury what got nothing. Candidates before decisions is what
// makes the K2-1 precedence expressible at all — the previous shape wrote the
// continuation straight into ov_match and left the tombstone nothing to argue
// with.
func assignTopics(ctx context.Context, tx pgx.Tx, p topicPhase, cores coreArrays) (topicStats, error) {
	var st topicStats
	if err := createTopicTemps(ctx, tx); err != nil {
		return st, err
	}
	if err := p.matchCarry(ctx, tx); err != nil {
		return st, err
	}
	if err := p.probeTombstones(ctx, tx); err != nil {
		return st, err
	}
	if err := p.resolve(ctx, tx, &st); err != nil {
		return st, err
	}
	if err := p.mintAndRetire(ctx, tx, &st); err != nil {
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
