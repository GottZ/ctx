package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// resolveBatchUpdateTarget resolves an update item's id within the key's
// writable scopes, as the update tool does (resolveUpdateTargetIn, the same
// wordings): an unknown id, an ambiguous or too short prefix and a key
// without a writable scope are the item's unknown_block — an id that cannot
// name a block (a NUL, invalid UTF-8, a character no id carries) is an
// unknown id, answered by the store without a query (issue #547). A failure of the
// resolve itself — its prefix query cut off by a cancelled request or a
// broken connection — is a fault, not an answer about the id (issue #547,
// from the bot review of #550): it answers 500 internal with its line, where
// it answered 422 unknown_block, as if the block did not exist.
func resolveBatchUpdateTarget(ctx context.Context, pool *pgxpool.Pool, ar *auth.AuthResult, id, reqID string) (string, *writeReject) {
	resolvedID, matches, err := store.ResolveBlockID(ctx, pool, id, writableBlockScopes(ar), nil)
	switch {
	case err == nil && resolvedID == "":
		return "", classUnknownBlock.reject("Block not found (or not in a writable scope of this key)")
	case err == nil:
		return resolvedID, nil
	case errors.Is(err, store.ErrAmbiguousID) || len(matches) > 0:
		return "", classUnknownBlock.reject(fmt.Sprintf("ambiguous id prefix (%d matches) — re-specify with a longer prefix or the full id", len(matches)))
	case errors.Is(err, store.ErrNoScopes) || errors.Is(err, store.ErrIDPrefixTooShort):
		return "", classUnknownBlock.reject(fmt.Sprintf("cannot resolve block id: %v", err))
	}
	logRequestError(ctx, "store batch: resolve update target error", "error", err, "request_id", reqID)
	return "", classInternal.reject("Internal server error")
}
