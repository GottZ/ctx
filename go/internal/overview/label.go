// The deterministic fallback label and the drift materialization — wave W5 of
// the Cluster-Topic-Map (design/01 §4.6, decisions E4-01 / E5-01).
//
// THE GUARANTEE: after every rebuild, every living topic carries a non-empty
// label. No LLM is involved, no backend has to exist, no pipeline has to have
// succeeded — the label is written in the SAME advisory-locked transaction that
// writes the identity and the core. The root map therefore never waits on a
// model, and the W6 pipeline is an IMPROVEMENT of an existing label rather than
// the thing that makes the map readable at all.
//
// THE THREE STAGES, strongest first (design/01 §4.6):
//
//  1. the three most frequent tags of the SUBSTANZ-KERN — not of all members:
//     the core is the statement about the cluster, the rim is noise;
//  2. the three most frequent categories (category is NOT NULL, so this stage
//     effectively always fires — live it carries the 21 % of groups without
//     any tag);
//  3. the representative title, i.e. today's behaviour as the last readable
//     rung.
//
// A fourth rung exists that the design does not name: a code-owned constant for
// the degenerate group with no tags, a blank category and a blank title. It is
// unreachable in practice and it is NOT cosmetic — gct_label_present forbids
// label_source <> 'none' with a NULL label and gct_label_len forbids the empty
// string, so an empty cascade would raise 23514 INSIDE the persist tx and roll
// the whole rebuild back. The guarantee has to be total or it is not a
// guarantee: a display gap must never be able to freeze the map.
//
// THE CAP IS LOAD-BEARING FOR THE SAME REASON. There is no length limit on
// tags anywhere — neither a CHECK on context_blocks nor a validation in the
// write path — so a single 200-character tag on a single core block would
// otherwise break gct_label_len and freeze every future rebuild of that
// partition. The cap sits OUTSIDE the whole COALESCE, not on the last stage.
//
// WHAT IT MUST NOT TOUCH (E5-01): 'llm' and 'manual' labels. The filter lives
// in the CASE arms rather than in the WHERE, because label_stale and
// label_attempts have to be maintained for those rows TOO — a pinned topic
// whose core drifted is legitimately stale (the W6 selection excludes it
// separately), and an 'llm' row is exactly where drift detection earns its
// keep.
package overview

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// fallbackLastResort is the terminal rung of the cascade. Deliberately a bare
// technical noun and NOT a localized phrase: the other three stages are pure
// corpus data (tags, categories, a title), so the fallback surface carries no
// language of its own, and E3-01 binds the LABEL language to dream.language
// only for the LLM half.
const fallbackLastResort = "cluster"

// fallbackTagStage aggregates the top three tags of the core blocks.
//
// b.scope = n.scope is redundant to the construction — core_blocks is
// scope-pure by I2 — and stands there anyway: the label goes on the wire in
// W7, so a scope leak here would be a content leak, and this is the line a
// negative probe removes to go red.
const fallbackTagStage = `(SELECT string_agg(x.tg, ' · ' ORDER BY x.cnt DESC, x.tg)
                 FROM (SELECT tg.tg, count(*) AS cnt
                         FROM unnest(n.core_blocks) AS cb
                         JOIN context_blocks b ON b.id = cb AND b.scope = n.scope
                         CROSS JOIN LATERAL unnest(b.tags) AS tg(tg)
                        WHERE btrim(tg.tg) <> ''
                        GROUP BY tg.tg
                        ORDER BY count(*) DESC, tg.tg
                        LIMIT 3) x)`

// fallbackCategoryStage reads the already-aggregated category_counts of the
// node row — no second pass over context_blocks.
const fallbackCategoryStage = `(SELECT string_agg(y.cat, ' · ' ORDER BY y.cnt DESC, y.cat)
                 FROM (SELECT key AS cat, (value)::int AS cnt
                         FROM jsonb_each(n.category_counts)
                        WHERE btrim(key) <> ''
                        ORDER BY (value)::int DESC, key
                        LIMIT 3) y)`

// fallbackTitleStage is today's map text. nullif(btrim(...)) rather than the
// bare column: a whitespace-only title is not a label, it is an empty cell that
// would pass COALESCE and fail the CHECK.
const fallbackTitleStage = `nullif(btrim(n.repr_title), '')`

// fallbackLabelTemplate writes the fallback label and materializes the drift
// state in ONE statement over the node rows this run just wrote. %s is the
// partition filter of the scoped run.
//
// Why one statement: the UPDATE touches the topic row anyway, so label_stale
// and the attempt-counter reset ride along for free (design/01 §6.3 counts it
// as a single pass). Why the drift comparison lives HERE: core_hash is freshly
// written one statement earlier, and the comparison
// (label_core_hash IS DISTINCT FROM core_hash) spans two tables and is
// therefore not index-addressable — materializing it into label_stale is what
// makes the W6 selection an index scan over idx_gct_label_pending instead of a
// sequential scan over a table full of tombstones.
//
// Why the attempt reset: a drifted core is a NEW input and therefore a
// legitimate reason for a new attempt. Without it a topic that failed three
// times stays unlabelled FOREVER, even after its content changed completely.
//
// Declared as a VAR so the W5 gates can patch it and prove each guard red.
// Production never writes to it.
var fallbackLabelTemplate = `
UPDATE graph_cluster_topic t
   SET label          = CASE WHEN t.label_source IN ('none','fallback') THEN f.fallback_label ELSE t.label END,
       label_source   = CASE WHEN t.label_source IN ('none','fallback') THEN 'fallback' ELSE t.label_source END,
       label_built_at = CASE WHEN t.label_source IN ('none','fallback') THEN now() ELSE t.label_built_at END,
       label_stale    = (t.label_core_hash IS DISTINCT FROM f.core_hash),
       label_attempts = CASE WHEN t.label_core_hash IS DISTINCT FROM f.core_hash THEN 0 ELSE t.label_attempts END
  FROM (
    SELECT n.topic_id, n.core_hash,
           btrim(left(btrim(regexp_replace(COALESCE(
             ` + fallbackTagStage + `,
             ` + fallbackCategoryStage + `,
             ` + fallbackTitleStage + `,
             '` + fallbackLastResort + `'
           ), '\s+', ' ', 'g')), 120)) AS fallback_label
      FROM graph_cluster_node n
     WHERE n.topic_id IS NOT NULL%s
  ) f
 WHERE f.topic_id = t.topic_id`

// writeFallbackLabels runs the W5 statement for this run's shape. It has to run
// AFTER the node aggregation — the cascade reads core_blocks, category_counts
// and repr_title off the freshly written node rows — and inside the same
// transaction, so a failure rolls the identity back with it instead of leaving
// a map with topics nobody named.
func (p topicPhase) writeFallbackLabels(ctx context.Context, tx pgx.Tx) (int64, error) {
	return p.exec(ctx, tx, "fallback label",
		fmt.Sprintf(fallbackLabelTemplate, ""),
		fmt.Sprintf(fallbackLabelTemplate, "\n       AND n.scope = ANY($1)"))
}
