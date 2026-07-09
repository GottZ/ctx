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
