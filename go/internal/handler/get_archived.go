// Issue #201: the by-id block read of the two get surfaces — MCP `get` and REST
// manage `get` (which the CLI `ctx get` posts) — with the opt-in read of an
// ARCHIVED block. Both surfaces read the same thing, so they answer from one
// function and refuse with one text.
package handler

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

// archivedReadNeedsFullIDMsg refuses `include_archived` with an id prefix. The
// prefix path (store.ResolveBlockID) never resolves to an archived row, and
// widening it would turn a prefix into a listing of archived ids; a citation
// carries the full id anyway. The refusal depends on the argument's shape only,
// never on what is stored — no existence signal.
const archivedReadNeedsFullIDMsg = "include_archived requires the full block id; a prefix never resolves to an archived block"

// archivedReadRefused reports whether a get request must be refused before any
// lookup: include_archived with anything but a full block id.
func archivedReadRefused(id string, includeArchived bool) bool {
	return includeArchived && !store.IsFullUUID(id)
}

// ownReadScopes is the part of the key's read scopes that is the key's own:
// its home scope and allowed scopes, without the scopes another tenant granted
// to its tenant (ReadScopes = home ++ allowed ++ cross-tenant grants, ctx_auth).
// Intersected with ReadScopes, so the auth function's filtering of system
// scopes still applies. A grant — of one block or of a whole scope — shares
// what its owner keeps live; it never keeps readable what the owner archived.
func ownReadScopes(ar *auth.AuthResult) []string {
	own := make([]string, 0, len(ar.ReadScopes))
	for _, s := range ar.ReadScopes {
		if s == ar.HomeScope || contains(ar.AllowedScopes, s) {
			own = append(own, s)
		}
	}
	return own
}

// readBlockForGet answers a get by resolved id. Without includeArchived it is
// store.GetBlock, unchanged: live blocks of the read scopes plus the grant arm.
// With it, a miss there falls through to store.GetArchivedBlock — archived
// rows of the key's own scopes only (ownReadScopes: no block grant, no scope
// grant), answered with archived: true. Unknown, foreign and (without the
// opt-in) archived ids all come back nil, so each surface answers them with one
// text.
func readBlockForGet(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, ar *auth.AuthResult, id string, grants []string, includeArchived bool) (*store.Block, error) {
	block, err := store.GetBlock(ctx, pool, set, id, ar.ReadScopes, grants)
	if err != nil || block != nil || !includeArchived {
		return block, err
	}
	return store.GetArchivedBlock(ctx, pool, set, id, ownReadScopes(ar))
}
