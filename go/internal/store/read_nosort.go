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
// (WorkflowStatusListSQL, IssueCreatedListSQL) and FullGraph's flat seed
// (fullGraphSeedSQL). Every other path to their order — a bitmap scan of the
// same index or of the scope and a sort of every row it admits, a walk of
// idx_context_created through the corpus with an incremental sort, a walk of
// a large read scope's whole range on idx_context_scope and a sort of it —
// needs a sort, and with sorts off the planner takes a path without a
// disabled node whenever one exists (PostgreSQL 18 compares the count of
// disabled nodes before the cost; the embed-excluded batches do the same,
// embedExcludedBatchNoSort). The planner estimates the rows of a (scope,
// type, status) as if the three were independent, and those of a scope the
// seed states through its LATERAL as a range of the table's average size, so
// a large set looks like a few dozen or a few thousand rows, and at
// random_page_cost 4 a sort of them looked cheaper than the walk: the status
// list read and sorted every issue of the status (552 buffers for 3 334 rows
// on the W6 store, a page of 50), the created list's custom plan walked
// idx_context_created from the newest block of the corpus (3 295 buffers at
// 100 000 issues), the seed read a read scope of 100 000 blocks whole (5 657
// buffers for a page of 200, 37 on the walk). The sorts a statement needs on
// top of its walk (the merge of the seed's arms) stay: no plan is without
// them. The setting ends with the transaction.
const readNoSort = "SET LOCAL enable_sort = off; SET LOCAL enable_incremental_sort = off"

// readNoSortTxOptions: a read-only transaction at READ COMMITTED, so each
// statement reads its own snapshot as it did on the pool.
var readNoSortTxOptions = pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly}

// readNoSortTx runs fn in a read-only transaction with sorts off
// (readNoSort).
func readNoSortTx(ctx context.Context, pool *pgxpool.Pool, what string, fn func(tx pgx.Tx) error) error {
	return pgxdb.WriteOpts(ctx, pool, readNoSortTxOptions, pgxdb.At(what), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, readNoSort); err != nil {
			return fmt.Errorf("%s: sorts off: %w", what, err)
		}
		return fn(tx)
	})
}
