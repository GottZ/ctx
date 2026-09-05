// Store layer for context_oauth_providers (migration 100, design/04 §3.2 +
// §7-W4): the allowlist carrier for external login (INV-C). Only providers
// registered here are trusted — the expected issuer at token-verify time
// always comes from this config row, never from a raw iss claim.
//
// The client_secret NEVER lives in the provider row: it is sealed via the
// existing sealbox stack (E4c — one crypto stack, one audit path) into
// context_secrets under the name 'oauth_provider.<slug>.client_secret',
// scope '_global'. The slug CHECK regex (migration 100) is a strict subset
// of ValidSecretName, so every legal slug yields a legal secret name — the
// '.' separator is deliberate, ':' would fail ValidSecretName.
//
// Fail-closed configuration honesty (design/04 §3.2/§4.1):
//   - token_auth='none' (public/native client, PKCE-only exchange) carries
//     NO secret — a configured secret is rejected, not silently dropped.
//   - every other token_auth REQUIRES a secret at create time.
//   - single_tenant_issuer=false REQUIRES a well-formed allowed_claim
//     ({claim, values[]}) BEFORE the insert — the schema deliberately has
//     no DB CHECK for this pair (admin config may grow stepwise), so the
//     Go layer is the enforcing one; a half-configured row is never created
//     by this path.

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/oidc"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/sealbox"
)

// Sentinel errors for the manage-handler status mapping (invalid → 400,
// exists → 409, sealbox → 503). Wrapped with detail via %w; the detail never
// contains secret material.
var (
	ErrOAuthProviderInvalid = errors.New("oauth provider: invalid configuration")
	ErrOAuthProviderExists  = errors.New("oauth provider: slug already registered")
	ErrOAuthProviderSealbox = errors.New("oauth provider: sealbox unavailable (CTX_SECRETS_KEY not configured)")
)

// OAuthProviderSecretName returns the context_secrets name under which a
// provider's client_secret is sealed. '.' separator on purpose — ':' is
// rejected by ValidSecretName (store/sealbox.go), the exact trap the
// migration-100 slug CHECK guards against.
func OAuthProviderSecretName(slug string) string {
	return "oauth_provider." + slug + ".client_secret"
}

// OAuthProvider is one context_oauth_providers row. It carries NO secret
// material: HasSecret is the only secret-related detail any read surface
// exposes (list/get responses serialize this struct directly).
type OAuthProvider struct {
	ID                 string            `json:"id"`
	Slug               string            `json:"slug"`
	Type               string            `json:"type"`
	DisplayName        string            `json:"display_name"`
	Issuer             string            `json:"issuer"`
	ClientID           string            `json:"client_id"`
	TokenAuth          string            `json:"token_auth"`
	RedirectBase       *string           `json:"redirect_base,omitempty"`
	Scopes             []string          `json:"scopes"`
	AuthURL            *string           `json:"auth_url,omitempty"`
	TokenURL           *string           `json:"token_url,omitempty"`
	UserinfoURL        *string           `json:"userinfo_url,omitempty"`
	IDTokenAlgs        []string          `json:"id_token_algs"`
	SingleTenantIssuer bool              `json:"single_tenant_issuer"`
	AllowedClaim       *oidc.ClaimFilter `json:"allowed_claim,omitempty"`
	Active             bool              `json:"active"`
	CreatedAt          time.Time         `json:"created_at"`
	CreatedBy          *string           `json:"created_by,omitempty"`
	HasSecret          bool              `json:"has_secret"`
}

// CreateOAuthProviderSpec carries an oauth-provider-create request. The
// ClientSecret is plaintext IN TRANSIT only: sealed before the row commits,
// never stored, never echoed. AllowedClaim arrives as raw JSON so the shape
// validation (strict decode, unknown fields rejected) lives here — the
// schema does not enforce it.
type CreateOAuthProviderSpec struct {
	Slug         string
	Type         string // 'oidc' | 'github'
	DisplayName  string
	Issuer       string
	ClientID     string
	ClientSecret string // required unless TokenAuth == 'none' (then forbidden)
	TokenAuth    string // '' defaults to 'client_secret_post' (schema default)
	RedirectBase string
	Scopes       []string // nil/empty → schema default {openid,profile,email}
	AuthURL      string
	TokenURL     string
	UserinfoURL  string
	IDTokenAlgs  []string // nil/empty → schema default {RS256}
	// SingleTenantIssuer=false (multi-tenant issuer: Azure 'common', Google
	// without hd) makes AllowedClaim mandatory. NOTE the Go zero value is the
	// fail-closed direction: a caller that forgets the field must supply a
	// claim filter or gets an error — set true explicitly for single-tenant IdPs.
	SingleTenantIssuer bool
	AllowedClaim       json.RawMessage // optional {claim, values[]} filter
	CreatedBy          string          // acting api_key id ("" = unattributed)
}

