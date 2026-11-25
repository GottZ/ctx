package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ErrRequestKeyGone: the key the request authenticated with was deleted
// while the request ran, and the write wrote nothing (issue #555). The one
// path that deletes a key row is the tenant-delete's key step
// (PruneTenant), so the key's tenant is being deleted; the handlers answer
// it with 401 unauthorized, without a line of their own.
var ErrRequestKeyGone = errors.New("store: the key this request authenticated with no longer exists; nothing was written")

// requestKeyCtx is the context key of the request's key (WithRequestKey).
type requestKeyCtx struct{}

// WithRequestKey returns ctx carrying the id of the key the request
// authenticated with. The authentication middleware sets it where it puts
// the request's identity into the context (handler.Auth), so every write a
// request makes, on whatever surface, carries it; the server's own runs
// (guard, dream, digest, forge sync, …) carry none.
func WithRequestKey(ctx context.Context, apiKeyID string) context.Context {
	if apiKeyID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestKeyCtx{}, apiKeyID)
}

// requestKey is the key WithRequestKey put into ctx, "" for none.
func requestKey(ctx context.Context) string {
	id, _ := ctx.Value(requestKeyCtx{}).(string)
	return id
}

// requestKeyHoldSQL takes the key-share lock of the request's key row — the
// lock a foreign key on context_api_keys takes when a row naming the key is
// inserted (the write budget's booking, a stage, a secret, a chat session).
const requestKeyHoldSQL = `SELECT 1 FROM context_api_keys WHERE id = $1::uuid FOR KEY SHARE`

// holdRequestKey is the first statement of a write transaction of a request
// that puts a row into a scope (issue #555): it locks the request's key row
// FOR KEY SHARE and answers ErrRequestKeyGone when the row is gone. Nothing
// for a context without a key (the server's own runs).
//
// Why the key: a tenant-delete deletes every key of the tenant in its key
// step, one transaction after the passes over the tenant's scopes and before
// its closing rounds (PruneTenant), and a deleted key id never returns. The
// delete takes the row lock this key share conflicts with, so a request that
// holds its key while it writes commits before the key step does, and the
// closing rounds — which begin after it — find and delete its rows; a
// request that comes to its write after the key step finds no key and writes
// nothing. A scope with no tenant row (a legacy scope of the default
// operation) is written as before: its writers' keys exist. The scope itself
// cannot tell the two apart once the tenant row and its scope mappings are
// gone — a scope no tenant owns may never have been assigned — so the
// request's key is the mark, not the scope.
//
// The lock is the transaction's FIRST, before any supersedes scope lock or
// row: a writer that held a scope lock and then waited for its key behind the
// key step would wait for a transaction that waits for another writer of the
// tenant, which may wait for that scope lock. Held first, the key share waits
// for nothing but a key delete, and the key step holds no scope lock and no
// block row. It conflicts only with a delete of the key or a change of its
// key columns, never with the last_used_at write of every authentication
// (FOR NO KEY UPDATE).
func holdRequestKey(ctx context.Context, tx pgx.Tx) error {
	key := requestKey(ctx)
	if key == "" {
		return nil
	}
	var one int
	err := tx.QueryRow(ctx, requestKeyHoldSQL, key).Scan(&one)
	if err == nil {
		return nil
	}
	if pgxdb.AbsentOrMalformed(err) {
		return ErrRequestKeyGone
	}
	return fmt.Errorf("store: request key hold: %w", err)
}

// requestKeyForeignKeys are the foreign keys on context_api_keys a request's
// own write inserts a row through: their check takes the key-share lock
// holdRequestKey takes, so a violation of one of them is the request's key
// gone (requestKeyGoneOr).
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
