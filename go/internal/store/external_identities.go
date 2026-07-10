// Store layer for the context_external_identities LOGIN path (OAuth L5,
// design/04 §4.3.9): the (issuer, subject) lookup under INV-C. The issuer
// argument always carries the VALIDATED issuer from the provider config row —
// never a raw iss claim (the oidc library guarantees this by construction).
//
// Deliberately lookup-only: an unknown (issuer, subject) is NOT created here.
// Provisioning is the E4b decision (admin-invite, DECISIONS.md) — no
// auto-create, no email-based auto-link (a provider setting email=victim must
// never capture an existing principal, design/04 §5).

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TouchExternalIdentityLogin resolves one verified external identity to its
// principal and refreshes the login bookkeeping in the same statement:
// verified_at + last_login_at are stamped, display_name is refreshed when the
// provider sent one, email is refreshed ONLY when non-empty — the caller MUST
// pass "" unless the provider marked the address verified (OIDC Core §5.7 /
// GitHub verified flag; design/04 §4.3.9). found=false means "identity not
// linked" (the E4b admin-invite reject); nothing is written in that case.
func TouchExternalIdentityLogin(ctx context.Context, pool *pgxpool.Pool, issuer, subject, verifiedEmail, displayName string) (string, bool, error) {
	if issuer == "" || subject == "" {
		return "", false, fmt.Errorf("external identities: issuer and subject are required")
	}
	var principalID string
	err := pool.QueryRow(ctx,
		`UPDATE context_external_identities
		    SET verified_at   = now(),
		        last_login_at = now(),
		        email         = CASE WHEN $3 <> '' THEN $3 ELSE email END,
		        display_name  = CASE WHEN $4 <> '' THEN $4 ELSE display_name END
		  WHERE issuer = $1 AND subject = $2
		 RETURNING principal_id`,
		issuer, subject, verifiedEmail, displayName,
	).Scan(&principalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("external identities: touch login: %w", err)
	}
	return principalID, true, nil
}

// PickPrincipalKey chooses the api key a fresh SSO session materialises on
// (R5, design/05 §4.5): the principal's most recently USED active key —
// "continue where the person last was" — falling back to the oldest key for
// never-used ones. Deterministic; the R6 key selector switches afterwards.
// ok=false means the principal holds NO active key: SSO established an
// identity but no authorization exists (INV-B) — the login answers
// fail-closed, since a session row structurally requires a key (INV-A).
func PickPrincipalKey(ctx context.Context, pool *pgxpool.Pool, principalID string) (apiKeyID string, ok bool, err error) {
	err = pool.QueryRow(ctx,
		`SELECT id::text
		   FROM context_api_keys
		  WHERE principal_id = $1::uuid AND active = true
		  ORDER BY last_used_at DESC NULLS LAST, created_at ASC
		  LIMIT 1`,
		principalID,
	).Scan(&apiKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("external identities: pick key: %w", err)
	}
	return apiKeyID, true, nil
}

// ErrIdentityConflict marks a link attempt on an (issuer, subject) that is
// already bound to a DIFFERENT principal — never silently re-bound (account-
// takeover shape); the operator must unlink explicitly first.
var ErrIdentityConflict = errors.New("external identity already linked to another principal")

// LinkExternalIdentity binds one (issuer, subject) to a principal (R5 E4b
// admin-invite: the operator pre-links identities; the login path itself
// never creates rows). Idempotent for the SAME principal (refreshes email/
// display_name), conflict error for a different one. The issuer is the
// caller-resolved trust value (provider row / GitHub constant) — never raw
// user input.
func LinkExternalIdentity(ctx context.Context, pool *pgxpool.Pool, provider, issuer, subject, principalID, email, displayName string) error {
	if provider == "" || issuer == "" || subject == "" {
		return fmt.Errorf("external identities: provider, issuer and subject are required")
	}
	var boundTo string
	err := pool.QueryRow(ctx,
		`INSERT INTO context_external_identities (principal_id, provider, issuer, subject, email, display_name)
		 VALUES ($1::uuid, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''))
		 ON CONFLICT (issuer, subject) DO UPDATE
		    SET email        = COALESCE(NULLIF(EXCLUDED.email, ''), context_external_identities.email),
		        display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), context_external_identities.display_name)
		  WHERE context_external_identities.principal_id = EXCLUDED.principal_id
		 RETURNING principal_id::text`,
		principalID, provider, issuer, subject, email, displayName,
	).Scan(&boundTo)
	if errors.Is(err, pgx.ErrNoRows) {
		// The ON CONFLICT WHERE clause filtered: the row exists with a
		// different principal.
		return ErrIdentityConflict
	}
	if err != nil {
		return fmt.Errorf("external identities: link: %w", err)
	}
	return nil
}

// UnlinkExternalIdentity removes one (issuer, subject) binding. Returns
// whether a row was removed (false = nothing was linked — idempotent).
func UnlinkExternalIdentity(ctx context.Context, pool *pgxpool.Pool, issuer, subject string) (bool, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_external_identities WHERE issuer = $1 AND subject = $2`,
		issuer, subject,
	)
	if err != nil {
		return false, fmt.Errorf("external identities: unlink: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ExternalIdentity is the operator-facing list row (no secret material — the
// table holds none).
type ExternalIdentity struct {
	PrincipalID string  `json:"principal_id"`
	Provider    string  `json:"provider"`
	Issuer      string  `json:"issuer"`
	Subject     string  `json:"subject"`
	Email       *string `json:"email,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	VerifiedAt  *string `json:"verified_at,omitempty"`
	LastLoginAt *string `json:"last_login_at,omitempty"`
	CreatedAt   string  `json:"created_at"`
}

// ListExternalIdentities returns the identity bindings, optionally filtered
// to one principal ("" = all).
func ListExternalIdentities(ctx context.Context, pool *pgxpool.Pool, principalID string) ([]ExternalIdentity, error) {
	rows, err := pool.Query(ctx,
		`SELECT principal_id::text, provider, issuer, subject, email, display_name,
		        verified_at::text, last_login_at::text, created_at::text
		   FROM context_external_identities
		  WHERE $1 = '' OR principal_id = NULLIF($1, '')::uuid
		  ORDER BY created_at DESC`,
		principalID,
	)
	if err != nil {
		return nil, fmt.Errorf("external identities: list: %w", err)
	}
	defer rows.Close()
	var out []ExternalIdentity
	for rows.Next() {
		var e ExternalIdentity
		if err := rows.Scan(&e.PrincipalID, &e.Provider, &e.Issuer, &e.Subject,
			&e.Email, &e.DisplayName, &e.VerifiedAt, &e.LastLoginAt, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("external identities: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
