package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// pruneKeyOwnedSQL is the owner test of the key step's scope branches, as an
// array (pruneOwnedSQL: $1 = the scope names the prune read at its start, $2 =
// the tenant): the names the tenant still owns. A second delete of a tenant
// whose row the first has deleted finds none, so a key the next owner of a
// scope name holds — its home there, or the name in its lists — is neither
// deleted nor stripped ("SECOND DELETE" at PruneTenant). An uncorrelated
// sub-select: read once per statement.
const pruneKeyOwnedSQL = `ARRAY` + pruneOwnedSQL

// pruneKeyDeleteGatesSQL lists the gates (requestKeyLockSQL) of the keys the
// key step deletes (pruneKeysDeleteSQL): the tenant's keys and every key whose
// home is one of its scopes. Sorted, so two key steps that meet take their
// gates in one order.
const pruneKeyDeleteGatesSQL = `SELECT DISTINCT hashtext(id::text) AS h FROM context_api_keys
 WHERE tenant_id = $2::uuid OR home_scope = ANY(` + pruneKeyOwnedSQL + `)
 ORDER BY h`

// pruneKeyStripGatesSQL lists the gates of the keys the key step takes one of
// the tenant's scopes from and leaves standing (pruneKeysStripSQL): keys of
// other tenants that name one in their allowed or write scopes.
const pruneKeyStripGatesSQL = `SELECT DISTINCT hashtext(id::text) AS h FROM context_api_keys
 WHERE (allowed_scopes && ` + pruneKeyOwnedSQL + ` OR write_scopes && ` + pruneKeyOwnedSQL + `)
   AND tenant_id <> $2::uuid AND home_scope <> ALL(` + pruneKeyOwnedSQL + `)
 ORDER BY h`

// pruneKeyGateSQL is the key step's side of a request key's gate.
const pruneKeyGateSQL = `SELECT pg_advisory_xact_lock(hashtext('ctx:request-key'), $1::int4)`

// pruneKeysDeleteSQL deletes the keys that cannot outlive the tenant: its
// own, and every key whose home scope is one of the scopes it still owns (a
// server-admin may mint one into another tenant's scope; its home dies with
// the scope, and a key whose home names the next owner's scope would read and
// write there).
const pruneKeysDeleteSQL = `DELETE FROM context_api_keys WHERE tenant_id = $2::uuid OR home_scope = ANY(` + pruneKeyOwnedSQL + `) RETURNING id::text`

// pruneKeysStripSQL takes the scopes the tenant still owns out of the allowed
// and write scopes of every other key that names one of them: the key lives
// on, but grants nothing in a scope name the next owner may get.
const pruneKeysStripSQL = `WITH owned AS (SELECT ` + pruneKeyOwnedSQL + ` AS s)
UPDATE context_api_keys k
   SET allowed_scopes = ARRAY(SELECT s FROM unnest(k.allowed_scopes) WITH ORDINALITY AS u(s, o) WHERE s <> ALL(owned.s) ORDER BY o),
       write_scopes   = ARRAY(SELECT s FROM unnest(k.write_scopes) WITH ORDINALITY AS u(s, o) WHERE s <> ALL(owned.s) ORDER BY o),
       updated_at     = now()
  FROM owned
 WHERE (k.allowed_scopes && owned.s OR k.write_scopes && owned.s)
   AND k.tenant_id <> $2::uuid AND k.home_scope <> ALL(owned.s)`

// lockRequestKeysExclusive takes, in tx, the gate of every key gatesSQL
// lists (with args), exclusively and in sorted order. Each gate waits for the
// request writes of its key that hold it, and a request write that comes
// after waits for this transaction and then finds its key gone or its scope
// taken (holdRequestKey). A key that joins the list while the gates are taken
// (a key minted meanwhile) is locked in a further round, until a round finds
// none.
func lockRequestKeysExclusive(ctx context.Context, tx pgx.Tx, gatesSQL string, args ...any) error {
	held := map[int32]bool{}
	for {
		hs, err := collectRows(ctx, tx, pgx.RowTo[int32], gatesSQL, args...)
		if err != nil {
			return fmt.Errorf("key gates: %w", err)
		}
		fresh := 0
		for _, h := range hs {
			if held[h] {
				continue
			}
			if _, err := tx.Exec(ctx, pruneKeyGateSQL, h); err != nil {
				return fmt.Errorf("key gate: %w", err)
			}
			held[h] = true
			fresh++
		}
		if fresh == 0 {
			return nil
		}
	}
}

