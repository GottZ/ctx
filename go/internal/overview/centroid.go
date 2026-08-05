// Centroid build arm of the Cluster-Topic-Map (design/03 §3.2/§4.6 M2, wave
// C8). It fills graph_cluster_centroid: one averaged member embedding per
// (topic_id, scope), so a query can find ITS clusters without going through its
// own RRF hits — the C3 seed derivation is circular by construction, this is
// the way out of the circle.
//
// THREE PROPERTIES CARRY THIS FILE, and each one is a decision, not a detail:
//
//  1. OWN TRANSACTION, AFTER THE PERSIST COMMIT (masterplan K5). The rebuild is
//     all-or-nothing under graph_overview.rebuild_timeout — a SIGKILL mid-persist
//     is a clean tx rollback (events/overview_worker.go). A centroid step INSIDE
//     that transaction would, at the target scale, roll back a complete and good
//     rebuild every time the SUM of Louvain + members + aggregation + centroids
//     overruns: reproducibly, in every cycle, at the same place. The map would
//     freeze, C4 would correctly switch the signal off — and that correct
//     fail-safe would hide the fact that the map is not being built at all.
//
//  2. INCREMENTAL BY member_hash (masterplan K7). A full build moves ~6,9 GB of
//     embedding I/O at 10M members — every six hours, forever. The hash over the
//     sorted member set is the diff carrier: an unchanged partition is skipped
//     entirely. That is the ONLY reason the table is keyed on the stable topic
//     identity: cluster_id is the smallest member UUID and turns over per run, so
//     a diff over it could not tell "unchanged" from "new" (§3.2).
//
//     The hash covers membership AND embedding coverage (the '+'/'-' marker per
//     member): a block that gets its embedding backfilled changes the centroid
//     without changing the member set, and a hash blind to that would skip the
//     recompute forever.
//
//  3. THE minUUID RENAME IS NOT A RECOMPUTE (masterplan K13). A newcomer with a
//     smaller UUID renames a whole community without a single member moving.
//     cluster_id is a run-local USE column here, so that case is a one-column
//     UPDATE — not 6,9 GB of avg() over unchanged embeddings.
//
// The vector never crosses the Go boundary: avg() runs server-side, the result
// stays in PostgreSQL. Nothing in this file holds an embedding.
package overview

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/visibility"
)

// CentroidOptions is the policy surface of the build arm — every field is a
// cluster.centroid_* config key (design/03 §4.9, declared whole in C0).
//
// Deliberately NOT part of overview.Options: the arm runs in the PARENT process
// (the W8 retention purge is the precedent), so nothing here crosses the worker
// IPC boundary and the axis keeps exactly ONE protocol change (W3's).
type CentroidOptions struct {
	// Batch is how many partitions one recompute statement covers
	// (cluster.centroid_batch, default 500). Batching is not cosmetic: one
	// statement over 1,68M member rows would hold an unbounded sort, make
	// progress unobservable and leave no air for live traffic between chunks.
	Batch int
	// WorkMem is the per-batch SET LOCAL work_mem (a PostgreSQL memory literal).
	// It is WHITELISTED, never interpolated raw — see centroidWorkMemRe.
	WorkMem string
	// ANNThreshold is a declared RESOURCE limit (UD-02-03), not a semantic one:
	// below it the read path is an exact scan (no recall question, no index
	// churn), above it this arm builds the HNSW index. <= 0 disables the index
	// path entirely.
	ANNThreshold int
	// VisibleTypes is the resolved retrieval allowlist — the SAME re-check the
	// node aggregation applies (nodeAggTemplate). Without it member_n and
	// node.size would count different sets and the honesty counter would lie.
	// Empty = wiring bug, fails loudly (rrf.Search posture).
	VisibleTypes []string
}

