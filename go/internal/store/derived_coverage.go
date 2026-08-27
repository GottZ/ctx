package store

import (
	"context"
	"fmt"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The coverage counter behind the /api/status operating figure (design/01
// §4.7.4). Two numbers per derived type: how many active blocks of that type
// exist, and how many of them MISS their drift anchor.
//
// # Why the second number has to exist
//
// The abort rule of §4.7.4 was written against a measured dead-end: the label
// arm burned label_attempts to its maximum on 22 of 52 live topics (42 %), 13 of
// them inside a single microsecond-identical tick, while context_llm_log showed
// 0 errors over 143 calls. Nothing was broken enough to raise anything. The gap
// existed for weeks and became visible only when somebody went looking across
// four columns of a foreign table. The rule's answer is not a better arm, it is
// that the gap is an OPERATING FIGURE — one number, on the status surface,
// before anyone thinks to ask.
//
// # What "misses its anchor" means, per anchor form
//
// The drift anchor is provenance.anchor and it belongs to the arm that clears
// it (§4.7.3) — never graph_cluster_topic.label_stale, which the label arm owns.
// The three V7 anchor forms do not drift alike, and the counter follows the
// model rather than flattening them:
//
//   - cluster_topic — the drifting population (§4.1: "driftet, stirbt,
//     verschmilzt"). Covered exactly when a live graph_cluster_node still
//     carries this topic_id WITH this core_hash. That single condition catches
//     all three ways the anchor can fail: the core moved (drift), the topic was
//     retired (§4.7.5 Tear), or it never existed.
//   - root_session — the monotone population (§4.1: "streng monoton, stirbt
//     nie"). watermark_from is the LOWER bound of a closed window and the title
//     carries it (§4.7.1), so a later run writes a NEW block rather than
//     staling this one. It has no core drift to miss, and reporting one would be
//     an invented number.
//   - block — the level-2 anchor. §4.7.5 answers source loss by ARCHIVING, and
//     an archived block is already out of the population below, so there is no
//     second drift signal to read here today. See the wave report's open point.
//
// Anything this build cannot READ as an anchor counts as missed: no provenance
// key, a foreign contract version (§3.2 refuses to decode those, so their anchor
// is not this build's to interpret), or an anchor kind outside the V7 vocabulary.
// Fail-closed is the only safe direction — the number's job is to expose a gap,
// so an unreadable block must never be silently counted as covered.
//
// # Cost at target scale
//
// The read is bounded by idx_blocks_type_name over the derived type names, and
// the core set it joins against is per-CLUSTER, not per-block (a few thousand
// rows at 1M+ blocks, hash-joinable in one pass). It is still O(derived blocks)
// per evaluation, which is why the caller runs it on its own slow cadence
// instead of every status tick (status.go, derivedCoverageInterval).

// DerivedCoverageRow is one derived type's two numbers.
type DerivedCoverageRow struct {
	// TypeName is the block type (insight, catalog).
	TypeName string
	// Blocks counts the ACTIVE blocks of that type, all scopes.
	Blocks int
	// AnchorMissed counts how many of those Blocks cannot be shown to still
	// hang on the thing they were derived from. Blocks - AnchorMissed is the
	// covered part; AnchorMissed alone is the gap §4.7.4 asks for.
	AnchorMissed int
}

// derivedCoverageSQL computes both numbers in one pass.
//
// The LEFT JOIN against the type list (rather than a GROUP BY over the blocks)
// is what makes an empty corpus report 0/0 per type instead of an empty result:
// a MISSING row and "this type has full coverage" must not render the same.
//
// core is DISTINCT because uq_gcn_scope_topic is unique per (scope, topic_id),
// not per topic_id — two scopes could carry the same topic id and fan the join
// out, inflating both counts. DISTINCT costs one sort over a small relation and
// removes the failure mode entirely.
//
// Every anchor field is read as TEXT (->>) and compared as TEXT. Deliberate, and
// the same reasoning blocks.go:312-321 records for the provenance identity: the
// values come out of JSON, where a non-UUID string is perfectly representable,
// and a ::uuid cast on this path would turn a malformed block into a 22P02 that
// takes the whole status refresh down — an availability bug where a comparison
// was wanted. topic_id::text casts the COLUMN instead, which cannot fail.
const derivedCoverageSQL = `
WITH t(type_name) AS (
    SELECT unnest($1::text[])
),
core AS (
    SELECT DISTINCT topic_id::text AS topic_id, core_hash
    FROM graph_cluster_node
    WHERE topic_id IS NOT NULL AND core_hash IS NOT NULL
),
b AS (
    SELECT cb.type_name,
           cb.metadata -> 'provenance'                          AS prov,
           cb.metadata -> 'provenance' ->> 'v'                  AS prov_v,
           cb.metadata -> 'provenance' -> 'anchor' ->> 'kind'      AS anchor_kind,
           cb.metadata -> 'provenance' -> 'anchor' ->> 'topic_id'  AS anchor_topic,
           cb.metadata -> 'provenance' -> 'anchor' ->> 'core_hash' AS anchor_core
    FROM context_blocks cb
    WHERE NOT cb.is_archived
      AND cb.type_name = ANY($1::text[])
),
j AS (
    SELECT b.*, (c.topic_id IS NOT NULL) AS core_alive
    FROM b
    LEFT JOIN core c
           ON b.anchor_kind = 'cluster_topic'
          AND c.topic_id    = b.anchor_topic
          AND c.core_hash   = b.anchor_core
)
SELECT t.type_name,
       count(j.type_name)::int AS blocks,
       (count(j.type_name) FILTER (
           WHERE j.prov IS NULL
              OR j.prov_v IS DISTINCT FROM $2
              OR j.anchor_kind IS NULL
              OR j.anchor_kind NOT IN ('cluster_topic', 'root_session', 'block')
              OR (j.anchor_kind = 'cluster_topic' AND NOT j.core_alive)
       ))::int AS anchor_missed
FROM t
LEFT JOIN j ON j.type_name = t.type_name
GROUP BY t.type_name
ORDER BY array_position($1::text[], t.type_name)`

// DerivedCoverage reports, per derived type name, the active block count and how
// many of those blocks miss their drift anchor (design/01 §4.7.4). Server-global
// across all scopes: the figure describes the LAYER's coverage, and a per-scope
// slice would answer a different question than the abort rule asks.
//
// An empty corpus is not an error — it yields one row per derived type with both
// numbers 0. The row set follows derived.DerivedTypeNames(), so a type that the
// derivation order knows can never be missing from the report.
func DerivedCoverage(ctx context.Context, pool *pgxpool.Pool) ([]DerivedCoverageRow, error) {
	names := derived.DerivedTypeNames()
	rows, err := pool.Query(ctx, derivedCoverageSQL, names, fmt.Sprint(derived.ContractVersion))
	if err != nil {
		return nil, fmt.Errorf("store: derived coverage: %w", err)
	}
	defer rows.Close()

	out := make([]DerivedCoverageRow, 0, len(names))
	for rows.Next() {
		var r DerivedCoverageRow
		if err := rows.Scan(&r.TypeName, &r.Blocks, &r.AnchorMissed); err != nil {
			return nil, fmt.Errorf("store: derived coverage scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: derived coverage rows: %w", err)
	}
	return out, nil
}
