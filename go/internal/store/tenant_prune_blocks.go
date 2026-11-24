package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// pruneLinksSQL deletes up to $2 dream links of the scope $1 — the links go
// before the blocks they name (their foreign keys are NO ACTION, see
// pruneScopeDeletes). context_dream_links has no index on scope (the
// SCALE-NAHT there), so each batch reads the table up to its limit.
const pruneLinksSQL = `DELETE FROM context_dream_links WHERE ctid IN (SELECT ctid FROM context_dream_links WHERE scope = $1 LIMIT $2)`

// pruneBlocksSQL deletes up to $2 blocks of the scope $1 (idx_context_scope);
// everything that holds a row per block follows its foreign key (CASCADE or
// SET NULL, see scope_purge.go).
const pruneBlocksSQL = `DELETE FROM context_blocks WHERE ctid IN (SELECT ctid FROM context_blocks WHERE scope = $1 LIMIT $2)`

// pruneScopeBlocks deletes the dream links and the blocks of the tenant's
// scopes (steps 1-2 of PruneTenant) in two passes over the scopes, each in
// sorted order, one batch per transaction, each transaction taking the
// scope's supersedes lock exclusively before any row (LockSupersedesScope;
// see "LOCKS" at PruneTenant):
//
//  1. every scope's dream links, batch by batch, so that no link of one of
//     the tenant's scopes still names a block when the second pass deletes
//     blocks — a block's child in another of its scopes (a scope move keeps
//     parent_id) goes with the block, and its links with it;
//  2. every scope's blocks, batch by batch; each batch deletes the scope's
//     links again first and deletes blocks only when there are none: a dream
//     batch of the scope may have written one between two batches of the
//     prune (it waits for the lock while a batch runs), and its foreign key
//     would refuse the block delete (23503) that the drain of all links
//     before all blocks used to run into in that window.
func pruneScopeBlocks(ctx context.Context, pool *pgxpool.Pool, scopes []string) error {
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	for _, scope := range sorted {
		for {
			links, err := pruneScopeBatch(ctx, pool, scope, false)
			if err != nil {
				return fmt.Errorf("store: prune context_dream_links: %w", err)
			}
			if links == 0 {
				break
			}
		}
	}
	for _, scope := range sorted {
		for {
			n, err := pruneScopeBatch(ctx, pool, scope, true)
			if err != nil {
				return fmt.Errorf("store: prune context_blocks: %w", err)
			}
			if n == 0 {
				break
			}
		}
	}
	return nil
}

// pruneScopeBatch runs one batch of pruneScopeBlocks in its own transaction:
// the scope's supersedes lock, then up to pruneBatchSize of the scope's dream
// links; with blocks set and no link left, up to pruneBatchSize of its blocks.
// It returns the rows it deleted (0 ends the pass for the scope).
func pruneScopeBatch(ctx context.Context, pool *pgxpool.Pool, scope string, blocks bool) (int64, error) {
	var n int64
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: prune batch"), func(tx pgx.Tx) error {
		if err := LockSupersedesScope(ctx, tx, scope); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, pruneLinksSQL, scope, pruneBatchSize)
		if err != nil {
			return err
		}
		if n = tag.RowsAffected(); n > 0 || !blocks {
			return nil
		}
		tag, err = tx.Exec(ctx, pruneBlocksSQL, scope, pruneBatchSize)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}
