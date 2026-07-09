package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OAuthCode is one pending authorization code (098, design 03 §3 / wave S1).
// The row carries the INV-A single-key selector (APIKeyID) + the person
// anchor (PrincipalID) — never a key set and never the plaintext key; the
// code itself is stored only as its SHA-256 (code_hash). The S1-transitional
// sealed-key blob is gone since S3 (099 dropped the column): /token mints an
// opaque token from api_key_id and never needs the plaintext key again.
type OAuthCode struct {
	APIKeyID      string
	PrincipalID   string
	ClientID      string // "" → NULL (client_id becomes mandatory in S2/W03-2)
	RedirectURI   string
	CodeChallenge string
	ExpiresAt     time.Time
}

// PutOAuthCode persists one pending authorization code under its SHA-256
// (the plaintext code never reaches the DB). resource/scope stay NULL until
// W03-6 populates them.
func PutOAuthCode(ctx context.Context, pool *pgxpool.Pool, codeHash string, c OAuthCode) error {
	var clientID any // "" must become NULL, not the empty string
	if c.ClientID != "" {
		clientID = c.ClientID
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO context_oauth_codes
		     (code_hash, api_key_id, principal_id, client_id, redirect_uri, code_challenge, expires_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6, $7)`,
		codeHash, c.APIKeyID, c.PrincipalID, clientID, c.RedirectURI, c.CodeChallenge, c.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("oauth codes: put: %w", err)
	}
	return nil
}

// TakeOAuthCode atomically consumes the code row (single use): DELETE …
// RETURNING guarantees that of two racing /token calls exactly one gets the
// row — the in-memory map's HA gap, closed (design 03 §5). A miss returns
// (nil, nil). Expiry is checked by the CALLER against ExpiresAt (the row is
// gone either way — a replayed expired code stays a miss).
func TakeOAuthCode(ctx context.Context, pool *pgxpool.Pool, codeHash string) (*OAuthCode, error) {
	var (
		c        OAuthCode
		clientID *string
	)
	err := pool.QueryRow(ctx,
		`DELETE FROM context_oauth_codes
		  WHERE code_hash = $1
		 RETURNING api_key_id, principal_id, client_id, redirect_uri, code_challenge, expires_at`,
		codeHash,
	).Scan(&c.APIKeyID, &c.PrincipalID, &clientID, &c.RedirectURI, &c.CodeChallenge, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // unknown or already consumed — the caller answers invalid_grant
	}
	if err != nil {
		return nil, fmt.Errorf("oauth codes: take: %w", err)
	}
	if clientID != nil {
		c.ClientID = *clientID
	}
	return &c, nil
}

// EvictExpiredOAuthCodes deletes codes past their expiry plus a grace hour
// (scheduler janitor tick, design 03 §4 GC). The grace keeps very recent
// expiries around for debugging; correctness never depends on this sweep —
// TakeOAuthCode's expiry check rejects stale codes regardless.
func EvictExpiredOAuthCodes(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_oauth_codes WHERE expires_at < now() - interval '1 hour'`)
	if err != nil {
		return 0, fmt.Errorf("oauth codes: evict: %w", err)
	}
	return tag.RowsAffected(), nil
}
