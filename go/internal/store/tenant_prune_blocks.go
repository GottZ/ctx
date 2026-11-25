package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// pruneLinksSQL deletes up to $2 dream links of the scopes $1 that the
// tenant $3 still owns — the links go before the blocks they name (their
// foreign keys are NO ACTION, see pruneScopeDeletes);
// idx_dream_links_scope_created serves the scope.
//
// Every scope-keyed statement of the prune names only a scope the tenant
// still owns (issue #555, "SECOND DELETE" at PruneTenant): the scope names
// the prune read at its start may belong to another tenant by now.
const pruneLinksSQL = `DELETE FROM context_dream_links WHERE ctid IN (SELECT ctid FROM context_dream_links WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $3::uuid) LIMIT $2)`

// prunePickSQL picks and locks up to $2 blocks of the scopes $1 the tenant
// $3 still owns (idx_context_scope) — the blocks a batch deletes
// (pruneBlockBatch).
const prunePickSQL = `SELECT id::text FROM context_blocks WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $3::uuid) LIMIT $2 FOR UPDATE`

// pruneChildrenSQL locks the children of one level of a batch's closure that
// lie in the tenant's scopes $2 — the rows the parent_id cascade deletes with
// the level (idx on parent_id, migration 172). The level's blocks are the
// tenant's (prunePickSQL), so this needs no owner test of its own.
const pruneChildrenSQL = `SELECT id::text FROM context_blocks WHERE parent_id = ANY($1::uuid[]) AND scope = ANY($2::text[]) FOR UPDATE`

// pruneForeignChildrenSQL reads the children of the closure $1 that lie
// OUTSIDE the tenant's scopes $2, with their scope.
const pruneForeignChildrenSQL = `SELECT id::text, scope FROM context_blocks WHERE parent_id = ANY($1::uuid[]) AND NOT scope = ANY($2::text[]) ORDER BY id`

// pruneDetachSQL detaches those children from the parent the batch deletes:
// the cascade must not take a row of another scope (issue #547).
const pruneDetachSQL = `UPDATE context_blocks SET parent_id = NULL WHERE id = ANY($1::uuid[]) AND parent_id = ANY($2::uuid[])`

// pruneClosureLinksSQL deletes the dream links that name a block of the
// closure, whatever scope the link carries (their foreign keys are NO
// ACTION), as a purge batch does.
const pruneClosureLinksSQL = `DELETE FROM context_dream_links WHERE source_block_id = ANY($1::uuid[]) OR target_block_id = ANY($1::uuid[])`

// pruneBlocksSQL deletes the batch's blocks; the rest of the closure goes
// with them through the parent_id cascade, and everything that holds a row
// per block follows its foreign key (CASCADE or SET NULL, see scope_purge.go).
const pruneBlocksSQL = `DELETE FROM context_blocks WHERE id = ANY($1::uuid[])`

// pruneClosingRounds bounds the closing rounds of a prune (pruneTenantClose)
// that still find rows. A fixed bound, not a policy: since the key step runs
// before the passes and the first step holds off the server runs (issue
// #555), a round finds rows only when a writer that holds no gate wrote into
// the tenant's scopes after the passes (an older instance during a rolling
// deploy) — a writer the prune cannot outrun: it fails then, the tenant stays
// offboarding, and a later run finishes. Before it fails, the prune counts
// what is left: a round that leaves nothing is followed by one more (up to
// twice the bound and one), and the number the error names is what is left.
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
// tenant (or of no tenant) stays: the batch detaches it from the parent it
// deletes, under that scope's lock (pruneBlockBatch).
func pruneScopeBlocks(ctx context.Context, pool *pgxpool.Pool, tenantID string, scopes []string) error {
	for {
		_, more, err := pruneScopeBatch(ctx, pool, tenantID, scopes, nil)
		if err != nil {
			return fmt.Errorf("store: prune context_dream_links: %w", err)
		}
		if !more {
			break
		}
	}
	for _, scope := range scopes {
		for {
			_, more, err := pruneScopeBatch(ctx, pool, tenantID, scopes, []string{scope})
			if err != nil {
				return fmt.Errorf("store: prune context_blocks: %w", err)
			}
			if !more {
				break
			}
		}
	}
	return nil
}