// CentroidStats is the observability of one arm run. Pure counters — nothing in
// the build reads them back.
type CentroidStats struct {
	Dirty      int // partitions whose stored row did not match the live one
	Recomputed int // full centroid recompute (membership or coverage changed)
	Renamed    int // cluster_id-only refresh (K13 minUUID rename, no embedding I/O)
	Swept      int // rows deleted because their partition no longer exists
	Batches    int // recompute statements issued
	IndexState string
}

// centroidWorkMemRe is the SET LOCAL whitelist. PostgreSQL memory literals have
// exactly this shape, and work_mem is a GUC name — neither can be bound as a
// query parameter, so the value HAS to be interpolated. The C0 key comment
// demands the whitelist for that reason: a config value reaching a SET LOCAL
// unvalidated is a settings-write turning into SQL.
var centroidWorkMemRe = regexp.MustCompile(`^[1-9][0-9]{0,6}(kB|MB|GB)$`)

// centroidHNSWIndex is the threshold index (UD-02-03). It is NOT in migration
// 128: the default is the exact scan, and an index that exists from the first
// deploy would be a maintenance cost nobody measured.
const centroidHNSWIndex = "idx_gcc_centroid_hnsw"

// memberSetExpr is the diff carrier's input: the member ids of one partition in
// a total order, each tagged with whether it currently HAS an embedding. Both
// the work-list query and the recompute derive member_hash from this exact
// expression — two spellings would drift and the arm would either recompute
// forever or never.
const memberSetExpr = `sha256(convert_to(string_agg(
        m.block_id::text || CASE WHEN b.embedding IS NULL THEN '-' ELSE '+' END,
        ',' ORDER BY m.block_id), 'UTF8'))`

// centroidJoin is the partition-to-blocks path, identical in both statements:
// node (which carries the identity) → member (which carries the assignment) →
// blocks (which carry the embedding), with the type re-check the node
// aggregation also applies. m.scope = n.scope is not redundant — both tables are
// scope-partitioned and a join without it would mix partitions.
const centroidJoin = `
FROM graph_cluster_node n
JOIN graph_cluster_member m ON m.cluster_id = n.cluster_id AND m.scope = n.scope
JOIN context_blocks b ON b.id = m.block_id AND %s`

// centroidWorkTemplate finds the partitions whose stored centroid no longer
// describes them. Slots: type-visibility fragment, optional scope filter.
//
// HAVING count(b.embedding) > 0 is load-bearing twice over: avg() of an all-NULL
// group is NULL and the column is NOT NULL, and a partition without a single
// embedded member has no centroid to compute — "no row" is the documented state
// for it, never a zero vector (a zero vector would be a cosine-neutral attractor
// that wins nothing and loses nothing, i.e. noise with a confident face).
//
// hash_match separates the two work kinds in ONE roundtrip: false ⇒ the
// membership or its embedding coverage moved ⇒ full recompute; true ⇒ only
// cluster_id drifted ⇒ the K13 rename, one column, no embedding I/O.
const centroidWorkTemplate = `
WITH cur AS (
    SELECT n.topic_id, n.scope, n.cluster_id, ` + memberSetExpr + ` AS member_hash` +
	centroidJoin + `
    WHERE n.topic_id IS NOT NULL %s
    GROUP BY n.topic_id, n.scope, n.cluster_id
    HAVING count(b.embedding) > 0
)
SELECT cur.topic_id::text,
       (c.topic_id IS NOT NULL AND c.member_hash = cur.member_hash) AS hash_match
FROM cur
LEFT JOIN graph_cluster_centroid c
       ON c.topic_id = cur.topic_id AND c.scope = cur.scope
WHERE c.topic_id IS NULL
   OR c.member_hash IS DISTINCT FROM cur.member_hash
   OR c.cluster_id IS DISTINCT FROM cur.cluster_id
ORDER BY cur.topic_id`

