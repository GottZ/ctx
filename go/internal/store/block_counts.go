package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// The block counters (migration 170, issue #266): the number of blocks per
// (scope, category, is_archived, guard_status) is the folded row in
// context_block_counts plus the delta rows of the key in
// context_block_count_deltas. Statement-level triggers on context_blocks
// append one delta row per key and statement in the writer's transaction —
// no shared row is updated or locked by a writer. The scheduler folds the
// delta rows into the counter rows (FoldBlockCountDeltas); GetStats,
// GetGuardStats, ListCategories, ActiveBlockCount and ActiveCategoryCount sum
// both tables for their read scopes. The reconcile below compares the sum with
// a real count per scope and corrects drift — the repair for writes the
// triggers cannot see (TRUNCATE is covered; ALTER TABLE … DISABLE TRIGGER and
// session_replication_role = replica are not).

// blockCountRowsSQL is the counter source of every reader: the folded rows and
// the pending deltas, one row stream. A reader's WHERE on scope reaches both
// branches (scope leads the unique key of the one and idx_block_count_deltas_scope
// of the other).
const blockCountRowsSQL = `(SELECT scope, category, is_archived, guard_status, n FROM context_block_counts
		  UNION ALL
		  SELECT scope, category, is_archived, guard_status, n FROM context_block_count_deltas)`

// blockCountFoldSQL moves every delta row the statement's snapshot sees into
// the counter rows, in one statement: the deleted rows and the added sums
// commit together, so any reader's snapshot sees the same total before and
// after. Delta rows of writers still in flight are not in the snapshot and
// stay for the next fold. GROUP BY makes every key unique within the upsert
// (ON CONFLICT may touch a row only once per statement); keys that net to zero
// are deleted and not applied.
const blockCountFoldSQL = `WITH d AS (
	    DELETE FROM context_block_count_deltas
	    RETURNING scope, category, is_archived, guard_status, n
	), a AS (
	    SELECT scope, category, is_archived, guard_status, sum(n)::bigint AS n
	      FROM d GROUP BY 1, 2, 3, 4 HAVING sum(n) <> 0
	), up AS (
	    INSERT INTO context_block_counts AS c (scope, category, is_archived, guard_status, n)
	    SELECT scope, category, is_archived, guard_status, n FROM a ORDER BY 1, 2, 3, 4
	    ON CONFLICT (scope, category, is_archived, guard_status) DO UPDATE SET n = c.n + EXCLUDED.n
	    RETURNING 1
	)
	SELECT (SELECT count(*) FROM d)::int`

// blockCountFoldLockSQL keeps two folds (several ctx processes on one
// database) from working on the same delta rows: the loser skips its turn.
const blockCountFoldLockSQL = `SELECT pg_try_advisory_xact_lock(hashtext('ctx:block_count_fold'))`

// blockCountReconcileLockSQL serializes the correction of one scope: two
// reconciles that saw the same drift would otherwise both add the difference.
const blockCountReconcileLockSQL = `SELECT pg_advisory_xact_lock(hashtext('ctx:block_count_reconcile:' || $1::text))`

// FoldBlockCountDeltas folds the pending delta rows into the counter rows and
// returns how many delta rows it folded (0 when another fold holds the turn).
func FoldBlockCountDeltas(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	folded := 0
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: block count fold"), func(tx pgx.Tx) error {
		var mine bool
		if err := tx.QueryRow(ctx, blockCountFoldLockSQL).Scan(&mine); err != nil {
			return fmt.Errorf("store: block count fold lock: %w", err)
		}
		if !mine {
			return nil
		}
		if err := tx.QueryRow(ctx, blockCountFoldSQL).Scan(&folded); err != nil {
			return fmt.Errorf("store: block count fold: %w", err)
		}
		return nil
	})
	return folded, err
}