// pruneScopeBatch runs one batch of pruneScopeBlocks in its own transaction
// (pruneBatchTx), again with the scopes of another tenant's children it met
// (writeSupersedesSourceScopes). It returns the rows it deleted and whether
// the pass goes on: a batch that found fewer rows than pruneBatchSize drained
// what was there when it looked, and ends the pass (issue #555). Before, the
// pass ended only on a batch that found none — beside a writer that wrote
// into the scope faster than a batch came round (a request writer of the
// tenant that took no part in the delete: on two cores the CI's pair test
// stood 10 min in one pass, deleting the 6 to 8 rows its writers wrote again)
// it never ended. Since the key step runs before the passes, no request
// write and no run write of the tenant comes after them; the closing rounds
// (pruneTenantClose) take whatever still does.
func pruneScopeBatch(ctx context.Context, pool *pgxpool.Pool, tenantID string, scopes, blockScopes []string) (int64, bool, error) {
	var n int64
	var more bool
	err := writeSupersedesSourceScopes(ctx, pool, pgxdb.At("store: prune batch"), func(tx pgx.Tx, extra []string) error {
		var err error
		n, more, err = pruneBatchTx(ctx, tx, tenantID, scopes, blockScopes, extra)
		return err
	})
	return n, more, err
}

// pruneBatchTx is one batch inside its transaction: the supersedes locks of
// scopes and extra in one sorted pass, then up to pruneBatchSize of the
// scopes' dream links; with blockScopes set and no link left, a block batch
// of those scopes (pruneBlockBatch). It returns the rows it deleted and
// whether a further batch may find more: a full batch of links or blocks, or
// links met in a block round (their blocks come in the next).
func pruneBatchTx(ctx context.Context, tx pgx.Tx, tenantID string, scopes, blockScopes, extra []string) (int64, bool, error) {
	held := sortedScopes(append(slices.Clone(scopes), extra...))
	for _, scope := range held {
		if err := LockSupersedesScope(ctx, tx, scope); err != nil {
			return 0, false, err
		}
	}
	tag, err := tx.Exec(ctx, pruneLinksSQL, scopes, pruneBatchSize, tenantID)
	if err != nil {
		return 0, false, err
	}
	if n := tag.RowsAffected(); len(blockScopes) == 0 {
		return n, n >= pruneBatchSize, nil
	} else if n > 0 {
		return n, true, nil
	}
	n, err := pruneBlockBatch(ctx, tx, tenantID, scopes, blockScopes, held)
	return n, n >= pruneBatchSize, err
}

// pruneBlockBatch deletes up to pruneBatchSize blocks of blockScopes and
// everything below them in the tenant's scopes (issue #547, review round 1):
//
//  1. the blocks, picked and locked FOR UPDATE;
//  2. their closure in the tenant's scopes, one level per statement, each
//     locked FOR UPDATE before the next is read — the scope purge's
//     discipline (lockPurgeClosure): a new child needs the key-share lock of
//     a held parent, so the next statement sees it;
//  3. the children of the closure in any OTHER scope — another tenant's, or
//     no tenant's — are detached (parent_id = NULL) instead of going with
//     the cascade: before, the prune deleted them and their subtrees, and a
//     dream link of their scope onto one of them made every run fail with
//     23503. The detach writes rows of that scope, so it runs under that
//     scope's supersedes lock; a scope the batch does not hold sends it
//     through a run of its own with the lock in the sorted pass
//     (scopesToLockError). A scope purge refuses such a closure instead
//     (ErrPurgeChildOutsideScope) — a purge names one scope and may refuse;
//     an archive cascades nothing. Neither removes a row of another scope;
//  4. the dream links that name a block of the closure, whatever their scope;
//  5. the blocks — the cascade takes the rest of the closure.
//
// It returns the blocks of step 1 (0 ends the pass).
func pruneBlockBatch(ctx context.Context, tx pgx.Tx, tenantID string, scopes, blockScopes, held []string) (int64, error) {
	ids, err := collectStrings(ctx, tx, prunePickSQL, blockScopes, pruneBatchSize, tenantID)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	closure := slices.Clone(ids)
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	for level := ids; len(level) > 0; {
		kids, err := collectStrings(ctx, tx, pruneChildrenSQL, level, scopes)
		if err != nil {
			return 0, err
		}
		level = nil
		for _, k := range kids {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				level = append(level, k)
			}
		}
		closure = append(closure, level...)
	}
	if err := pruneDetachForeignChildren(ctx, tx, closure, scopes, held); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, pruneClosureLinksSQL, closure); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, pruneBlocksSQL, ids)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// pruneDetachForeignChildren is step 3 of pruneBlockBatch: the children of
