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

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ResolveWebSession maps a ctx_session cookie value (the context_web_sessions
// row id, a v7 UUID — ≥74 bits of randomness, above the 64-bit session-id
// floor) onto the api_key_id selector of its CURRENT access token, plus the
// session's csrf_secret (R3: the Auth middleware checks it against the
// X-CSRF-Token header on state-changing cookie requests — the synchronizer
// pattern, 05 §4.4). The token gates mirror LookupAccessToken exactly
// (token_type='access', not revoked, not expired); no audience predicate —
// the overlay's existence IS the web-session marker, and only login mints
// (audiences=[ctx-mcp, ctx-web]) ever get an overlay row. The UPDATE is the
// ctx_auth-style write-on-read: a successful resolve stamps last_used_at.
// Every miss — malformed id (folded before it can raise 22P02, the L4
// pattern), unknown id, revoked or expired token — is the same silent
// ("","",false): the caller answers one uniform 401, no oracle.
func ResolveWebSession(ctx context.Context, pool *pgxpool.Pool, sessionID string) (apiKeyID, csrfSecret string, ok bool, err error) {
	if uuid.Validate(sessionID) != nil {
		return "", "", false, nil //nolint:nilerr // deliberate: malformed id folds into the same silent miss (no oracle, L4 pattern)
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
		 RETURNING t.api_key_id::text, ws.csrf_secret`,
		sessionID,
	).Scan(&apiKeyID, &csrfSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("web sessions: resolve: %w", err)
	}
	return apiKeyID, csrfSecret, true, nil
}

// CreateWebSession lays a web-session overlay row over a freshly minted
// login token pair (R3, 05 §4.3): the access token is located by its hash
// (MintTokenPair returns only the plaintext — the row id never leaves the
// DB), and the overlay inherits the token row's refresh_family so the
// cookie session follows the 03 rotation lineage. Returns the new session
// id (the ctx_session cookie value).
func CreateWebSession(ctx context.Context, pool *pgxpool.Pool, accessToken, principalID, csrfSecret, userAgent, clientIP string) (string, error) {
	var ip any
	if clientIP != "" {
		ip = clientIP
	}
	var sessionID string
	err := pool.QueryRow(ctx,
		`INSERT INTO context_web_sessions
		     (principal_id, access_token_id, refresh_family, csrf_secret, user_agent, client_ip)
		 SELECT $1::uuid, t.id, t.refresh_family, $3, NULLIF($4, ''), $5
		   FROM context_access_tokens t
		  WHERE t.token_hash = $2 AND t.token_type = 'access'
		 RETURNING id::text`,
		principalID, TokenHash(accessToken), csrfSecret, userAgent, ip,
	).Scan(&sessionID)
	if err != nil {
		return "", fmt.Errorf("web sessions: create: %w", err)
	}
	return sessionID, nil
}

// RepointWebSession moves a session's access_token_id onto the rotated
// access token (R3 refresh, 05 §4.3) — bound INSIDE the predicate to the
// session's OWN refresh_family: a refresh token of family A can never
// repoint a session of family B (cross-session hijack shape), it is a
// side-effect-free miss instead. csrf_secret deliberately survives the
// rotation (it hangs on the session, not the access token).
func RepointWebSession(ctx context.Context, pool *pgxpool.Pool, sessionID, newAccessToken string) (bool, error) {
	if uuid.Validate(sessionID) != nil {
		return false, nil //nolint:nilerr // deliberate: malformed id folds into the same silent miss (no oracle)
	}
	tag, err := pool.Exec(ctx,
		`UPDATE context_web_sessions ws
		    SET access_token_id = t.id, last_used_at = now()
		   FROM context_access_tokens t
		  WHERE ws.id = $1
		    AND t.token_hash = $2
		    AND t.token_type = 'access'
		    AND t.refresh_family = ws.refresh_family`,
		sessionID, TokenHash(newAccessToken),
	)
	if err != nil {
		return false, fmt.Errorf("web sessions: repoint: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DestroyWebSession implements logout (R3, 05 §4.3): revoke the session's
// WHOLE token family (access + refresh, the same kill 03's reuse detection
// uses) and delete the overlay row — one transaction, no half-dead session.
// A miss (unknown/malformed id) is silently fine: logout is idempotent.
func DestroyWebSession(ctx context.Context, pool *pgxpool.Pool, sessionID string) error {
	if uuid.Validate(sessionID) != nil {
		return nil //nolint:nilerr // deliberate: logout of a malformed id is a no-op, not an oracle
	}
	return pgxdb.Write(ctx, pool, pgxdb.Stages{Begin: "web sessions: begin destroy"}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE context_access_tokens
			    SET revoked_at = now()
			  WHERE revoked_at IS NULL
			    AND refresh_family = (SELECT refresh_family FROM context_web_sessions WHERE id = $1)`,
			sessionID,
		); err != nil {
			return fmt.Errorf("web sessions: revoke family: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM context_web_sessions WHERE id = $1`, sessionID,
		); err != nil {
			return fmt.Errorf("web sessions: delete overlay: %w", err)
		}
		return nil
	})
}
