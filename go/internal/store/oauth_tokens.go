package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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

// OAuthToken is the mint input for one token family (099, waves S3/S4). The
// rows carry the INV-A single-key selector (APIKeyID) + the person anchor
// (PrincipalID) — never a key set. Scope stays advisory (INV-B: authorisation
// is always the full key scope via ctx_auth_by_id, design 03 §5). Lifetimes
// are passed per mint (E-TTL config values), not carried here.
type OAuthToken struct {
	APIKeyID    string
	PrincipalID string
	ClientID    string   // "" → NULL
	Audiences   []string // RFC 8707; always contains the ctx-MCP resource
	Scope       string   // "" → NULL
	IssuedVia   string   // 'oauth' | 'login'
}

// TokenHash is the DB lookup form of an opaque token — the plaintext never
// reaches the DB (design 03 §3). Hashed over the FULL presented string
// including the ctxt_/ctxr_ prefix.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
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

// newOpaqueToken generates one prefixed opaque token (32B crypto/rand hex).
func newOpaqueToken(prefix string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("oauth tokens: rand: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}

// TokenPair is the /token response substance of one mint or rotation (S4):
// a fresh access token plus the refresh token that continues its family.
// AccessTTL is the NOMINAL lifetime for the wire's expires_in (RFC 6749
// §5.1) — deriving it from AccessExpires-now would answer 3599 for a 1h
// token depending on scheduling.
type TokenPair struct {
	AccessToken    string
	RefreshToken   string
	AccessTTL      time.Duration
	AccessExpires  time.Time
	RefreshExpires time.Time
}

// RotateOutcome classifies a rotation attempt (S4, design 03 §4). Only Rotated
// carries a pair; everything else answers invalid_grant upstream — with NO
// distinction on the wire (no oracle for which check failed).
type RotateOutcome int

const (
	// RotateMiss — unknown, expired, wrong-client or capped-out refresh:
	// no theft signal, no side effect beyond a capped family's hygiene revoke.
	RotateMiss RotateOutcome = iota
	// RotateReuse — the presented refresh was ALREADY rotated/revoked: the
	// theft signal. The whole family is revoked as the detection response.
	RotateReuse
	// Rotated — success; the old refresh is dead, the pair is fresh.
	Rotated
)

// MintTokenPair mints the access+refresh row pair of one NEW token family
// (authorization_code grant at /token, or Achse 05 login): two rows, one
// fresh refresh_family, refresh parent_id NULL (the family anchor). Both
// rows share the INV-A single-key selector and all issuance attributes.
// The family absolute cap (E-TTL 90d) is measured from this anchor row's
// created_at — RotateRefreshToken enforces it on every rotation.
func MintTokenPair(ctx context.Context, pool *pgxpool.Pool, t OAuthToken, accessTTL, refreshTTL time.Duration) (*TokenPair, error) {
	access, err := newOpaqueToken(AccessTokenPrefix)
	if err != nil {
		return nil, err
	}
	refresh, err := newOpaqueToken(RefreshTokenPrefix)
	if err != nil {
		return nil, err
	}
	family := uuid.NewString()
	now := time.Now()
	pair := &TokenPair{
		AccessToken:    access,
		RefreshToken:   refresh,
		AccessTTL:      accessTTL,
		AccessExpires:  now.Add(accessTTL),
		RefreshExpires: now.Add(refreshTTL),
	}

	var clientID, scope any
	if t.ClientID != "" {
		clientID = t.ClientID
	}
	if t.Scope != "" {
		scope = t.Scope
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("oauth tokens: begin mint pair: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, row := range []struct {
		hash, typ string
		expires   time.Time
	}{
		{TokenHash(access), "access", pair.AccessExpires},
		{TokenHash(refresh), "refresh", pair.RefreshExpires},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_access_tokens
			     (token_hash, token_type, api_key_id, principal_id, client_id, audiences, scope, refresh_family, issued_via, expires_at)
			 VALUES ($1, $2, $3::uuid, $4::uuid, $5, $6, $7, $8::uuid, $9, $10)`,
			row.hash, row.typ, t.APIKeyID, t.PrincipalID, clientID, t.Audiences, scope, family, t.IssuedVia, row.expires,
		); err != nil {
			return nil, fmt.Errorf("oauth tokens: mint pair (%s): %w", row.typ, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("oauth tokens: commit mint pair: %w", err)
	}
	return pair, nil
}

// RotateRefreshToken performs one refresh_token grant (S4, design 03 §4):
// atomic take of the presented refresh, reuse detection with family revoke,
// and the mint of the successor pair — all inside one transaction.
//
// The take predicate carries the client binding (client_id must equal the
// row's — OAuth 2.1 §6 refresh binding, analogous to the S2 code binding):
// a wrong-client presentation is a plain miss WITHOUT side effects, so a
// third party who only sniffed the token value cannot revoke the victim's
// family by spraying client_ids (no oracle, no DoS). Reuse detection fires
// on a hash+client match whose row is already rotated/revoked — that IS the
// theft signal (either the thief re-used it after the victim rotated, or
// the victim re-used it after the thief did; indistinguishable, so the
// family dies either way and the human re-authorizes).
//
// The family absolute cap (E-TTL, capAge from the family anchor row's
// created_at) bounds how long rolling rotation can keep a family alive: a
// capped-out family is revoked (hygiene) and the attempt is a miss.
func RotateRefreshToken(ctx context.Context, pool *pgxpool.Pool, presented, clientID string, accessTTL, refreshTTL, capAge time.Duration) (*TokenPair, RotateOutcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, RotateMiss, fmt.Errorf("oauth tokens: begin rotate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var presentedClient any
	if clientID != "" {
		presentedClient = clientID
	}

	// Atomic take: of two racing rotations exactly one gets the row. The
	// client binding sits IN the predicate (see doc above).
	var (
		oldID, apiKeyID, principalID, family string
		rowClient, scope                     *string
		audiences                            []string
		issuedVia                            string
	)
	err = tx.QueryRow(ctx,
		`UPDATE context_access_tokens
		    SET revoked_at = now()
		  WHERE token_hash = $1
		    AND token_type = 'refresh'
		    AND client_id IS NOT DISTINCT FROM $2
		    AND revoked_at IS NULL
		    AND expires_at > now()
		 RETURNING id, api_key_id, principal_id, client_id, audiences, scope, refresh_family, issued_via`,
		TokenHash(presented), presentedClient,
	).Scan(&oldID, &apiKeyID, &principalID, &rowClient, &audiences, &scope, &family, &issuedVia)
	if errors.Is(err, pgx.ErrNoRows) {
		// No live take — distinguish theft (already rotated/revoked) from a
		// plain miss. The reuse probe carries the SAME client binding.
		var reuseFamily string
		err = tx.QueryRow(ctx,
			`SELECT refresh_family
			   FROM context_access_tokens
			  WHERE token_hash = $1
			    AND token_type = 'refresh'
			    AND client_id IS NOT DISTINCT FROM $2
			    AND revoked_at IS NOT NULL`,
			TokenHash(presented), presentedClient,
		).Scan(&reuseFamily)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, RotateMiss, nil // unknown/expired/wrong client — no signal
		}
		if err != nil {
			return nil, RotateMiss, fmt.Errorf("oauth tokens: reuse probe: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE context_access_tokens SET revoked_at = now()
			  WHERE refresh_family = $1::uuid AND revoked_at IS NULL`, reuseFamily); err != nil {
			return nil, RotateMiss, fmt.Errorf("oauth tokens: family revoke: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, RotateMiss, fmt.Errorf("oauth tokens: commit family revoke: %w", err)
		}
		return nil, RotateReuse, nil
	}
	if err != nil {
		return nil, RotateMiss, fmt.Errorf("oauth tokens: take refresh: %w", err)
	}

	// Family absolute cap (E-TTL 90d): anchored at the family's first row.
	var familyStart time.Time
	if err := tx.QueryRow(ctx,
		`SELECT min(created_at) FROM context_access_tokens WHERE refresh_family = $1::uuid`,
		family,
	).Scan(&familyStart); err != nil {
		return nil, RotateMiss, fmt.Errorf("oauth tokens: family anchor: %w", err)
	}
	capEnd := familyStart.Add(capAge)
	if !time.Now().Before(capEnd) {
		if _, err := tx.Exec(ctx,
			`UPDATE context_access_tokens SET revoked_at = now()
			  WHERE refresh_family = $1::uuid AND revoked_at IS NULL`, family); err != nil {
			return nil, RotateMiss, fmt.Errorf("oauth tokens: cap revoke: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, RotateMiss, fmt.Errorf("oauth tokens: commit cap revoke: %w", err)
		}
		return nil, RotateMiss, nil // capped out — re-authorize; no theft signal
	}

	access, err := newOpaqueToken(AccessTokenPrefix)
	if err != nil {
		return nil, RotateMiss, err
	}
	refresh, err := newOpaqueToken(RefreshTokenPrefix)
	if err != nil {
		return nil, RotateMiss, err
	}
	now := time.Now()
	refreshExpires := now.Add(refreshTTL)
	if refreshExpires.After(capEnd) {
		refreshExpires = capEnd // rolling TTL never outlives the family cap
	}
	pair := &TokenPair{
		AccessToken:    access,
		RefreshToken:   refresh,
		AccessTTL:      accessTTL,
		AccessExpires:  now.Add(accessTTL),
		RefreshExpires: refreshExpires,
	}
	var scopeVal, clientVal any
	if rowClient != nil {
		clientVal = *rowClient
	}
	if scope != nil {
		scopeVal = *scope
	}
	for _, row := range []struct {
		hash, typ string
		expires   time.Time
	}{
		{TokenHash(access), "access", pair.AccessExpires},
		{TokenHash(refresh), "refresh", pair.RefreshExpires},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_access_tokens
			     (token_hash, token_type, api_key_id, principal_id, client_id, audiences, scope, refresh_family, parent_id, issued_via, expires_at)
			 VALUES ($1, $2, $3::uuid, $4::uuid, $5, $6, $7, $8::uuid, $9::uuid, $10, $11)`,
			row.hash, row.typ, apiKeyID, principalID, clientVal, audiences, scopeVal, family, oldID, issuedVia, row.expires,
		); err != nil {
			return nil, RotateMiss, fmt.Errorf("oauth tokens: mint rotated (%s): %w", row.typ, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, RotateMiss, fmt.Errorf("oauth tokens: commit rotate: %w", err)
	}
	return pair, Rotated, nil
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