// tokenAuthOrDefault mirrors the schema default of migration 100.
func (s CreateOAuthProviderSpec) tokenAuthOrDefault() string {
	if s.TokenAuth == "" {
		return "client_secret_post"
	}
	return s.TokenAuth
}

// invalidf wraps ErrOAuthProviderInvalid with detail (handler → 400).
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrOAuthProviderInvalid, fmt.Sprintf(format, args...))
}

// validate runs every fail-closed check BEFORE any DB write and returns the
// normalized allowed_claim JSON (nil when absent). Slug FORMAT is deliberately
// left to the migration-100 CHECK (mapped to ErrOAuthProviderInvalid below) —
// one authoritative regex, not two drifting copies.
func (s CreateOAuthProviderSpec) validate() ([]byte, error) {
	if s.Slug == "" || s.Type == "" || s.DisplayName == "" || s.Issuer == "" || s.ClientID == "" {
		return nil, invalidf("slug, type, display_name, issuer and client_id are required")
	}
	if s.Type != "oidc" && s.Type != "github" {
		return nil, invalidf("type must be 'oidc' or 'github', got %q", s.Type)
	}
	tokenAuth := s.tokenAuthOrDefault()
	switch tokenAuth {
	case "client_secret_post", "client_secret_basic":
		if s.ClientSecret == "" {
			return nil, invalidf("client_secret is required for token_auth=%q", tokenAuth)
		}
	case "none":
		// Public/native client (RFC 7591 / OAuth 2.1): the exchange is
		// PKCE-only, NO secret exists. A configured secret contradicts the
		// declared client type — reject instead of silently dropping it.
		if s.ClientSecret != "" {
			return nil, invalidf("token_auth='none' declares a public client (PKCE-only) — a client_secret must not be configured")
		}
	default:
		return nil, invalidf("token_auth must be 'client_secret_post', 'client_secret_basic' or 'none', got %q", tokenAuth)
	}

	var claimJSON []byte
	if len(s.AllowedClaim) > 0 && string(s.AllowedClaim) != "null" {
		// Strict shape check {claim, values[]}: unknown fields are a config
		// mistake ('value' vs 'values' would otherwise pass silently and the
		// filter would reject every login at runtime — or worse, a future
		// lenient reader would ignore the filter).
		dec := json.NewDecoder(bytes.NewReader(s.AllowedClaim))
		dec.DisallowUnknownFields()
		var f oidc.ClaimFilter
		if err := dec.Decode(&f); err != nil {
			return nil, invalidf("allowed_claim must be {\"claim\": string, \"values\": [string]}: %v", err)
		}
		if f.Claim == "" || len(f.Values) == 0 {
			return nil, invalidf("allowed_claim requires a non-empty claim and at least one value")
		}
		for _, v := range f.Values {
			if v == "" {
				return nil, invalidf("allowed_claim values must be non-empty strings")
			}
		}
		normalized, err := json.Marshal(f)
		if err != nil {
			return nil, fmt.Errorf("oauth provider: normalize allowed_claim: %w", err)
		}
		claimJSON = normalized
	}
	if !s.SingleTenantIssuer && claimJSON == nil {
		// F3 (design/04 §4.1): under a multi-tenant issuer, iss+aud do not
		// constrain WHO authenticates — without a claim filter every account
		// worldwide under this issuer would reach the provisioning path.
		return nil, invalidf("single_tenant_issuer=false requires allowed_claim ({claim, values[]}) — fail-closed, see design/04 §4.1")
	}
	return claimJSON, nil
}

