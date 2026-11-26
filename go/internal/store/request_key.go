package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ErrRequestKeyGone: the key the request authenticated with was deleted, or
// lost a scope it granted at the authentication, while the request ran, and
// the write wrote nothing (issue #555). The paths that delete a key row or
// take a scope from it are the tenant-delete's key step (PruneTenant) and a
// key edit; the handlers answer it with 401 unauthorized, without a line of
// their own.
var ErrRequestKeyGone = errors.New("store: the key this request authenticated with no longer exists or no longer grants its scopes; nothing was written")

// RequestKey is what a request's context carries for its writes
// (WithRequestKey): the key it authenticated with and the scope lists that
// key granted at the authentication — the lists every write gate of the
// request evaluates (handler.writableBlockScopes).
type RequestKey struct {
	ID            string
	HomeScope     string
	AllowedScopes []string
	WriteScopes   []string
}

// requestKeyCtx is the context key of the request's key (WithRequestKey).
type requestKeyCtx struct{}

// WithRequestKey returns ctx carrying the key the request authenticated with.
// The authentication middleware sets it where it puts the request's identity
// into the context (handler.Auth), so every write a request makes, on
// whatever surface, carries it; the server's own runs carry none (they carry
// their tenant, WithRunTenant).
func WithRequestKey(ctx context.Context, key RequestKey) context.Context {
	if key.ID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestKeyCtx{}, key)
}

// requestKey is the key WithRequestKey put into ctx; ok false for none.
func requestKey(ctx context.Context) (RequestKey, bool) {
	k, ok := ctx.Value(requestKeyCtx{}).(RequestKey)
	return k, ok
}

// requestKeyLockSQL takes the request key's gate: a SHARED transaction-level
// advisory lock in the two-key space of request keys. The tenant-delete's key
// step takes the same lock EXCLUSIVELY for every key it deletes or takes a
// scope from (lockRequestKeysExclusive), before it changes them.
//
// Why an advisory lock and not the key row (FOR KEY SHARE, the first form of
// #555): every authentication writes the key row (ctx_auth_by_id's
// last_used_at), and a tuple that carries key-share lockers and an updater at
// once needs a MultiXact — 64 writers of one key, each holding the share for
// its whole transaction, ran at a quarter of the base's writes per second and
// wrote 55 684 MultiXacts in 20 s. The advisory lock lives in the lock
// manager only: shared holders never wait for each other, nothing writes the
// tuple, and the lock manager queues a new shared request behind a waiting
// exclusive one, so the key step cannot starve behind a stream of
// overlapping writers (a tuple lock lets a compatible newcomer pass a
// waiting deleter). Advisory locks are database-wide, so the writers and the
// key step of every ctxd instance at one database meet in the same queue.
//
// hashtext maps two keys to one lock now and then (32 bits): such a pair
// waits for each other's key step, which is time, never a wrong answer — the
// writer decides by its own key row (requestKeyGrantsSQL).
const requestKeyLockSQL = `SELECT pg_advisory_xact_lock_shared(hashtext('ctx:request-key'), hashtext($1::uuid::text))`

// requestKeyGrantsSQL is the writer's decision, a statement of its own after
// the gate (a statement's snapshot is taken when it starts; under READ
// COMMITTED one taken after the gate was granted sees a key step that
// committed before): the key row is still there and still grants every scope
// it granted at the authentication. A key step that held the gate before has
// deleted the key or taken a scope from its lists; while the writer holds the
// gate no key step can.
const requestKeyGrantsSQL = `SELECT 1 FROM context_api_keys
 WHERE id = $1::uuid
   AND home_scope = $2
   AND COALESCE(allowed_scopes, '{}') @> COALESCE($3::text[], '{}')
   AND COALESCE(write_scopes, '{}') @> COALESCE($4::text[], '{}')`

// holdRequestKey is the first statement of a write transaction of a request
// that puts a row into a scope (issue #555): it takes the request key's gate
// (requestKeyLockSQL) and answers ErrRequestKeyGone when the key is gone or
// no longer grants what it granted at the authentication. A context without
// a request key holds its run's tenant instead (holdRunTenant), and nothing
// when it carries neither.
//
// Why the key: a tenant-delete deletes every key of the tenant, and takes the
// tenant's scopes from every other key that names one, in its key step — the
// first step after the tenant's door closes (markTenantOffboarding) and
// before any of its data passes (PruneTenant). A request write that holds its
// gate makes the key step wait and commits before it, so the passes after it
// delete its rows; a request write after the key step finds its key gone or
// its scope taken and writes nothing. A scope with no tenant row (a legacy
// scope of the default operation) is written as before: its writers' keys
// exist. The scope itself cannot tell the two apart once the tenant row and
// its scope mappings are gone — a scope no tenant owns may never have been
// assigned — so the request's key is the mark, not the scope.
//
// The gate is the transaction's FIRST lock, before any supersedes scope lock
// or row: a writer that held a scope lock or a row and then waited for its
// gate behind the key step would wait for a step that waits for another
// writer of the tenant, which may wait for that lock or row. Taken first, the
// gate waits for nothing but a key step, and the key step holds no scope lock
// and no block row. A transaction takes at most one gate (its request's key),
// so no two gates are ever taken in two orders.
func holdRequestKey(ctx context.Context, tx pgx.Tx) error {
	key, ok := requestKey(ctx)
	if !ok {
		return holdRunTenant(ctx, tx)
	}
	b := &pgx.Batch{}
	b.Queue(requestKeyLockSQL, key.ID)
	b.Queue(requestKeyGrantsSQL, key.ID, key.HomeScope, key.AllowedScopes, key.WriteScopes)
	br := tx.SendBatch(ctx, b)
	_, lockErr := br.Exec()
	var one int
	grantsErr := br.QueryRow().Scan(&one)
	closeErr := br.Close()
	switch {
	case lockErr != nil:
		return fmt.Errorf("store: request key gate: %w", lockErr)
	case grantsErr == nil:
		return closeErr
	case pgxdb.AbsentOrMalformed(grantsErr):
		return ErrRequestKeyGone
	default:
		return fmt.Errorf("store: request key gate: %w", grantsErr)
	}
}

// requestKeyForeignKeys are the foreign keys on context_api_keys a request's
// own write inserts a row through. A write that holds its gate first cannot
// meet a deleted key there; a violation of one of them on a path without the
// gate (the access log of a read) is the request's key gone all the same
// (requestKeyGoneOr).
var requestKeyForeignKeys = map[string]bool{
	"context_access_log_api_key_id_fkey":    true, // the write budget's booking, LogAccess, LogAccessRef
	"context_secrets_created_by_fkey":       true, // PutSecret
	"context_secrets_rotated_by_fkey":       true, // PutSecret over an existing secret
	"context_chat_sessions_created_by_fkey": true, // CreateSession
}

// requestKeyGoneOr is ErrRequestKeyGone for err when it is the violation of
// one of requestKeyForeignKeys (23503: the key row the insert names is gone),
// and err otherwise.
func requestKeyGoneOr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" && requestKeyForeignKeys[pgErr.ConstraintName] {
		return ErrRequestKeyGone
	}
	return err
}
