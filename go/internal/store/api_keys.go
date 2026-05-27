package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ApiKey represents a row in context_api_keys (without the plaintext key,
// which is only returned once at creation).
type ApiKey struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	HomeScope     string     `json:"home_scope"`
	AllowedScopes []string   `json:"allowed_scopes"`
	Active        bool       `json:"active"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// CreateApiKey generates a new API key, hashes it, and inserts the row.
// Returns the persisted record and the plaintext key (shown once, then only
// the SHA-256 hash is retained server-side).
//
// home_scope is required by API contract (v2.0.0 breaking change): callers
// must pass a non-empty value. The handler enforces this before calling.
// allowedScopes may be nil — the DB column defaults to '{shared}'.
func CreateApiKey(ctx context.Context, pool *pgxpool.Pool, label, homeScope string, allowedScopes []string) (*ApiKey, string, error) {
	if label == "" {
		return nil, "", fmt.Errorf("api_keys: label is required")
	}
	if homeScope == "" {
		return nil, "", fmt.Errorf("api_keys: home_scope is required")
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, "", fmt.Errorf("api_keys: generate key: %w", err)
	}
	plaintext := hex.EncodeToString(keyBytes)
	h := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(h[:])

	// api_key (legacy plaintext column, kept for migration compat) gets the
	// SHA-256 hash as well — production uses key_hash for lookups (see
	// migration 014_security_hardening.sql which dropped the unique
	// constraint on api_key and made key_hash unique).
	row := &ApiKey{}
	err := pool.QueryRow(ctx,
		`INSERT INTO context_api_keys (label, key_hash, api_key, home_scope, allowed_scopes, active)
		 VALUES ($1, $2, $2, $3, COALESCE($4::text[], '{shared}'::text[]), true)
		 RETURNING id, label, home_scope, allowed_scopes, active, last_used_at, created_at`,
		label, keyHash, homeScope, allowedScopes,
	).Scan(&row.ID, &row.Label, &row.HomeScope, &row.AllowedScopes, &row.Active, &row.LastUsedAt, &row.CreatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("api_keys: insert: %w", err)
	}

	return row, plaintext, nil
}

// ListApiKeys returns all API key rows ordered by created_at desc.
// Plaintext keys are not returned — callers receive metadata only.
func ListApiKeys(ctx context.Context, pool *pgxpool.Pool) ([]ApiKey, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, label, home_scope, allowed_scopes, active, last_used_at, created_at
		 FROM context_api_keys
		 ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("api_keys: list: %w", err)
	}
	defer rows.Close()

	var keys []ApiKey
	for rows.Next() {
		var k ApiKey
		if err := rows.Scan(&k.ID, &k.Label, &k.HomeScope, &k.AllowedScopes, &k.Active, &k.LastUsedAt, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("api_keys: scan: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// DeleteApiKey deactivates an API key by ID (soft delete: sets active=false).
func DeleteApiKey(ctx context.Context, pool *pgxpool.Pool, id string) (bool, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE context_api_keys SET active = false, updated_at = now()
		 WHERE id = $1::uuid AND active = true`,
		id,
	)
	if err != nil {
		return false, fmt.Errorf("api_keys: delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
