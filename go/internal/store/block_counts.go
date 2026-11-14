package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// context_block_counts (migration 170, issue #266) holds one row per
// (scope, category, is_archived, guard_status) with the number of blocks
// carrying that key. Statement-level triggers on context_blocks keep it in the
// writer's transaction; GetStats, GetGuardStats, ListCategories and
// ActiveCategoryCount sum it instead of counting context_blocks. The reconcile
// below compares it with a real count per scope and corrects drift — the
// repair for writes the triggers cannot see (TRUNCATE is covered; ALTER TABLE
// … DISABLE TRIGGER and session_replication_role = replica are not).

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

// blockCountReconcileAttempts bounds the retries of one scope's reconcile
// transaction after a serialization failure (a writer changed one of the
// scope's counter rows after the reconcile's snapshot). A scope that still
// conflicts after these attempts is skipped with a warning; the next pass
// takes it again. A fixed constant: it bounds a retry loop, it is no policy.
const blockCountReconcileAttempts = 3

// errReconcileConflict marks a scope whose reconcile kept losing against
// concurrent writers.
var errReconcileConflict = errors.New("store: block count reconcile: scope kept conflicting with writers")

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
// its blocks in ONE snapshot. A key whose sums differ is drift; a key with a
// counter row but neither a count nor blocks is a zero row to prune.
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
// Correct against concurrent writes without blocking them: each scope runs in
// its own REPEATABLE READ transaction, which reads the counter rows and counts
// the blocks in the same snapshot — a writer's block change and its counter
// delta commit together, so the snapshot holds both or neither. A correction
// is applied as a DELTA (actual - counted) onto the row, not as an absolute
// value: a write that commits after the snapshot and before the correction
// changes that row, and REPEATABLE READ refuses to update a row changed after
// the snapshot (40001) — the scope's transaction is then retried with a fresh
// snapshot. A write still in flight applies its own delta after the
// correction, on top of the corrected value. Nothing is locked beyond the
// counter rows being corrected; a scope without drift is a read.
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
		if errors.Is(err, errReconcileConflict) {
			slog.Warn("stats: block count reconcile skipped a scope under write conflicts",
				"scope", s, "attempts", blockCountReconcileAttempts)
			continue
		}
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

// reconcileScopeCounts reconciles one scope, retrying its transaction after a
// serialization failure.
func reconcileScopeCounts(ctx context.Context, pool *pgxpool.Pool, scope string) ([]BlockCountDrift, int, error) {
	for attempt := 1; ; attempt++ {
		var (
			drift  []BlockCountDrift
			pruned int
		)
		err := pgxdb.WriteOpts(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead},
			pgxdb.At("store: block count reconcile"), func(tx pgx.Tx) error {
				var err error
				drift, pruned, err = reconcileScopeTx(ctx, tx, scope)
				return err
			})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40001" {
			if attempt < blockCountReconcileAttempts {
				continue
			}
			return nil, 0, errReconcileConflict
		}
		if err != nil {
			return nil, 0, err
		}
		return drift, pruned, nil
	}
}

// reconcileScopeTx is one scope's snapshot comparison and correction.
func reconcileScopeTx(ctx context.Context, tx pgx.Tx, scope string) ([]BlockCountDrift, int, error) {
	rows, err := tx.Query(ctx, reconcileDriftSQL, scope)
	if err != nil {
		return nil, 0, fmt.Errorf("store: block count drift: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BlockCountDrift, error) {
		d := BlockCountDrift{Scope: scope}
		return d, r.Scan(&d.Category, &d.IsArchived, &d.GuardStatus, &d.Counted, &d.Actual)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("store: block count drift: %w", err)
	}
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