// centroidUpsertTemplate recomputes one batch. UPSERT rather than
// delete-and-insert: the step is then idempotent and re-runnable, a timeout
// mid-run leaves committed progress instead of nothing, and the incremental path
// and the cold-start path are the SAME code.
//
// Slots: type-visibility fragment, optional scope filter.
const centroidUpsertTemplate = `
INSERT INTO graph_cluster_centroid
       (topic_id, scope, cluster_id, centroid, member_n, embedded_n, member_hash, computed_at)
SELECT n.topic_id, n.scope, n.cluster_id,
       avg(b.embedding)::halfvec(1024),
       count(*)::int,
       count(b.embedding)::int,
       ` + memberSetExpr + `,
       now()` +
	centroidJoin + `
WHERE n.topic_id = ANY($2::uuid[]) %s
GROUP BY n.topic_id, n.scope, n.cluster_id
HAVING count(b.embedding) > 0
ON CONFLICT (topic_id, scope) DO UPDATE SET
    cluster_id  = EXCLUDED.cluster_id,
    centroid    = EXCLUDED.centroid,
    member_n    = EXCLUDED.member_n,
    embedded_n  = EXCLUDED.embedded_n,
    member_hash = EXCLUDED.member_hash,
    computed_at = EXCLUDED.computed_at`

// centroidRenameSQL is the K13 path: the community was renamed, not changed.
// computed_at deliberately does NOT move — nothing was computed, and a
// timestamp that claims otherwise would make a stale centroid look fresh.
const centroidRenameSQL = `
UPDATE graph_cluster_centroid c
   SET cluster_id = n.cluster_id
  FROM graph_cluster_node n
 WHERE n.topic_id = c.topic_id
   AND n.scope    = c.scope
   AND c.topic_id = ANY($1::uuid[])
   AND c.cluster_id IS DISTINCT FROM n.cluster_id`

var (
	centroidWorkSQL       = fmt.Sprintf(centroidWorkTemplate, visibility.TypeVisible("b", "$1"), "")
	centroidWorkScopedSQL = fmt.Sprintf(centroidWorkTemplate, visibility.TypeVisible("b", "$1"),
		"AND n.scope = ANY($2)")

	centroidUpsertSQL       = fmt.Sprintf(centroidUpsertTemplate, visibility.TypeVisible("b", "$1"), "")
	centroidUpsertScopedSQL = fmt.Sprintf(centroidUpsertTemplate, visibility.TypeVisible("b", "$1"),
		"AND n.scope = ANY($3)")
)

// BuildCentroids refreshes graph_cluster_centroid for the partitions this
// rebuild owns. Call it AFTER the persist transaction has committed and only for
// a run that actually persisted — a skipped rebuild changed no membership, so
// the diff would find nothing anyway and the run would be pure cost.
//
// scopeFilter nil/empty = the global pass (every partition); non-empty = exactly
// those scopes, in every statement including the orphan sweep. That congruence
// is the same B1-C1 rule the persist teardown lives by: a scoped run must never
// touch a foreign partition's rows.
func BuildCentroids(ctx context.Context, pool *pgxpool.Pool, scopeFilter []string, opts CentroidOptions) (CentroidStats, error) {
	var st CentroidStats
	if len(opts.VisibleTypes) == 0 {
		return st, fmt.Errorf("overview: centroid build with empty type allowlist — block-type registry not wired?")
	}
	if opts.WorkMem != "" && !centroidWorkMemRe.MatchString(opts.WorkMem) {
		return st, fmt.Errorf("overview: cluster.centroid_work_mem %q is not a PostgreSQL memory literal (e.g. 256MB)", opts.WorkMem)
	}
	batch := opts.Batch
	if batch <= 0 {
		batch = 500
	}
	scoped := len(scopeFilter) > 0

	swept, err := sweepOrphanCentroids(ctx, pool, scopeFilter, scoped)
	if err != nil {
		return st, err
	}
	st.Swept = swept

	recompute, rename, err := centroidWorkList(ctx, pool, scopeFilter, scoped, opts.VisibleTypes)
	if err != nil {
		return st, err
	}
	st.Dirty = len(recompute) + len(rename)

	if len(rename) > 0 {
		tag, err := pool.Exec(ctx, centroidRenameSQL, rename)
		if err != nil {
			return st, fmt.Errorf("overview: centroid rename refresh: %w", err)
		}
		st.Renamed = int(tag.RowsAffected())
	}

	// ONE TRANSACTION PER BATCH, not one for the whole run. SET LOCAL work_mem
	// needs a transaction, and a single long one would hold its snapshot for the
	// entire 6,9 GB pass. Per-batch commits make partial progress durable: a run
	// cut short by centroid_timeout leaves the partitions it finished refreshed,
	// and the next cycle's diff picks up exactly the rest. Progress is monotonic
	// across cycles instead of all-or-nothing within one.
	for start := 0; start < len(recompute); start += batch {
		end := min(start+batch, len(recompute))
		n, err := upsertCentroidBatch(ctx, pool, recompute[start:end], scopeFilter, scoped, opts)
		st.Recomputed += n
		st.Batches++
		if err != nil {
			return st, err
		}
	}

	st.IndexState, err = reconcileCentroidIndex(ctx, pool, opts.ANNThreshold)
	if err != nil {
		return st, err
	}
	return st, nil
}

