package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ErrRunTenantGone: the tenant a server run writes for is being deleted (or
// gone) and the write wrote nothing (issue #555). A run reads its tenants at
// the start of its cycle (only active ones) and writes later; the tenant's
// delete may have begun in between. Runs skip the write: it is not an error
// of the run.
var ErrRunTenantGone = errors.New("store: the tenant this run writes for is being deleted; nothing was written")

// runTenantCtx is the context key of a server run's tenant (WithRunTenant).
type runTenantCtx struct{}

// WithRunTenant returns ctx carrying the tenant a server run acts for — the
// tenant whose scopes the run read and writes into (the scheduler's
// background tenants, a project's tenant for the forge sync). Every write of
// the run that puts a row into a scope then holds that tenant's gate
// (holdRunTenant), as a request's write holds its key's (holdRequestKey). A
// run without a tenant (the tenant-less single pass of a deployment without
// a tenant register) carries none and holds nothing.
func WithRunTenant(ctx context.Context, tenantID string) context.Context {
	if tenantID == "" {
		return ctx
	}
	return context.WithValue(ctx, runTenantCtx{}, tenantID)
}

// runTenant is the tenant WithRunTenant put into ctx, "" for none.
func runTenant(ctx context.Context) string {
	id, _ := ctx.Value(runTenantCtx{}).(string)
	return id
}

// runTenantLockSQL is a run's tenant gate: a SHARED transaction-level
// advisory lock in the two-key space of tenants. The tenant-delete's first
// step (markTenantOffboarding) takes it EXCLUSIVELY before it marks the
// tenant offboarding, so a run's write that holds it commits before that
// step — and the delete's passes, which all come after it, delete its rows —
// and a run's write after it finds the tenant no longer active and writes
// nothing (runTenantActiveSQL). The same queue as the request keys' gate
// (requestKeyLockSQL): a waiting delete is not passed by new shared holders;
// a hash collision of two tenants costs waiting, not a wrong answer.
const runTenantLockSQL = `SELECT pg_advisory_xact_lock_shared(hashtext('ctx:run-tenant'), hashtext($1::uuid::text))`

// runTenantActiveSQL is the run's decision after its gate, in a statement of
// its own (a snapshot taken after the gate was granted).
const runTenantActiveSQL = `SELECT 1 FROM context_tenants WHERE id = $1::uuid AND status = 'active'`

// runTenantExclusiveSQL is the delete's side of the gate (markTenantOffboarding).
const runTenantExclusiveSQL = `SELECT pg_advisory_xact_lock(hashtext('ctx:run-tenant'), hashtext($1::uuid::text))`

// holdRunTenant is holdRequestKey for a context without a request key: it
// takes the run tenant's gate and answers ErrRunTenantGone when the tenant is
// not active any more. Nothing for a context without a run tenant.
func holdRunTenant(ctx context.Context, tx pgx.Tx) error {
	return HoldRunTenant(ctx, tx, runTenant(ctx))
}

// HoldRunTenant takes, in tx, the gate of the tenant a server run writes for
// and answers ErrRunTenantGone when the tenant is not active any more; nothing
// for "" — holdRunTenant with the tenant named, for a run whose writes leave
// this package (the overview's persist, which runs in a worker process and
// gets its tenant through its options).
func HoldRunTenant(ctx context.Context, tx pgx.Tx, tenant string) error {
	if tenant == "" {
		return nil
	}
	b := &pgx.Batch{}
	b.Queue(runTenantLockSQL, tenant)
	b.Queue(runTenantActiveSQL, tenant)
	br := tx.SendBatch(ctx, b)
	_, lockErr := br.Exec()
	var one int
	activeErr := br.QueryRow().Scan(&one)
	closeErr := br.Close()
	switch {
	case lockErr != nil:
		return fmt.Errorf("store: run tenant gate: %w", lockErr)
	case activeErr == nil:
		return closeErr
	case pgxdb.AbsentOrMalformed(activeErr):
		return ErrRunTenantGone
	default:
		return fmt.Errorf("store: run tenant gate: %w", activeErr)
	}
}

// markTenantOffboarding is the prune's first step (issue #547): the tenant's
// status becomes 'offboarding', which ctx_auth refuses for every key, session
// and OAuth token of the tenant from the next request on (the status gate of
// migration 060), and which takes the tenant's scopes out of the background
// runs at their next cycle (Scheduler.backgroundTenants). It takes the run
// tenant's gate exclusively first (issue #555): it waits for every run write
// of the tenant that holds the gate, and a run write after it writes nothing
// (holdRunTenant). A request that authenticated before it is held by its key
// (holdRequestKey) until the key step.
func markTenantOffboarding(ctx context.Context, pool *pgxpool.Pool, tenantID string) error {
	if err := pgxdb.Write(ctx, pool, pgxdb.At("store: prune tenant: mark offboarding"), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, runTenantExclusiveSQL, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE context_tenants SET status = 'offboarding', updated_at = now() WHERE id = $1::uuid AND status <> 'offboarding'`,
			tenantID)
		return err
	}); err != nil {
		return fmt.Errorf("store: prune tenant: mark offboarding: %w", err)
	}
	return nil
}