// BlockCountDrift is one counter key the reconcile corrected.
type BlockCountDrift struct {
	Scope       string  `json:"scope"`
	Category    string  `json:"category"`
	IsArchived  bool    `json:"is_archived"`
	GuardStatus *string `json:"guard_status"`
	Counted     int64   `json:"counted"` // the counters before the correction
	Actual      int64   `json:"actual"`  // the real count, the counters after it
}

// BlockCountReconcile is the result of one reconcile pass.
type BlockCountReconcile struct {
	Scopes      int               `json:"scopes"`
	Corrections []BlockCountDrift `json:"corrections"`
	// Pruned counts folded counter rows at 0 that were removed (no drift: a
	// fold leaves zero rows standing).
	Pruned int `json:"pruned"`
	// Failed names the scopes whose reconcile failed; the pass went on with
	// the others. The errors are logged per scope and returned joined.
	Failed []string `json:"failed"`
}

// reconcileScopesSQL lists every scope that has blocks or counter rows. The
// block side is a loose index scan over idx_context_scope (one probe per
// distinct scope, not a pass over the table); the counter side catches scopes
// whose blocks are all gone but whose rows still stand.
const reconcileScopesSQL = `WITH RECURSIVE s AS (
	    (SELECT scope FROM context_blocks ORDER BY scope LIMIT 1)
	    UNION ALL
	    SELECT (SELECT b.scope FROM context_blocks b WHERE b.scope > s.scope ORDER BY b.scope LIMIT 1)
	      FROM s WHERE s.scope IS NOT NULL
	)
	SELECT scope::text FROM s WHERE scope IS NOT NULL
	UNION
	SELECT scope FROM context_block_counts
	UNION
	SELECT scope FROM context_block_count_deltas
	ORDER BY 1`

// reconcileDriftSQL compares the counters of scope $1 (folded rows plus
// deltas) with a real count of its blocks in ONE statement, i.e. one snapshot.
// A key whose sums differ is drift; a key with counter rows but neither a
// count nor blocks may hold a folded zero row to prune.
const reconcileDriftSQL = `SELECT category, is_archived, guard_status,
	       sum(counted)::bigint, sum(actual)::bigint
	  FROM (SELECT category, is_archived, guard_status, n AS counted, 0::bigint AS actual
	          FROM ` + blockCountRowsSQL + ` k WHERE scope = $1
	        UNION ALL
	        SELECT category::text, is_archived, guard_status::text, 0::bigint, count(*)
	          FROM context_blocks WHERE scope = $1
	         GROUP BY 1, 2, 3) x
	 GROUP BY 1, 2, 3
	HAVING sum(counted) <> sum(actual) OR sum(actual) = 0
	 ORDER BY 1, 2, 3`

// ReconcileBlockCounts compares the block counters with a real count of
// context_blocks, scope by scope, and corrects every difference. scope ""
// reconciles every scope; otherwise only the named one.
//
// Per scope the CHECK is one read-only statement: it reads the counters and
// counts the blocks in one snapshot, and since a writer's block change and its
// delta row commit together, that snapshot holds both or neither — a
// difference it reports is drift, not a write in flight. A scope without drift
// ends there, having locked nothing. A scope with drift is CORRECTED in a
// transaction that takes the scope's advisory lock (so two reconciles do not
// both add the difference), repeats the check in a fresh snapshot, and appends
// the difference (actual - counted) as delta rows — the same append-only
// write the triggers make. A writer whose block change that snapshot does not
// see adds its own delta after the commit, on top; nothing a writer holds is
// waited for, and no writer waits for the reconcile.
//
// A scope that fails (a statement timeout on a large scope, a lost connection)
// is logged, named in Failed, and the pass goes on with the next scope while
// ctx lives; the scope errors come back joined. Every correction is logged
// (scope, key, counted, actual) and returned.
func ReconcileBlockCounts(ctx context.Context, pool *pgxpool.Pool, scope string) (*BlockCountReconcile, error) {
	scopes := []string{scope}
	if scope == "" {
		rows, err := pool.Query(ctx, reconcileScopesSQL)
		if err != nil {
			return nil, fmt.Errorf("store: block count reconcile scopes: %w", err)
		}
		if scopes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, fmt.Errorf("store: block count reconcile scopes: %w", err)
		}
	}
	res := &BlockCountReconcile{Corrections: []BlockCountDrift{}, Failed: []string{}}
	var errs []error
	for _, s := range scopes {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		drift, pruned, err := reconcileScopeCounts(ctx, pool, s)
		if err != nil {
			slog.Warn("stats: block count reconcile failed for a scope", "scope", s, "error", err)
			res.Failed = append(res.Failed, s)
			errs = append(errs, fmt.Errorf("scope %q: %w", s, err))
			continue
		}
		res.Scopes++
		res.Pruned += pruned
		for _, d := range drift {
			slog.Warn("stats: block counter drift corrected",
				"scope", d.Scope, "category", d.Category, "is_archived", d.IsArchived,
				"guard_status", d.GuardStatus, "counted", d.Counted, "actual", d.Actual)
		}
		res.Corrections = append(res.Corrections, drift...)
	}
	return res, errors.Join(errs...)
}

