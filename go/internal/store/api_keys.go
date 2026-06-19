package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTenantID is the fixed UUID of the single-tenant default tenant that
// the 059 backfill pins to slug 'default' (the value also lives as the DB
// DEFAULT on context_api_keys.tenant_id). It is the canonical home of this
// constant; the handler layer aliases it. The trailing '…0d3fa0' ≈ "default".
const DefaultTenantID = "00000000-0000-0000-0000-0000000d3fa0"

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
// allowedScopes may be nil — see the tenant-aware default below.
//
// tenantID (MT T06, Achse 01-T6) binds the new key to its owning tenant. The
// handler passes the creator's tenant (ar.TenantID); an empty string falls back
// to the default tenant (single-tenant / 059-backfill semantics — never NULL,
// never fail-open, the default tenant IS the incumbent). The store is the last
// line before the row exists, so the tenant-aware {shared} rule lives here.
func CreateApiKey(ctx context.Context, pool *pgxpool.Pool, label, homeScope string, allowedScopes []string, tenantID string) (*ApiKey, string, error) {
	if label == "" {
		return nil, "", fmt.Errorf("api_keys: label is required")
	}
	if homeScope == "" {
		return nil, "", fmt.Errorf("api_keys: home_scope is required")
	}
	if tenantID == "" {
		tenantID = DefaultTenantID
	}
	// Tenant-aware {shared} default (design/01 §3.6 / E3 / R-LEAK5): only the
	// default tenant inherits 'shared' automatically. A foreign tenant's key
	// with no explicit allowed_scopes gets an empty set — never an implicit
	// cross-tenant read into the default tenant's shared blocks. A non-nil empty
	// slice makes the COALESCE below keep '{}' instead of the '{shared}' literal.
	// TENANT-DECISION(shared-scope-owner): shared stays a default-tenant scope
	// (Weg a) — Alt: system-wide scope like _global (Weg b), reversible by
	// rehanging one context_tenant_scopes row.
	if allowedScopes == nil && tenantID != DefaultTenantID {
		allowedScopes = []string{}
	}
	// Scope-format gate (G03): '_'-prefixed scopes are SYSTEM-RESERVED
	// ('_global' = settings identity sentinel, 051). Enforced here as well
	// as in the handler — the store is the last line before the row exists.
	if strings.HasPrefix(homeScope, "_") {
		return nil, "", fmt.Errorf("api_keys: scope names starting with '_' are reserved: %s", homeScope)
	}
	for _, s := range allowedScopes {
		if strings.HasPrefix(s, "_") {
			return nil, "", fmt.Errorf("api_keys: scope names starting with '_' are reserved: %s", s)
		}
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, "", fmt.Errorf("api_keys: generate key: %w", err)
	}
	plaintext := hex.EncodeToString(keyBytes)
	h := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(h[:])

	// Migration 014_security_hardening.sql dropped the plaintext api_key
	// column; key_hash is now NOT NULL UNIQUE and the sole identifier for
	// ctx_auth lookups.
	row := &ApiKey{}
	err := pool.QueryRow(ctx,
		`INSERT INTO context_api_keys (label, key_hash, home_scope, allowed_scopes, active, tenant_id)
		 VALUES ($1, $2, $3, COALESCE($4::text[], '{shared}'::text[]), true, $5::uuid)
		 RETURNING id, label, home_scope, allowed_scopes, active, last_used_at, created_at`,
		label, keyHash, homeScope, allowedScopes, tenantID,
	).Scan(&row.ID, &row.Label, &row.HomeScope, &row.AllowedScopes, &row.Active, &row.LastUsedAt, &row.CreatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("api_keys: insert: %w", err)
	}

	return row, plaintext, nil
}

// ListApiKeys returns API key rows ordered by created_at desc, scoped to one
// tenant unless tenantFilter is empty (server-admin → all tenants) and limited
// to active keys unless activeOnly is false.
// Plaintext keys are not returned — callers receive metadata only.
func ListApiKeys(ctx context.Context, pool *pgxpool.Pool, tenantFilter string, activeOnly bool) ([]ApiKey, error) {
	// tenantFilter empty → NULL → no tenant constraint (server-admin, all
	// tenants); set → only that tenant's keys. activeOnly true → only active.
	var tenantArg *string
	if tenantFilter != "" {
		tenantArg = &tenantFilter
	}
	rows, err := pool.Query(ctx,
		`SELECT id, label, home_scope, allowed_scopes, active, last_used_at, created_at
		 FROM context_api_keys
		 WHERE ($1::uuid IS NULL OR tenant_id = $1::uuid)
		   AND (NOT $2::boolean OR active)
		 ORDER BY created_at DESC`,
		tenantArg, activeOnly)
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

// DeleteApiKey deactivates an API key by ID (soft delete: sets active=false),
// constrained to the caller's tenant unless the caller is a server-admin.
//
// The tenant constraint lives INSIDE the single UPDATE — atomic and
// TOCTOU-free, no fetch-then-write. A server-admin (isServerAdmin=true) deletes
// any key by id, exactly as before; a non-server-admin deletes only keys whose
// tenant_id matches callerTenant. A miss — wrong tenant, absent, already
// inactive, or a malformed id (22P02) — collapses to found=false with no error,
// so the caller cannot tell a foreign tenant's key from a nonexistent one
// (Leak-Pfad L2, design/05 §5.2). An empty callerTenant for a non-server-admin
// yields a NULL tenant arg, which matches nothing — fail-closed.
func DeleteApiKey(ctx context.Context, pool *pgxpool.Pool, id, callerTenant string, isServerAdmin bool) (bool, error) {
	var tenantArg *string
	if !isServerAdmin && callerTenant != "" {
		tenantArg = &callerTenant
	}
	tag, err := pool.Exec(ctx,
		`UPDATE context_api_keys SET active = false, updated_at = now()
		 WHERE id = $1::uuid AND active = true
		   AND ($2::boolean OR tenant_id = $3::uuid)`,
		id, isServerAdmin, tenantArg,
	)
	if err != nil {
		// A malformed id (invalid uuid text) can't match any key — collapse it
		// to a miss so it's indistinguishable from an absent id (no oracle).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return false, nil
		}
		return false, fmt.Errorf("api_keys: delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
