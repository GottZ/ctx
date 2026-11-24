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

// pruneBlocksSQL deletes up to $2 blocks of the scopes $1 (idx_context_scope);
// everything that holds a row per block follows its foreign key (CASCADE or
// SET NULL, see scope_purge.go).
const pruneBlocksSQL = `DELETE FROM context_blocks WHERE ctid IN (SELECT ctid FROM context_blocks WHERE scope = ANY($1::text[]) LIMIT $2)`

// pruneClosingRounds bounds the closing rounds of a prune (pruneTenantClose)
// that still find rows. A fixed bound, not a policy: a round finds rows only
// when a write committed in the tenant's scopes after the round before it,
// and since the prune's first step refuses the tenant's keys
// (markTenantOffboarding) only a request that authenticated before that can
// commit one — once. Rows that keep arriving round after round come from a
// writer that bypasses the gate (a loop inside the process), which a prune
// cannot outrun: it fails then, the tenant stays offboarding, and a later
// run finishes.
const pruneClosingRounds = 10

// pruneScopeBlocks deletes the dream links and the blocks of the tenant's
// scopes (steps 1-2 of PruneTenant), one batch per transaction, each
// transaction taking the supersedes locks of ALL the tenant's scopes
// exclusively, in sorted order, before any row (LockSupersedesScope; see
// "LOCKS" at PruneTenant; scopes come sorted):
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
	for {
		links, err := pruneScopeBatch(ctx, pool, scopes, nil)
		if err != nil {
			return fmt.Errorf("store: prune context_dream_links: %w", err)
		}
		if links == 0 {
			break
		}
	}
	for _, scope := range scopes {
		for {
			n, err := pruneScopeBatch(ctx, pool, scopes, []string{scope})
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

// pruneScopeBatch runs one batch of pruneScopeBlocks in its own transaction
// (pruneBatchTx). It returns the rows it deleted (0 ends the pass).
func pruneScopeBatch(ctx context.Context, pool *pgxpool.Pool, scopes, blockScopes []string) (int64, error) {
	var n int64
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: prune batch"), func(tx pgx.Tx) error {
		var err error
		n, err = pruneBatchTx(ctx, tx, scopes, blockScopes)
		return err
	})
	return n, err
}

// pruneBatchTx is one batch inside its transaction: the supersedes locks of
// scopes (sorted), then up to pruneBatchSize of their dream links; with
// blockScopes set and no link left, up to pruneBatchSize blocks of those
// scopes. It returns the rows it deleted.
func pruneBatchTx(ctx context.Context, tx pgx.Tx, scopes, blockScopes []string) (int64, error) {
	for _, scope := range scopes {
		if err := LockSupersedesScope(ctx, tx, scope); err != nil {
			return 0, err
		}
	}
	tag, err := tx.Exec(ctx, pruneLinksSQL, scopes, pruneBatchSize)
	if err != nil {
		return 0, err
	}
	if n := tag.RowsAffected(); n > 0 || len(blockScopes) == 0 {
		return n, nil
	}
	tag, err = tx.Exec(ctx, pruneBlocksSQL, blockScopes, pruneBatchSize)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// markTenantOffboarding is the prune's first step (issue #547): the tenant's
// status becomes 'offboarding', which ctx_auth refuses for every key, session
// and OAuth token of the tenant from the next request on (the status gate of
// migration 060), and which takes the tenant's scopes out of the background
// runs at their next cycle (Scheduler.backgroundTenants). So no request that
// starts after it writes into a scope the prune has passed; one that
// authenticated before it is what the closing rounds (pruneTenantClose) are
// for. The keys stay until the key step: the staged writes are deleted by the
// ids that step returns.
func markTenantOffboarding(ctx context.Context, pool *pgxpool.Pool, tenantID string) error {
	if _, err := pool.Exec(ctx,
		`UPDATE context_tenants SET status = 'offboarding', updated_at = now() WHERE id = $1::uuid AND status <> 'offboarding'`,
		tenantID); err != nil {
		return fmt.Errorf("store: prune tenant: mark offboarding: %w", err)
	}
	return nil
}

// pruneTenantClose is the prune's last step (issue #547): closing rounds over
// all the tenant's scopes, each a transaction under their supersedes locks
// (sorted), that delete what was written into them since their pass — the
// dream links, the blocks, the blobs, sources and chat sessions, up to
// pruneBatchSize each, and the secrets — and, in the round that finds
// nothing, the tenant row (ON DELETE CASCADE takes its scope mappings), so no
// row of a scope outlives a prune that reports success. A request that
// authenticated before markTenantOffboarding and writes after its scope's
// pass leaves such a row; one whose write commits after the last round's
// statements and without a supersedes lock (a single store) is not seen.
// More than pruneClosingRounds rounds with rows: an error, the tenant row
// stays.
func pruneTenantClose(ctx context.Context, pool *pgxpool.Pool, tenantID string, scopes []string) error {
	for round := 1; ; round++ {
		var left int64
		err := pgxdb.Write(ctx, pool, pgxdb.At("store: prune closing round"), func(tx pgx.Tx) error {
			var err error
			if left, err = pruneClosingRound(ctx, tx, scopes); err != nil || left > 0 {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, tenantID)
			return err
		})
		if err != nil {
			return fmt.Errorf("store: delete tenant: %w", err)
		}
		if left == 0 {
			return nil
		}
		if round >= pruneClosingRounds {
			return fmt.Errorf("store: delete tenant: rows kept arriving in its scopes for %d closing rounds (%d in the last); the tenant stays offboarding, run the delete again", round, left)
		}
	}
}

// pruneClosingRound deletes, in tx, what the closing round finds in scopes
// (sorted) and returns how many rows that was.
func pruneClosingRound(ctx context.Context, tx pgx.Tx, scopes []string) (int64, error) {
	if len(scopes) == 0 {
		return 0, nil
	}
	left, err := pruneBatchTx(ctx, tx, scopes, scopes)
	if err != nil || left > 0 {
		return left, err
	}
	for _, d := range pruneScopeDeletes {
		tag, err := tx.Exec(ctx, d.sql, scopes, pruneBatchSize)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", d.table, err)
		}
		left += tag.RowsAffected()
	}
	tag, err := tx.Exec(ctx, pruneSecretsSQL, scopes)
	if err != nil {
		return 0, fmt.Errorf("context_secrets: %w", err)
	}
	return left + tag.RowsAffected(), nil
}

// sortedScopes is scopes in the order every multi-scope holder takes their
// supersedes locks.
func sortedScopes(scopes []string) []string {
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	return sorted
}
