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
	ReadScopes    []string // [HomeScope] + AllowedScopes
	IsValid       bool
	IsAdmin       bool // admin tier (052): settings/secrets/key management
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
	)

	err := pool.QueryRow(ctx,
		`SELECT api_key_id, home_scope, allowed_scopes, read_scopes, is_valid, is_admin FROM ctx_auth($1)`,
		apiKey,
	).Scan(&apiKeyID, &homeScope, &allowedScopes, &readScopes, &isValid, &isAdmin)
	if err != nil {
		return nil, fmt.Errorf("auth: query ctx_auth: %w", err)
	}

	result := &AuthResult{
		HomeScope:     homeScope,
		AllowedScopes: allowedScopes,
		ReadScopes:    readScopes,
		IsValid:       isValid,
		IsAdmin:       isAdmin,
	}

	if apiKeyID != nil {
		result.ApiKeyID = *apiKeyID
	}

	return result, nil
}
