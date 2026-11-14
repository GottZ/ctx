package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// context_block_counts (migration 170, issue #266) holds one row per
// (scope, category, is_archived, guard_status) with the number of blocks
// carrying that key. Statement-level triggers on context_blocks keep it in the
// writer's transaction; GetStats, GetGuardStats, ListCategories,
// ActiveBlockCount and ActiveCategoryCount sum it instead of counting
// context_blocks. The reconcile below compares it with a real count per scope
// and corrects drift — the repair for writes the triggers cannot see
// (TRUNCATE is covered; ALTER TABLE … DISABLE TRIGGER and
// session_replication_role = replica are not).

// BlockCountDrift is one counter row the reconcile corrected.
type BlockCountDrift struct {
	Scope       string  `json:"scope"`
	Category    string  `json:"category"`
	IsArchived  bool    `json:"is_archived"`
	GuardStatus *string `json:"guard_status"`
	Counted     int64   `json:"counted"` // the counter before the correction
	Actual      int64   `json:"actual"`  // the real count, the counter after it
}

// BlockCountReconcile is the result of one reconcile pass.
type BlockCountReconcile struct {
	Scopes      int               `json:"scopes"`
	Corrections []BlockCountDrift `json:"corrections"`
	// Pruned counts counter rows that stood at 0 with no block behind them
	// and were removed (no drift: the triggers leave zero rows standing).
	Pruned int `json:"pruned"`
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
	ORDER BY 1`

// reconcileDriftSQL compares the counter rows of scope $1 with a real count of
// its blocks in ONE statement, i.e. one snapshot. A key whose sums differ is
// drift; a key with a counter row but neither a count nor blocks is a zero row
// to prune.
const reconcileDriftSQL = `SELECT category, is_archived, guard_status,
	       sum(counted)::bigint, sum(actual)::bigint
	  FROM (SELECT category, is_archived, guard_status, n AS counted, 0::bigint AS actual
	          FROM context_block_counts WHERE scope = $1
	        UNION ALL
	        SELECT category::text, is_archived, guard_status::text, 0::bigint, count(*)
	          FROM context_blocks WHERE scope = $1
	         GROUP BY 1, 2, 3) x
	 GROUP BY 1, 2, 3
	HAVING sum(counted) <> sum(actual) OR sum(actual) = 0
	 ORDER BY 1, 2, 3`

// ReconcileBlockCounts compares context_block_counts with a real count of
// context_blocks, scope by scope, and corrects every difference. scope ""
// reconciles every scope; otherwise only the named one.
//
// Two steps per scope. The CHECK is one read-only statement: it reads the
// counter rows and counts the blocks in one snapshot, and since a writer's
// block change and its counter delta commit together, that snapshot holds both
// or neither — a difference it reports is drift, not a write in flight. A
// scope without drift ends there, having locked nothing. A scope with drift is
// CORRECTED in a transaction that first takes the write-ordering row
// (context_guard_state, FOR NO KEY UPDATE — the row every counter-writing
// statement takes before its first counter row, migration 170) and then
// repeats the check: while the reconcile holds that row no committed counter
// can change, so the second snapshot is exact, and a writer whose block
// change it does not see yet applies its own delta after the commit, onto the
// corrected value. Writers wait for the correction — the length of one
// recount of the scope, and only for scopes with drift. Two reconciles of the
// same scope serialize on the row as well.
//
// Every correction is logged (scope, key, counted, actual) and returned.
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
	res := &BlockCountReconcile{Corrections: []BlockCountDrift{}}
	for _, s := range scopes {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		drift, pruned, err := reconcileScopeCounts(ctx, pool, s)
		if err != nil {
			return res, err
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
	return res, nil
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
// the write-ordering row when the check found something.
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
		if _, err := tx.Exec(ctx, `SELECT 1 FROM context_guard_state WHERE id FOR NO KEY UPDATE`); err != nil {
			return fmt.Errorf("store: block count reconcile order row: %w", err)
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

// correctScopeCounts applies the differences of one scope's check. The
// counters cannot move while the caller holds the write-ordering row, so the
// delta (actual - counted) lands exactly on the value the check read.
func correctScopeCounts(ctx context.Context, tx pgx.Tx, scope string, found []BlockCountDrift) ([]BlockCountDrift, int, error) {
	drift := make([]BlockCountDrift, 0, len(found))
	pruned := 0
	for _, d := range found {
		if d.Actual == 0 {
			if _, err := tx.Exec(ctx,
				`DELETE FROM context_block_counts
				  WHERE scope = $1 AND category = $2 AND is_archived = $3
				    AND guard_status IS NOT DISTINCT FROM $4::text`,
				scope, d.Category, d.IsArchived, d.GuardStatus); err != nil {
				return nil, 0, fmt.Errorf("store: block count prune: %w", err)
			}
		} else if _, err := tx.Exec(ctx,
			`INSERT INTO context_block_counts AS c (scope, category, is_archived, guard_status, n)
			 VALUES ($1, $2, $3, $4::text, $5)
			 ON CONFLICT (scope, category, is_archived, guard_status)
			 DO UPDATE SET n = c.n + EXCLUDED.n`,
			scope, d.Category, d.IsArchived, d.GuardStatus, d.Actual-d.Counted); err != nil {
			return nil, 0, fmt.Errorf("store: block count correct: %w", err)
		}
		if d.Counted == d.Actual {
			pruned++
			continue
		}
		drift = append(drift, d)
	}
	return drift, pruned, nil
}
