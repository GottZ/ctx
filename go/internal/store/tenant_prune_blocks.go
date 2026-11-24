package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// pruneLinksSQL deletes up to $2 dream links of the scopes $1 — the links go
// before the blocks they name (their foreign keys are NO ACTION, see
// pruneScopeDeletes); idx_dream_links_scope_created serves the scope.
const pruneLinksSQL = `DELETE FROM context_dream_links WHERE ctid IN (SELECT ctid FROM context_dream_links WHERE scope = ANY($1::text[]) LIMIT $2)`

// pruneBlocksSQL deletes up to $2 blocks of the scope $1 (idx_context_scope);
// everything that holds a row per block follows its foreign key (CASCADE or
// SET NULL, see scope_purge.go).
const pruneBlocksSQL = `DELETE FROM context_blocks WHERE ctid IN (SELECT ctid FROM context_blocks WHERE scope = $1 LIMIT $2)`

// pruneScopeBlocks deletes the dream links and the blocks of the tenant's
// scopes (steps 1-2 of PruneTenant), one batch per transaction, each
// transaction taking the supersedes locks of ALL the tenant's scopes
// exclusively, in sorted order, before any row (LockSupersedesScope; see
// "LOCKS" at PruneTenant):
//
//  1. the links of every scope, batch by batch;
//  2. the blocks scope by scope, batch by batch; each batch deletes the
//     links of every scope again first and deletes blocks only when there
//     are none left.
//
// All the tenant's scopes, not only the scope whose blocks the batch
// deletes: a block's child in another of the tenant's scopes (a scope move
// keeps parent_id) goes with the block through the parent_id cascade, and a
// dream batch of that other scope must neither have written a link to it in
// between (its foreign key would refuse the delete, 23503) nor run beside the
// cascade (a lock cycle, 40P01) — the review of #547 ran both. A dream batch
// waits for the lock while a batch runs and may write a link between two
// batches, which the next batch deletes first. A child in a scope of ANOTHER
// tenant is not covered: the cascade has always deleted it, without that
// scope's lock.
func pruneScopeBlocks(ctx context.Context, pool *pgxpool.Pool, scopes []string) error {
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	for {
		links, err := pruneScopeBatch(ctx, pool, sorted, "")
		if err != nil {
			return fmt.Errorf("store: prune context_dream_links: %w", err)
		}
		if links == 0 {
			break
		}
	}
	for _, scope := range sorted {
		for {
			n, err := pruneScopeBatch(ctx, pool, sorted, scope)
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
// the supersedes locks of scopes (sorted), then up to pruneBatchSize of their
// dream links; with blockScope set and no link left, up to pruneBatchSize of
// that scope's blocks. It returns the rows it deleted (0 ends the pass).
func pruneScopeBatch(ctx context.Context, pool *pgxpool.Pool, scopes []string, blockScope string) (int64, error) {
	var n int64
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: prune batch"), func(tx pgx.Tx) error {
		for _, scope := range scopes {
			if err := LockSupersedesScope(ctx, tx, scope); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, pruneLinksSQL, scopes, pruneBatchSize)
		if err != nil {
			return err
		}
		if n = tag.RowsAffected(); n > 0 || blockScope == "" {
			return nil
		}
		tag, err = tx.Exec(ctx, pruneBlocksSQL, blockScope, pruneBatchSize)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}