// mapOAuthProviderPgError folds constraint violations onto the sentinel
// errors: unique (slug) → exists, CHECK (slug regex / type / token_auth) →
// invalid. Everything else passes through.
func mapOAuthProviderPgError(err error, slug string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %q", ErrOAuthProviderExists, slug)
		case "23514": // check_violation
			return invalidf("schema constraint %q rejected the row (slug must match ^[a-z0-9][a-z0-9._-]{0,40}$; type oidc|github; token_auth client_secret_post|client_secret_basic|none)", pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("oauth provider: create %s: %w", slug, err)
}

// CreateOAuthProvider validates fail-closed, inserts the provider row and
// seals the client_secret into context_secrets — row and secret commit in
// ONE transaction (no half-configured provider survives an error on either
// side). box may be nil only for token_auth='none' specs.
func CreateOAuthProvider(ctx context.Context, pool *pgxpool.Pool, box *sealbox.Box, spec CreateOAuthProviderSpec) (*OAuthProvider, error) {
	claimJSON, err := spec.validate()
	if err != nil {
		return nil, err
	}
	tokenAuth := spec.tokenAuthOrDefault()
	secretRequired := tokenAuth != "none"

	secretName := OAuthProviderSecretName(spec.Slug)
	var nonce, ciphertext []byte
	if secretRequired {
		if box == nil {
			return nil, ErrOAuthProviderSealbox
		}
		// AAD binds the ciphertext to (name, '_global') — copied onto another
		// row it fails authentication (sealbox contract).
		nonce, ciphertext, err = box.Seal(secretName, GlobalScope, []byte(spec.ClientSecret))
		if err != nil {
			return nil, fmt.Errorf("oauth provider: seal secret: %w", err)
		}
	}

	scopes := spec.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	algs := spec.IDTokenAlgs
	if len(algs) == 0 {
		algs = []string{"RS256"}
	}

	var p OAuthProvider
	if err := pgxdb.Write(ctx, pool, pgxdb.At("oauth provider"), func(tx pgx.Tx) error {
		// Row first, secret second: an invalid slug then fails on the migration-100
		// CHECK (mapped above) before ValidSecretName inside PutSecret could ever
		// see it — one authoritative error surface for the slug format.
		var claimRaw []byte
		err := tx.QueryRow(ctx,
			`INSERT INTO context_oauth_providers
			    (slug, type, display_name, issuer, client_id, token_auth, redirect_base,
			     scopes, auth_url, token_url, userinfo_url, id_token_algs,
			     single_tenant_issuer, allowed_claim, created_by, created_by_principal)
			 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''),
			         $8, NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''), $12,
			         $13, $14, NULLIF($15, '')::uuid,
			         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = NULLIF($15, '')::uuid))
			 RETURNING id, slug, type, display_name, issuer, client_id, token_auth,
			           redirect_base, scopes, auth_url, token_url, userinfo_url,
			           id_token_algs, single_tenant_issuer, allowed_claim, active,
			           created_at, created_by`,
			spec.Slug, spec.Type, spec.DisplayName, spec.Issuer, spec.ClientID, tokenAuth, spec.RedirectBase,
			scopes, spec.AuthURL, spec.TokenURL, spec.UserinfoURL, algs,
			spec.SingleTenantIssuer, claimJSON, spec.CreatedBy,
		).Scan(&p.ID, &p.Slug, &p.Type, &p.DisplayName, &p.Issuer, &p.ClientID, &p.TokenAuth,
			&p.RedirectBase, &p.Scopes, &p.AuthURL, &p.TokenURL, &p.UserinfoURL,
			&p.IDTokenAlgs, &p.SingleTenantIssuer, &claimRaw, &p.Active,
			&p.CreatedAt, &p.CreatedBy)
		if err != nil {
			return mapOAuthProviderPgError(err, spec.Slug)
		}
		if p.AllowedClaim, err = decodeAllowedClaim(claimRaw); err != nil {
			return err
		}

		if secretRequired {
			var by *string
			if spec.CreatedBy != "" {
				by = &spec.CreatedBy
			}
			if _, err := PutSecret(ctx, tx, secretName, GlobalScope, nonce, ciphertext, 1, by); err != nil {
				return fmt.Errorf("oauth provider: persist secret: %w", err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	p.HasSecret = secretRequired
	return &p, nil
}

// oauthProviderSelect is the shared read shape. has_secret is an EXISTS
// probe against context_secrets — existence only, never metadata beyond it.
const oauthProviderSelect = `
	SELECT p.id, p.slug, p.type, p.display_name, p.issuer, p.client_id, p.token_auth,
	       p.redirect_base, p.scopes, p.auth_url, p.token_url, p.userinfo_url,
	       p.id_token_algs, p.single_tenant_issuer, p.allowed_claim, p.active,
	       p.created_at, p.created_by,
	       EXISTS (SELECT 1 FROM context_secrets s
	               WHERE s.name = 'oauth_provider.' || p.slug || '.client_secret'
	                 AND s.scope = '_global') AS has_secret
	FROM context_oauth_providers p`

// scanOAuthProvider scans one oauthProviderSelect row (pgx.Rows satisfies
// pgx.Row structurally).
func scanOAuthProvider(row pgx.Row) (*OAuthProvider, error) {
	var p OAuthProvider
	var claimRaw []byte
	err := row.Scan(&p.ID, &p.Slug, &p.Type, &p.DisplayName, &p.Issuer, &p.ClientID, &p.TokenAuth,
		&p.RedirectBase, &p.Scopes, &p.AuthURL, &p.TokenURL, &p.UserinfoURL,
		&p.IDTokenAlgs, &p.SingleTenantIssuer, &claimRaw, &p.Active,
		&p.CreatedAt, &p.CreatedBy, &p.HasSecret)
	if err != nil {
		return nil, err
	}
	if p.AllowedClaim, err = decodeAllowedClaim(claimRaw); err != nil {
		return nil, err
	}
	return &p, nil
}

// decodeAllowedClaim turns the stored JSONB (created only via the strict
// validate path) back into the verifier's filter type.
func decodeAllowedClaim(raw []byte) (*oidc.ClaimFilter, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var f oidc.ClaimFilter
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("oauth provider: decode allowed_claim: %w", err)
	}
	return &f, nil
}

// ListOAuthProviders returns every provider row, ordered by slug. Secret
// existence surfaces ONLY as the has_secret bool.
func ListOAuthProviders(ctx context.Context, pool *pgxpool.Pool) ([]OAuthProvider, error) {
	rows, err := pool.Query(ctx, oauthProviderSelect+` ORDER BY p.slug`)
	if err != nil {
		return nil, fmt.Errorf("oauth provider: list: %w", err)
	}
	defer rows.Close()

	providers := []OAuthProvider{}
	for rows.Next() {
		p, err := scanOAuthProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("oauth provider: scan: %w", err)
		}
		providers = append(providers, *p)
	}
	return providers, rows.Err()
}

// GetOAuthProviderBySlug returns one provider row (found=false when absent).
// The login flow (04-W6) resolves its provider through this — it must
// additionally require Active AND the §4.2 fail-closed inactivity rule
// (single_tenant_issuer=false without allowed_claim ⇒ treat as inactive);
// this getter deliberately returns the raw config row for the admin surface.
func GetOAuthProviderBySlug(ctx context.Context, pool *pgxpool.Pool, slug string) (*OAuthProvider, bool, error) {
	p, err := scanOAuthProvider(pool.QueryRow(ctx, oauthProviderSelect+` WHERE p.slug = $1`, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("oauth provider: get %s: %w", slug, err)
	}
	return p, true, nil
}

// DeleteOAuthProvider removes a provider row AND its sealed client_secret in
// one transaction — a deleted provider must never leave an orphaned secret
// behind. found=false when no provider matched (nothing else touched). The
// secret delete tolerates absence (token_auth='none' providers have none);
// the 051 triggers emit audit + NOTIFY for the secret removal.
func DeleteOAuthProvider(ctx context.Context, pool *pgxpool.Pool, slug string) (bool, error) {
	if slug == "" {
		return false, invalidf("slug is required")
	}
	if err := pgxdb.Write(ctx, pool, pgxdb.At("oauth provider"), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM context_oauth_providers WHERE slug = $1`, slug)
		if err != nil {
			return fmt.Errorf("oauth provider: delete %s: %w", slug, err)
		}
		if tag.RowsAffected() == 0 {
			// (false, nil) before the commit in the straight-line form —
			// pgxdb.ErrRollback keeps that miss a rollback instead of a commit.
			return pgxdb.ErrRollback
		}
		if _, err := DeleteSecret(ctx, tx, OAuthProviderSecretName(slug), GlobalScope, nil); err != nil {
			return fmt.Errorf("oauth provider: sweep secret for %s: %w", slug, err)
		}
		return nil
	}); err != nil {
		if errors.Is(err, pgxdb.ErrRollback) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
