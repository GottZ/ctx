package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Opaque-token prefixes (099, design 03 §3): the prefix is what makes a token
// distinguishable from a raw (pure-hex) api key at the middleware entrance —
// api keys never carry one (api_keys.go hex.EncodeToString), so the branch in
// resolveCredential is collision-free by construction.
const (
	// AccessTokenPrefix marks opaque access tokens (valid at /mcp).
	AccessTokenPrefix = "ctxt_"
	// RefreshTokenPrefix marks refresh tokens (NEVER valid at /mcp; minted
	// from S4 on, the constant exists so the middleware can reject the class
	// explicitly instead of falling through to the raw-key path).
	RefreshTokenPrefix = "ctxr_"
)

// OAuthToken is the mint input for one opaque-token row (099, wave S3). The
// row carries the INV-A single-key selector (APIKeyID) + the person anchor
// (PrincipalID) — never a key set. Scope stays advisory (INV-B: authorisation
// is always the full key scope via ctx_auth_by_id, design 03 §5).
type OAuthToken struct {
	APIKeyID    string
	PrincipalID string
	ClientID    string   // "" → NULL
	Audiences   []string // RFC 8707; always contains the ctx-MCP resource
	Scope       string   // "" → NULL
	IssuedVia   string   // 'oauth' | 'login'
	ExpiresAt   time.Time
}

// TokenHash is the DB lookup form of an opaque token — the plaintext never
// reaches the DB (design 03 §3). Hashed over the FULL presented string
// including the ctxt_/ctxr_ prefix.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// MintAccessToken generates a fresh opaque access token (ctxt_ + 32B
// crypto/rand hex), persists its SHA-256 row and returns the plaintext —
// the ONLY place the plaintext ever exists server-side is this return value
// on its way into the /token (or Achse-05 login) response.
func MintAccessToken(ctx context.Context, pool *pgxpool.Pool, t OAuthToken) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("oauth tokens: rand: %w", err)
	}
	token := AccessTokenPrefix + hex.EncodeToString(raw)

	var clientID, scope any // "" must become NULL, not the empty string
	if t.ClientID != "" {
		clientID = t.ClientID
	}
	if t.Scope != "" {
		scope = t.Scope
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO context_access_tokens
		     (token_hash, token_type, api_key_id, principal_id, client_id, audiences, scope, issued_via, expires_at)
		 VALUES ($1, 'access', $2::uuid, $3::uuid, $4, $5, $6, $7, $8)`,
		TokenHash(token), t.APIKeyID, t.PrincipalID, clientID, t.Audiences, scope, t.IssuedVia, t.ExpiresAt,
	)
	if err != nil {
		return "", fmt.Errorf("oauth tokens: mint: %w", err)
	}
	return token, nil
}

// LookupAccessToken resolves a presented ctxt_ token to its INV-A single-key
// selector. Fail-closed by predicate: unknown hash, wrong type, revoked or
// expired rows are all the same miss ("", false, nil) — the caller answers
// 401 without an oracle. The key/principal gates themselves live downstream
// in ctx_auth_by_id (one gate location, design 03 §4).
func LookupAccessToken(ctx context.Context, pool *pgxpool.Pool, token string) (apiKeyID string, ok bool, err error) {
	err = pool.QueryRow(ctx,
		`SELECT api_key_id
		   FROM context_access_tokens
		  WHERE token_hash = $1
		    AND token_type = 'access'
		    AND revoked_at IS NULL
		    AND expires_at > now()`,
		TokenHash(token),
	).Scan(&apiKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("oauth tokens: lookup: %w", err)
	}
	return apiKeyID, true, nil
}

// EvictExpiredOAuthTokens deletes token rows 7 days past expiry (scheduler
// janitor tick, design 03 §4 GC). Correctness never depends on this sweep —
// LookupAccessToken's expires_at predicate rejects stale tokens regardless.
// The long grace deliberately keeps expired/revoked REFRESH rows around:
// S4's reuse-detection (replayed rotated refresh → family revoke) needs the
// rotated row to still exist as evidence; S4 revisits the retention split.
func EvictExpiredOAuthTokens(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_access_tokens WHERE expires_at < now() - interval '7 days'`)
	if err != nil {
		return 0, fmt.Errorf("oauth tokens: evict: %w", err)
	}
	return tag.RowsAffected(), nil
}
