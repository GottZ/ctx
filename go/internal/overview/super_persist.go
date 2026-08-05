// Write path of the meta-cluster level (W-F): the ONLY part of the supergraph
// work that happens inside the persist transaction, and deliberately the
// cheapest — two INSERTs over at most `TargetRows` groups plus their membership,
// and one projection of the freshly aggregated edges onto the topic identity.
// The resolution search that produced the grouping ran outside every transaction
// (super.go header, K5).

package overview

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// topicEdgeAggTemplate projects graph_cluster_edge onto the TOPIC identity
// (K10). The slot is the scope filter.
//
// Why a persisted projection at all, when store.projectEdgesOntoTopics already
// does it in Go per request: a request-time projection answers ONE question and
// dies with the response. This table is the graph a second clustering pass runs
// over, and the endpoint a map row or a retrieval signal can still resolve after
// the next rebuild renamed every cluster_id.
//
// The pair is normalised on the TOPIC ids, and scope_a/scope_b are swapped
// TOGETHER with them. That is the K1-2 lesson verbatim: graph_cluster_edge
// normalises cluster_a < cluster_b, but topic order is unrelated to cluster
// order — reusing the source row's scope_s/scope_t would bind topic_a to the
// scope of the OTHER endpoint, and the read path would resolve the wrong half of
// a scope-crossing cluster or nothing at all.
//
// The GROUP BY is load-bearing rather than tidy: a scope-crossing cluster
// contributes two edge rows that collapse onto the same topic pair, and without
// the aggregation the second one would hit the primary key with 23505.
const topicEdgeAggTemplate = `
INSERT INTO graph_cluster_topic_edge (topic_a, topic_b, scope_a, scope_b, link_count, weight_sum)
SELECT LEAST(na.topic_id, nb.topic_id),
       GREATEST(na.topic_id, nb.topic_id),
       CASE WHEN na.topic_id <= nb.topic_id THEN na.scope ELSE nb.scope END,
       CASE WHEN na.topic_id <= nb.topic_id THEN nb.scope ELSE na.scope END,
       sum(e.link_count)::int, sum(e.weight_sum)::real
  FROM graph_cluster_edge e
  JOIN graph_cluster_node na ON na.cluster_id = e.cluster_a AND na.scope = e.scope_s
  JOIN graph_cluster_node nb ON nb.cluster_id = e.cluster_b AND nb.scope = e.scope_t
 WHERE na.topic_id IS NOT NULL AND nb.topic_id IS NOT NULL
   AND na.topic_id <> nb.topic_id%s
 GROUP BY 1, 2, 3, 4`

var (
	topicEdgeAggSQL       = fmt.Sprintf(topicEdgeAggTemplate, "")
	topicEdgeAggScopedSQL = fmt.Sprintf(topicEdgeAggTemplate,
		"\n   AND e.scope_s = ANY($1) AND e.scope_t = ANY($1)")
)

// superWriteSQL materialises one rebuild's meta level.
//
// Both CTEs are MATERIALIZED on purpose and it is not a hint about cost:
// gen_random_uuid() is VOLATILE, and an inlined `ids` would be re-evaluated at
// every reference — the membership rows would then point at super ids that the
// graph_cluster_super INSERT never wrote. `mem` is referenced twice and inherits
// the same requirement.
//
// The membership INSERT is the primary statement and the group INSERT a
// data-modifying CTE: PostgreSQL runs those exactly once and to completion
// regardless of whether the primary query reads them, so the two land together
// or not at all — inside the persist transaction, under its advisory lock and
// behind its teardown, which is what keeps the B1-C1 atomicity one level up.
//
// lead_topic_id is the biggest child topic, cluster_id as the deterministic
// tiebreak: the map prints that topic's label as the group's name, so a wobbling
// choice would rewrite the map without the partition having moved.
const superWriteSQL = `
WITH g AS MATERIALIZED (
    SELECT DISTINCT ord, scope
      FROM unnest($1::int[], $2::text[], $3::text[]) AS t(ord, scope, cluster_id)
), ids AS MATERIALIZED (
    SELECT ord, scope, gen_random_uuid() AS super_id FROM g
), mem AS MATERIALIZED (
    SELECT i.super_id, i.scope, n.topic_id, n.size, n.cluster_id
      FROM unnest($1::int[], $2::text[], $3::text[]) AS t(ord, scope, cluster_id)
      JOIN ids i ON i.ord = t.ord
      JOIN graph_cluster_node n
        ON n.cluster_id = t.cluster_id::uuid AND n.scope = t.scope
     WHERE n.topic_id IS NOT NULL
), ins_super AS (
    INSERT INTO graph_cluster_super (super_id, scope, size, topic_n, lead_topic_id)
    SELECT super_id, scope, sum(size)::int, count(*)::int,
           (array_agg(topic_id ORDER BY size DESC, cluster_id))[1]
      FROM mem
     GROUP BY super_id, scope
)
INSERT INTO graph_cluster_super_member (topic_id, scope, super_id)
SELECT topic_id, scope, super_id FROM mem`

// superMetaSQL writes the two liveness columns of migration 127.
//
// A separate statement rather than two more bind parameters on metaWrite*SQL:
// those two statements are the W-A contract and every parameter added to them is
// a place where a scope's numbers can end up on another scope's row. This one
// touches exactly the rows the run just wrote (both persist branches DELETE and
// re-INSERT the meta rows first, so super_n starts out NULL every run and a
// disabled level simply leaves it that way).
const superMetaSQL = `
UPDATE graph_overview_meta m
   SET super_n = s.n, super_resolution = s.res
  FROM unnest($1::text[], $2::int[], $3::float8[]) AS s(scope, n, res)
 WHERE m.scope = s.scope`

// writeTopicEdges runs the K10 projection. It must run AFTER the edge
// aggregation (it reads graph_cluster_edge) and AFTER the identity phase (it
// reads graph_cluster_node.topic_id) — persist keeps that order.
func writeTopicEdges(ctx context.Context, tx pgx.Tx, scoped bool, scopeFilter []string) error {
	sql, args := topicEdgeAggSQL, []any(nil)
	if scoped {
		sql, args = topicEdgeAggScopedSQL, []any{scopeFilter}
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("topic edge projection: %w", err)
	}
	return nil
}

// writeSuperLevel persists the grouping of this run. A level that was never
// attempted writes nothing at all — and its meta columns stay NULL, which is how
// the map tells "off" from "capped".
func writeSuperLevel(ctx context.Context, tx pgx.Tx, l superLevel) error {
	if !l.Attempted {
		return nil
	}
	ords, scopes, clusters := superArrays(l)
	if len(ords) == 0 {
		return nil // every scope capped, or nothing to group
	}
	if _, err := tx.Exec(ctx, superWriteSQL, ords, scopes, clusters); err != nil {
		return fmt.Errorf("super level write: %w", err)
	}
	return nil
}

// writeSuperMeta stamps super_n/super_resolution. It runs AFTER the meta write
// and not next to the grouping INSERT, and the order is load-bearing: BOTH
// persist branches delete and re-insert their meta rows, so a stamp placed
// before them would decorate rows that are about to be thrown away — and the
// map would report "no meta level" on a run that built one.
func writeSuperMeta(ctx context.Context, tx pgx.Tx, l superLevel) error {
	metaScopes, ns, gammas := superMetaArrays(l)
	if len(metaScopes) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, superMetaSQL, metaScopes, ns, gammas); err != nil {
		return fmt.Errorf("super level meta: %w", err)
	}
	return nil
}
