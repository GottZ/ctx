// Web-session overlay resolution (design 05 §4.2 path 3, wave R2).
//
// context_web_sessions (102) is the cookie-side overlay over the ONE token
// store context_access_tokens (099): the ctx_session cookie carries the
// overlay row id, the row points at the CURRENT access-token row, and the
// token row carries the INV-A single-key selector. Authority NEVER derives
// from the session row itself — resolution always continues through
// ctx_auth_by_id (the api_key active / tenant status / principal is_active
// gates), so a soft-revoked key kills its sessions instantly with no
// session-side bookkeeping (05 §4.1).
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ResolveWebSession maps a ctx_session cookie value (the context_web_sessions
// row id, a v7 UUID — ≥74 bits of randomness, above the 64-bit session-id
// floor) onto the api_key_id selector of its CURRENT access token. The token
// gates mirror LookupAccessToken exactly (token_type='access', not revoked,
// not expired); no audience predicate — the overlay's existence IS the
// web-session marker, and only login mints (audiences=[ctx-mcp, ctx-web])
// ever get an overlay row. The UPDATE is the ctx_auth-style write-on-read:
// a successful resolve stamps last_used_at. Every miss — malformed id
// (folded before it can raise 22P02, the L4 pattern), unknown id, revoked
// or expired token — is the same silent (,"",false): the caller answers one
// uniform 401, no oracle.
func ResolveWebSession(ctx context.Context, pool *pgxpool.Pool, sessionID string) (apiKeyID string, ok bool, err error) {
	if uuid.Validate(sessionID) != nil {
		return "", false, nil //nolint:nilerr // deliberate: malformed id folds into the same silent miss (no oracle, L4 pattern)
	}
	err = pool.QueryRow(ctx,
		`UPDATE context_web_sessions ws
		    SET last_used_at = now()
		   FROM context_access_tokens t
		  WHERE ws.id = $1
		    AND t.id = ws.access_token_id
		    AND t.token_type = 'access'
		    AND t.revoked_at IS NULL
		    AND t.expires_at > now()
		 RETURNING t.api_key_id::text`,
		sessionID,
	).Scan(&apiKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("web sessions: resolve: %w", err)
	}
	return apiKeyID, true, nil
}