// collectRows runs sql in tx and collects its rows with fn.
func collectRows[T any](ctx context.Context, tx pgx.Tx, fn pgx.RowToFunc[T], sql string, args ...any) ([]T, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, fn)
}

// pruneTenantKeys is the tenant-delete's key step (steps 7b and 7, see
// PruneTenant), in two transactions, each stamped with the caller's actor
// and request id and ALWAYS via='offboarding'. First the tenant's scopes go
// out of the lists of every key of another tenant that names one (it lives
// on), under the gates of those keys only: a short transaction, so their
// writes into their own scopes wait for no more than it. Then, under the
// gates of the keys that go, the tenant's model consumers with their grants,
// limits and keys, and the knowledge keys — the tenant's and every key whose
// home is one of its scopes — with their staged writes. The key delete sets
// their access log rows' api_key_id to NULL (its foreign key), at the target
// scale millions of rows: only writers of keys that go wait for that.
func pruneTenantKeys(ctx context.Context, pool *pgxpool.Pool, tenantID string, scopes []string, stamp AuditStamp) error {
	stamp.Via = ViaOffboarding
	if len(scopes) > 0 {
		if err := pgxdb.Write(ctx, pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
			// The gates first, as of every request write (holdRequestKey).
			if err := lockRequestKeysExclusive(ctx, tx, pruneKeyStripGatesSQL, scopes, tenantID); err != nil {
				return err
			}
			if err := stampAudit(ctx, tx, stamp); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, pruneKeysStripSQL, scopes, tenantID); err != nil {
				return fmt.Errorf("key scopes: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return pgxdb.Write(ctx, pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		// The gates first: the first lock of this transaction, as of every
		// request write (holdRequestKey).
		if err := lockRequestKeysExclusive(ctx, tx, pruneKeyDeleteGatesSQL, scopes, tenantID); err != nil {
			return err
		}
		if err := stampAudit(ctx, tx, stamp); err != nil {
			return err
		}
		if err := pruneTenantModelConsumers(ctx, tx, tenantID); err != nil {
			return err
		}
		// The keys, then the staged writes of exactly the keys deleted, in a
		// statement of its own (issue #277): since migration 194 no foreign
		// key cascades a key delete onto context_pending_writes. The key
		// delete takes the row lock the key read of every stage writer
		// conflicts with (FOR KEY SHARE: a new stage, a re-arm, a consume —
		// pendingWriteKeySQL): it waits for a writer that holds a key
		// and then deletes that key, whose stage the next statement — a
		// snapshot of its own — sees as the writer left it and deletes; a
		// writer that comes after finds its key gone and writes nothing (the
		// "snapshot of its own" is READ COMMITTED, which pgxdb.Write pins). So
		// the stage delete never waits for a stage row: on this hypertable a
		// delete that waited for an update of its row deleted nothing once
		// the update committed (PostgreSQL 18.6, TimescaleDB 2.31.0), which
		// is why the re-arm and the consume hold the key too. Deleting by the
		// ids the key delete returned, not by the tenant, also takes the
		// stages of a key minted while the prune ran. The order key rows,
		// then stage rows is the one the cascade had and the one every stage
		// writer takes.
		keyIDs, err := collectRows(ctx, tx, pgx.RowTo[string], pruneKeysDeleteSQL, scopes, tenantID)
		if err != nil {
			return err
		}
		if len(keyIDs) == 0 {
			// No key, no stage (issue #547): an empty id list deletes
			// nothing and would still lock the hypertable — and wait for
			// whoever holds it.
			return nil
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM context_pending_writes WHERE api_key_id = ANY($1::uuid[])`, keyIDs); err != nil {
			return fmt.Errorf("pending writes: %w", err)
		}
		return nil
	})
}
