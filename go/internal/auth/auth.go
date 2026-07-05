// Package auth handles API key authentication against the context_api_keys table.
// Keys are verified via SHA-256 hash comparison using pgcrypto.
package auth

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5/pgxpool"
)

// hexOnly strips everything except hex characters [a-fA-F0-9].
var hexOnly = regexp.MustCompile(`[^a-fA-F0-9]`)

// AuthResult holds the outcome of an API key authentication check.
type AuthResult struct {
	ApiKeyID      string   // UUID of the authenticated key (empty if invalid)
	HomeScope     string   // e.g. "private", "work"
	AllowedScopes []string // e.g. ["shared", "work"]
	ReadScopes    []string // [HomeScope] + AllowedScopes (+ cross-tenant grants)
	IsValid       bool
	IsAdmin       bool   // admin tier (052): settings/secrets/key management
	TenantID      string // UUID of the owning tenant (Modell C, 060); empty in the sentinel paths
	TenantRole    Role   // owner|admin|member tenant-role (059); typed in T20. Empty ("") in the sentinel paths.
	// WriteScopes is the RAW per-key write-scope set (078, E4b): scopes the key
	// may WRITE blocks to beyond its home_scope. It is the DB column value, NOT
	// yet intersected with (allowed_scopes ∪ home_scope) — that fail-closed
	// intersection is applied at the single eval point (handler.writableBlockScopes),
	// so a stale entry from a later allowed_scopes shrink never widens the write set.
	// Empty for every key until one is minted with explicit write_scopes.
	WriteScopes []string
	// ConfirmWrites is the per-key stage-then-confirm opt-in (090, F6-C6 D-W4).
	// NOT a security boundary — gating LLM writes is the harness's job
	// (DECISIONS §Klarstellung D-E1/E2); this is a per-principal distrust tool
	// for harnesses without a (trusted) gating layer of their own. false for
	// every existing key (fail-open, D-E2). Consumed at the store handlers
	// only (D-W5); internal writers (digest, dream) never see it.
	ConfirmWrites bool
}

// SanitizeKey strips all non-hex characters from an API key.
func SanitizeKey(raw string) string {
	return hexOnly.ReplaceAllString(raw, "")
}

// Authenticate verifies an API key against the database using the ctx_auth PG function.
// Returns an AuthResult with IsValid=false if the key is invalid or empty.
func Authenticate(ctx context.Context, pool *pgxpool.Pool, apiKey string) (*AuthResult, error) {
	if apiKey == "" {
		return &AuthResult{IsValid: false}, nil
	}

	var (
		apiKeyID      *string // nullable UUID
		homeScope     string
		allowedScopes []string
		readScopes    []string
		isValid       bool
		isAdmin       bool
		tenantID      *string // nullable UUID (NULL in the miss/suspended sentinel paths)
		tenantRole    string  // never NULL ('' in the sentinel paths)
		writeScopes   []string // 078 (E4b): RAW write-scope set; '{}' in the sentinel paths
		confirmWrites bool     // 090 (D-W4): per-key confirm opt-in; false in the sentinel paths
	)

	// confirm_writes is the LAST ctx_auth column (090, appended after
	// write_scopes so this named SELECT stays valid across the DROP+CREATE).
	err := pool.QueryRow(ctx,
		`SELECT api_key_id, home_scope, allowed_scopes, read_scopes, is_valid, is_admin, tenant_id, tenant_role, write_scopes, confirm_writes FROM ctx_auth($1)`,
		apiKey,
	).Scan(&apiKeyID, &homeScope, &allowedScopes, &readScopes, &isValid, &isAdmin, &tenantID, &tenantRole, &writeScopes, &confirmWrites)
	if err != nil {
		return nil, fmt.Errorf("auth: query ctx_auth: %w", err)
	}

	result := &AuthResult{
		HomeScope:     homeScope,
		AllowedScopes: allowedScopes,
		ReadScopes:    readScopes,
		IsValid:       isValid,
		IsAdmin:       isAdmin,
		TenantRole:    Role(tenantRole), // text column → named Role type (domain pinned to 059 CHECK, K4)
		WriteScopes:   writeScopes,
		ConfirmWrites: confirmWrites,
	}

	if apiKeyID != nil {
		result.ApiKeyID = *apiKeyID
	}
	if tenantID != nil {
		result.TenantID = *tenantID
	}

	return result, nil
}