// scopeDrift runs reconcileDriftSQL for one scope.
func scopeDrift(ctx context.Context, q pgxdb.Querier, scope string) ([]BlockCountDrift, error) {
	rows, err := q.Query(ctx, reconcileDriftSQL, scope)
	if err != nil {
		return nil, fmt.Errorf("store: block count drift: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BlockCountDrift, error) {
		d := BlockCountDrift{Scope: scope}
		return d, r.Scan(&d.Category, &d.IsArchived, &d.GuardStatus, &d.Counted, &d.Actual)
	})
	if err != nil {
		return nil, fmt.Errorf("store: block count drift: %w", err)
	}
	return found, nil
}

// reconcileScopeCounts checks one scope without a lock and corrects it under
// the scope's advisory lock when the check found something.
func reconcileScopeCounts(ctx context.Context, pool *pgxpool.Pool, scope string) ([]BlockCountDrift, int, error) {
	found, err := scopeDrift(ctx, pool, scope)
	if err != nil || len(found) == 0 {
		return nil, 0, err
	}
	var (
		drift  []BlockCountDrift
		pruned int
	)
	err = pgxdb.Write(ctx, pool, pgxdb.At("store: block count reconcile"), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, blockCountReconcileLockSQL, scope); err != nil {
			return fmt.Errorf("store: block count reconcile lock: %w", err)
		}
		found, err := scopeDrift(ctx, tx, scope)
		if err != nil {
			return err
		}
		drift, pruned, err = correctScopeCounts(ctx, tx, scope, found)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return drift, pruned, nil
}

// correctScopeCounts appends the differences of one scope's check as delta
// rows and removes the scope's folded zero rows.
func correctScopeCounts(ctx context.Context, tx pgx.Tx, scope string, found []BlockCountDrift) ([]BlockCountDrift, int, error) {
	drift := make([]BlockCountDrift, 0, len(found))
	for _, d := range found {
		if d.Counted == d.Actual {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_block_count_deltas (scope, category, is_archived, guard_status, n)
			 VALUES ($1, $2, $3, $4::text, $5)`,
			scope, d.Category, d.IsArchived, d.GuardStatus, d.Actual-d.Counted); err != nil {
			return nil, 0, fmt.Errorf("store: block count correct: %w", err)
		}
		drift = append(drift, d)
	}
	// A folded row at 0 adds nothing to any sum, whatever deltas are pending
	// for its key, so removing it is always exact; a fold that adds to the key
	// later inserts the row again.
	tag, err := tx.Exec(ctx, `DELETE FROM context_block_counts WHERE scope = $1 AND n = 0`, scope)
	if err != nil {
		return nil, 0, fmt.Errorf("store: block count prune: %w", err)
	}
	return drift, int(tag.RowsAffected()), nil
}
