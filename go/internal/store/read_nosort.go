package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// readNoSort turns sorts off for the statements of a read that walks an
// index in key order and stops after its page (issue #536), in the read-only
// transaction of readNoSortTx: the pages of the workflow lists
// (WorkflowStatusListSQL, IssueCreatedListSQL). Every other path to their
// order — a bitmap scan of the same index and a sort of every row it admits,
// a walk of idx_context_created through the corpus with an incremental sort —
// needs a sort, and with sorts off the planner takes a path without a
// disabled node whenever one exists (PostgreSQL 18 compares the count of
// disabled nodes before the cost; the embed-excluded batches do the same,
// embedExcludedBatchNoSort). The planner estimates the rows of a (scope,
// type, status) as if the three were independent, so a large set looks like
// a few dozen rows, and at random_page_cost 4 a sort of them looked cheaper
// than the walk: the status list read and sorted every issue of the status
// (552 buffers for 3 334 rows on the W6 store, a page of 50), the created
// list's custom plan walked idx_context_created from the newest block of the
// corpus (3 295 buffers at 100 000 issues). The setting ends with the
// transaction.
const readNoSort = "SET LOCAL enable_sort = off; SET LOCAL enable_incremental_sort = off"

// readNoSortTxOptions: a read-only transaction at READ COMMITTED, so each
// statement reads its own snapshot as it did on the pool.
var readNoSortTxOptions = pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly}

// readNoSortTx runs fn in a read-only transaction with sorts off
// (readNoSort).
func readNoSortTx(ctx context.Context, pool *pgxpool.Pool, what string, fn func(tx pgx.Tx) error) error {
	return readTx(ctx, pool, what, readNoSort, fn)
}

// readTx runs fn in a read-only transaction (readNoSortTxOptions) after the
// planner settings plan (SET LOCAL statements, ending with the transaction).
func readTx(ctx context.Context, pool *pgxpool.Pool, what, plan string, fn func(tx pgx.Tx) error) error {
	return pgxdb.WriteOpts(ctx, pool, readNoSortTxOptions, pgxdb.At(what), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, plan); err != nil {
			return fmt.Errorf("%s: planner settings: %w", what, err)
		}
		return fn(tx)
	})
}
