// Retention for dead topics — wave W8 of the Cluster-Topic-Map
// (design/01 §4.8 last section, §6.7).
//
// Topics accumulate: every birth writes a row, every death keeps it. Without a
// horizon graph_cluster_topic grows monotonically with the cumulative number of
// communities that ever existed — the projected steady state is ~2.000
// deaths/day at 10k living clusters, i.e. 730k rows a year.
//
// THREE PROPERTIES, each against a named break:
//
//   - NO advisory lock, and NOT inside the persist transaction. In the steady
//     state the purge is a few hundred rows and would be harmless there; the
//     SWITCH-ON case is not. tombstone_retention is legitimately 0 ("never
//     delete"), and moving it to 45 d after months of operation makes the first
//     purge a six-figure DELETE — inside the held lock, with foreign-key
//     trigger work per row. A tombstone has no atomicity relation to the
//     identity assignment: a grave that disappears one run later is
//     unobservable.
//   - BATCHED, one short transaction each. Same batch size as the member
//     insert, so a single transaction is never open long enough to hold up
//     autovacuum or a parallel rebuild.
//   - The FK COMPANION INDEXES from migration 124 carry it. PostgreSQL indexes
//     the referenced side of a foreign key, never the referencing one, and
//     origin_topic_id/merged_into are self-references — without
//     idx_gct_origin/idx_gct_merged_into every deleted row would force the
//     ON DELETE SET NULL trigger into a sequential scan over the same table,
//     which makes the purge quadratic exactly when it is largest.
//
// The purge is also the declared upper bound of the W3 tombstone re-attach
// window: the grave-precedence rule reads core_blocks of retired topics, and
// what this deletes it can no longer read. One key governs both ends, which is
// why the two can never drift apart.
package overview

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// purgeBatch caps one DELETE. Same value as memberBatch, for the same reason.
const purgeBatch = 5000

// purgeMaxRounds bounds the loop. A declared resource limit, not a semantic
// one: at 5.000 rows per round this is 5M tombstones in one tick, far past the
// projected 180k horizon, and the remainder is simply purged on the next tick.
// It exists so a pathological state cannot turn a background arm into an
// endless one.
const purgeMaxRounds = 1000

// purgeTemplate deletes one batch of expired graves. %s is the partition filter
// of the scoped run.
//
// The NOT EXISTS clause is redundancy against the foreign key, not logic: a
// referenced topic cannot be deleted anyway. It states the intent — only graves
// that no node row still points at — at the place where a reader looks for it.
//
// origin_topic_id and merged_into of SURVIVING rows fall to NULL through
// ON DELETE SET NULL: a lineage chain shortens, it never tears, and a stale
// reference degrades to "died, destination unknown" instead of a dangling id.
//
// Declared as a VAR so the W8 gates can substitute the comparison and prove it
// red. Production never writes to it.
var purgeTemplate = `
WITH doomed AS (
    SELECT topic_id FROM graph_cluster_topic t
     WHERE t.retired_at IS NOT NULL
       AND t.retired_at < now() - make_interval(secs => $1)
       AND NOT EXISTS (SELECT 1 FROM graph_cluster_node n WHERE n.topic_id = t.topic_id)%s
     LIMIT ` + fmt.Sprint(purgeBatch) + `
)
DELETE FROM graph_cluster_topic t USING doomed d WHERE d.topic_id = t.topic_id`

// PurgeTombstones removes topics that died longer ago than retention.
//
// It runs in the PARENT process after the rebuild tick and reads its window
// straight from the config — it never crosses the worker boundary. That is what
// keeps design/01 §3.6's promise of exactly ONE IPC protocol change over the
// whole axis: an Options field for the retention would have been the second.
//
// retention <= 0 means "never delete" and is a legitimate, documented operating
// state (it also switches the W3 re-attach off, so the two ends of the window
// stay consistent). scopeFilter nil is the global run: no scope predicate at
// all, matching the rebuild's own two shapes.
func PurgeTombstones(ctx context.Context, pool *pgxpool.Pool, scopeFilter []string, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	sql := fmt.Sprintf(purgeTemplate, "")
	args := []any{retention.Seconds()}
	if len(scopeFilter) > 0 {
		sql = fmt.Sprintf(purgeTemplate, "\n       AND t.scope = ANY($2)")
		args = append(args, scopeFilter)
	}

	total := 0
	for round := 0; round < purgeMaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		// One short transaction per batch: pool.Exec runs the statement in its
		// own implicit transaction, which is exactly the scope wanted here.
		tag, err := pool.Exec(ctx, sql, args...)
		if err != nil {
			return total, fmt.Errorf("overview: tombstone purge: %w", err)
		}
		n := int(tag.RowsAffected())
		total += n
		if n < purgeBatch {
			return total, nil
		}
	}
	slog.Warn("overview: tombstone purge hit its round cap — the rest goes next tick",
		"purged", total, "rounds", purgeMaxRounds, "scope_filter", scopeFilter)
	return total, nil
}