// sweepOrphanCentroids removes rows whose partition no longer exists.
//
// THIS IS THE TEARDOWN, MOVED (deviation from design/03 §3.2, deliberate). The
// design extends the persist teardown — TRUNCATE globally, scoped DELETE
// otherwise — with the centroid table. That form is incompatible with the
// incremental path this wave exists to build: a teardown inside the persist
// transaction deletes every row the diff would compare against, so member_hash
// could never report "unchanged" and every cycle would be a full 6,9 GB rebuild.
// K7 outranks the teardown symmetry, so the removal moved here: a targeted
// anti-join sweep, in the arm's own transaction, under the SAME scope
// congruence. A scoped run deletes only rows of ITS scopes; the gate probes
// exactly that.
func sweepOrphanCentroids(ctx context.Context, pool *pgxpool.Pool, scopeFilter []string, scoped bool) (int, error) {
	const base = `
DELETE FROM graph_cluster_centroid c
 WHERE NOT EXISTS (
     SELECT 1 FROM graph_cluster_node n
      WHERE n.topic_id = c.topic_id AND n.scope = c.scope)`
	var (
		tag pgconn.CommandTag
		err error
	)
	if scoped {
		tag, err = pool.Exec(ctx, base+` AND c.scope = ANY($1)`, scopeFilter)
	} else {
		tag, err = pool.Exec(ctx, base)
	}
	if err != nil {
		return 0, fmt.Errorf("overview: centroid orphan sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// centroidWorkList splits the dirty partitions into the two work kinds.
func centroidWorkList(ctx context.Context, pool *pgxpool.Pool, scopeFilter []string, scoped bool, visibleTypes []string) (recompute, rename []string, err error) {
	var rows pgx.Rows
	if scoped {
		rows, err = pool.Query(ctx, centroidWorkScopedSQL, visibleTypes, scopeFilter)
	} else {
		rows, err = pool.Query(ctx, centroidWorkSQL, visibleTypes)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("overview: centroid work list: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var topicID string
		var hashMatch bool
		if err := rows.Scan(&topicID, &hashMatch); err != nil {
			return nil, nil, fmt.Errorf("overview: centroid work list scan: %w", err)
		}
		if hashMatch {
			rename = append(rename, topicID)
		} else {
			recompute = append(recompute, topicID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("overview: centroid work list rows: %w", err)
	}
	return recompute, rename, nil
}

// upsertCentroidBatch recomputes one chunk in its own transaction.
func upsertCentroidBatch(ctx context.Context, pool *pgxpool.Pool, topicIDs, scopeFilter []string, scoped bool, opts CentroidOptions) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("overview: centroid batch begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	if opts.WorkMem != "" {
		// Validated against centroidWorkMemRe in BuildCentroids — the literal
		// cannot be a bind parameter (GUC assignment), so the whitelist IS the
		// injection barrier.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL work_mem = '%s'`, opts.WorkMem)); err != nil {
			return 0, fmt.Errorf("overview: centroid work_mem: %w", err)
		}
	}

	var tag pgconn.CommandTag
	if scoped {
		tag, err = tx.Exec(ctx, centroidUpsertScopedSQL, opts.VisibleTypes, topicIDs, scopeFilter)
	} else {
		tag, err = tx.Exec(ctx, centroidUpsertSQL, opts.VisibleTypes, topicIDs)
	}
	if err != nil {
		return 0, fmt.Errorf("overview: centroid upsert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("overview: centroid batch commit: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// reconcileCentroidIndex applies the UD-02-03 resource limit: an exact scan
// below the threshold, an HNSW index above it.
//
// DEVIATION FROM design/03 §3.2, and it follows from the incremental path. The
// design prescribes build-and-swap — write to graph_cluster_centroid_next, build
// the index once, RENAME — because it assumed the table is fully re-written
// every cycle (~83.000 rows deleted and re-inserted at 10M). With the K7 diff
// that assumption is gone: a cycle rewrites only the partitions that actually
// moved, so the pathology build-and-swap was invented against (83.000 single
// HNSW insertions with ef_construction=128 per cycle, plus the deleted-entry
// bloat of a 6h full churn) does not exist. Swapping a whole table each cycle
// would instead FORCE the full rewrite back — it would trade the index churn for
// exactly the 6,9 GB the diff removes. So the swap degenerates to what it always
// was underneath: the index is created ONCE when the corpus crosses the
// threshold, and dropped again if it falls well below.
//
// Hysteresis (drop only below half the threshold) is deliberate: without it a
// corpus sitting on the boundary would pay an index build every six hours.
func reconcileCentroidIndex(ctx context.Context, pool *pgxpool.Pool, threshold int) (string, error) {
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes
		                 WHERE tablename = 'graph_cluster_centroid' AND indexname = $1)`,
		centroidHNSWIndex).Scan(&exists); err != nil {
		return "", fmt.Errorf("overview: centroid index probe: %w", err)
	}
	if threshold <= 0 {
		if exists {
			if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+centroidHNSWIndex); err != nil {
				return "", fmt.Errorf("overview: centroid index drop: %w", err)
			}
			return "dropped", nil
		}
		return "absent", nil
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM graph_cluster_centroid`).Scan(&rows); err != nil {
		return "", fmt.Errorf("overview: centroid row count: %w", err)
	}
	switch {
	case rows > threshold && !exists:
		// Plain CREATE INDEX, not CONCURRENTLY: it takes a SHARE lock, which
		// blocks writers — and the ONLY writer of this table is this arm, which
		// is right here. Readers (the retrieval path) are unaffected.
		if _, err := pool.Exec(ctx,
			`CREATE INDEX IF NOT EXISTS `+centroidHNSWIndex+
				` ON graph_cluster_centroid USING hnsw (centroid halfvec_cosine_ops)`); err != nil {
			return "", fmt.Errorf("overview: centroid index build: %w", err)
		}
		return "created", nil
	case rows <= threshold/2 && exists:
		if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+centroidHNSWIndex); err != nil {
			return "", fmt.Errorf("overview: centroid index drop: %w", err)
		}
		return "dropped", nil
	case exists:
		return "present", nil
	default:
		return "absent", nil
	}
}

// CentroidTimeout clamps the arm's own budget. It exists so the caller cannot
// accidentally inherit graph_overview.rebuild_timeout — the whole point of the
// arm's own transaction is that the two budgets are separate (§6.2 point 3).
func CentroidTimeout(configured time.Duration) time.Duration {
	if configured <= 0 {
		return 5 * time.Minute
	}
	return configured
}
