package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

// supersedesNotDeclarableMsg is the ONE answer to every `supersedes` id a
// write cannot name — unknown, archived, outside the write's scope, malformed,
// the written block itself — and deliberately the same sentence for all of
// them, without the id: told apart, they would answer "does this uuid exist"
// and "is it yours" (parentNotLinkableMsg, the same rule for parent_id).
const supersedesNotDeclarableMsg = "supersedes: no block of this write's scope with that id — unknown, " +
	"archived, foreign-scope, malformed and self-referencing ids answer alike, so the field is no " +
	"existence oracle"

// supersedesReject renders a refused supersedes declaration (issue #197). nil
// when err is no such refusal. A refusal is the client being told no, not a
// server fault, and is not logged at error level (upsertFailureReject).
func supersedesReject(err error) *writeReject {
	if errors.Is(err, store.ErrSupersedesTarget) {
		return classUnknownSupersedes.reject(supersedesNotDeclarableMsg)
	}
	var c *store.SupersedesConflictError
	if !errors.As(err, &c) {
		return nil
	}
	switch c.Kind {
	case store.SupersedesSuperseded:
		if c.Head == "" {
			return classSupersedesConflict.reject(fmt.Sprintf(
				"supersedes: block %s is already superseded by another block; the chain above it is %d links or longer, so no head is named — follow superseded_by to the current head and supersede that block instead",
				c.Target, store.SupersedesChainDepth))
		}
		return classSupersedesConflict.reject(fmt.Sprintf(
			"supersedes: block %s is already superseded by another block; the current head of its chain is %s — supersede that block instead",
			c.Target, c.Head))
	case store.SupersedesCycle:
		return classSupersedesConflict.reject(fmt.Sprintf(
			"supersedes: block %s already supersedes this block, directly or through a chain (walked up to %d links) — the declaration would close a cycle",
			c.Target, store.SupersedesChainDepth))
	default:
		return classSupersedesNotAllowed.reject(fmt.Sprintf(
			"supersedes: block %s cannot be superseded — chunk rows and blocks of a type outside the dream loop (dream.linkable=false) keep their lifecycle",
			c.Target))
	}
}

// upsertStoreBlock is the upsert of the store surfaces (REST /api/store, MCP
// store direct, the confirm of a staged store): store.UpsertBlock, or — when
// the write names a block it supersedes — store.UpsertBlockSupersedes, which
// applies the declaration in the write's own transaction. Without the field
// the call is UpsertBlock's, byte for byte.
func upsertStoreBlock(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, apiKeyID string,
	category, title, content string, tags []string, metadata map[string]any, scope string, scopeExplicit bool,
	sens store.SensitivityWrite, typeName, supersedes string,
) (*store.Block, error) {
	if supersedes == "" {
		return store.UpsertBlock(ctx, pool, category, title, content, tags, metadata, scope, scopeExplicit, sens, typeName)
	}
	return store.UpsertBlockSupersedes(ctx, pool, category, title, content, tags, metadata, scope, scopeExplicit, sens, typeName,
		store.SupersedesDeclaration{TargetID: supersedes, APIKeyID: apiKeyID, Types: set})
}