// closure outside the tenant's scopes, detached under their scopes' locks;
// a scope not in held ends the run with a scopesToLockError naming it.
func pruneDetachForeignChildren(ctx context.Context, tx pgx.Tx, closure, scopes, held []string) error {
	rows, err := tx.Query(ctx, pruneForeignChildrenSQL, closure, scopes)
	if err != nil {
		return err
	}
	type child struct{ ID, Scope string }
	kids, err := pgx.CollectRows(rows, pgx.RowToStructByPos[child])
	if err != nil || len(kids) == 0 {
		return err
	}
	var missing, detach []string
	for _, k := range kids {
		if !slices.Contains(held, k.Scope) && !slices.Contains(missing, k.Scope) {
			missing = append(missing, k.Scope)
		}
		detach = append(detach, k.ID)
	}
	if len(missing) > 0 {
		return &scopesToLockError{scopes: missing}
	}
	_, err = tx.Exec(ctx, pruneDetachSQL, detach, closure)
	return err
}

// pruneTenantClose is the prune's last step (issue #547): closing rounds over
// all the tenant's scopes, each a transaction under their supersedes locks
// (sorted), stamped as the key step is, that delete what was written into
// them since their pass — the dream links, the blocks, the blobs, sources,
// chat sessions and write log, up to pruneBatchSize each, and the sweeps
// (pruneScopeSweeps) — and, in the round that finds nothing, the block
// counters of the scopes and the queued reconciles of the tenant
// (pruneTenantLast) and the tenant row (ON DELETE CASCADE takes its scope
// mappings and grants), so no row of a scope outlives a prune that reports
// success. Since the key step and the run gate come before the passes (issue
// #555), only a writer that holds no gate still writes then.
// More than pruneClosingRounds rounds with rows: an error, the tenant row
// stays.
func pruneTenantClose(ctx context.Context, pool *pgxpool.Pool, tenantID string, scopes []string, stamp AuditStamp) error {
	for round := 1; ; round++ {
		var left int64
		err := writeSupersedesSourceScopes(ctx, pool, pgxdb.At("store: prune closing round"), func(tx pgx.Tx, extra []string) error {
			// Stamped (no lock): the round's configuration deletes and the
			// tenant row's cascade write audit rows that name the offboarding.
			if err := stampAudit(ctx, tx, stamp); err != nil {
				return err
			}
			var err error
			if left, err = pruneClosingRound(ctx, tx, tenantID, scopes, extra); err != nil || left > 0 {
				return err
			}
			if err := pruneTenantLast(ctx, tx, tenantID, scopes); err != nil {
				return err
			}
			if err := pruneNoLateScope(ctx, tx, tenantID, scopes); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, tenantID)
			if err == nil && tag.RowsAffected() == 0 {
				// Another delete of the tenant came first (issue #555).
				return ErrTenantNotFound
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("store: delete tenant: %w", err)
		}
		if left == 0 {
			return nil
		}
		if round < pruneClosingRounds {
			continue
		}
		remaining, err := pruneRowsLeft(ctx, pool, tenantID, scopes)
		if err != nil {
			return fmt.Errorf("store: delete tenant: count what is left: %w", err)
		}
		if remaining == 0 && round <= 2*pruneClosingRounds {
			// The round drained what was there (a backlog, or one more row of a
			// writer that holds no gate): the next finds nothing and deletes
			// the tenant. Bounded, so a writer that writes one row per round
			// cannot keep the delete going — the round after the last of the
			// bound included: it deletes the tenant only when it finds nothing.
			continue
		}
		return fmt.Errorf("store: delete tenant: rows kept arriving in its scopes for %d closing rounds (the last deleted %d, %d are left now); the tenant stays offboarding, run the delete again", round, left, remaining)
	}
}

// pruneLateScopesSQL counts the scopes of the tenant $1 that are not among the
// names $2 the prune read at its start.
const pruneLateScopesSQL = `SELECT count(*)::int FROM context_tenant_scopes WHERE tenant_id = $1::uuid AND scope <> ALL($2::text[])`

// pruneNoLateScope keeps the tenant row's delete from cascading away the
// mapping of a scope the prune never worked on (issue #555): its rows would
// stay in a scope no tenant owns, and the next tenant assigned the name would
// own them. AssignTenantScopeTx refuses an offboarding tenant, so only a
// writer that does not ask adds such a scope — an older instance during a
// rolling deploy, or an assign after an operator set the tenant's status back
// while its delete ran. The tenant row is locked first, as every assign locks
// it, and the count is a statement of its own: it sees every assign that
// committed before the lock was granted, and none can come after. The delete
// fails then and the tenant stays offboarding; the next delete reads the
// scope in its list.
func pruneNoLateScope(ctx context.Context, tx pgx.Tx, tenantID string, scopes []string) error {
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM context_tenants WHERE id = $1::uuid FOR UPDATE`, tenantID).Scan(&one); err != nil {
		if pgxdb.AbsentOrMalformed(err) {
			// Another delete of the tenant came first.
			return ErrTenantNotFound
		}
		return err
	}
	var late int
	if err := tx.QueryRow(ctx, pruneLateScopesSQL, tenantID, scopes).Scan(&late); err != nil {
		return err
	}
	if late > 0 {
		return fmt.Errorf("%d scope(s) were assigned to the tenant while its delete ran; the tenant stays offboarding, run the delete again", late)
	}
	return nil
}

// pruneRowsLeftSQL counts the rows of every table the closing rounds drain
// that lie in the scopes $1 the tenant $2 still owns.
const pruneRowsLeftSQL = `SELECT
	  (SELECT count(*) FROM context_dream_links WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $2::uuid))
	+ (SELECT count(*) FROM context_blocks WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $2::uuid))
	+ (SELECT count(*) FROM context_blobs WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $2::uuid))
	+ (SELECT count(*) FROM context_sources WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $2::uuid))
	+ (SELECT count(*) FROM context_chat_sessions WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $2::uuid))
	+ (SELECT count(*) FROM context_secrets WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $2::uuid))
	+ (SELECT count(*) FROM context_write_log WHERE scope = ANY($1::text[]) AND scope IN (SELECT ts.scope FROM context_tenant_scopes ts WHERE ts.tenant_id = $2::uuid))`

// pruneRowsLeft is what the closing rounds would still find (pruneRowsLeftSQL).
func pruneRowsLeft(ctx context.Context, pool *pgxpool.Pool, tenantID string, scopes []string) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, pruneRowsLeftSQL, scopes, tenantID).Scan(&n)
	return n, err
}

// pruneClosingRound deletes, in tx, what the closing round finds in scopes
// (sorted; extra: the scopes of another tenant's children an earlier run met)
// and returns how many rows that was.
func pruneClosingRound(ctx context.Context, tx pgx.Tx, tenantID string, scopes, extra []string) (int64, error) {
	if len(scopes) == 0 {
		return 0, nil
	}
	left, _, err := pruneBatchTx(ctx, tx, tenantID, scopes, scopes, extra)
	if err != nil || left > 0 {
		return left, err
	}
	for _, d := range pruneScopeDeletes {
		tag, err := tx.Exec(ctx, d.sql, scopes, pruneBatchSize, tenantID)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", d.table, err)
		}
		left += tag.RowsAffected()
	}
	n, err := pruneSweep(ctx, tx, tenantID, scopes)
	return left + n, err
}

// sortedScopes is scopes in the order every multi-scope holder takes their
// supersedes locks.
func sortedScopes(scopes []string) []string {
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	return sorted
}
