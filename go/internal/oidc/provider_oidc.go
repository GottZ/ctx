// Generic OIDC provider: discovery-based endpoints + ID-token verification.
// This is the spine of the provider abstraction (design/04 §4.1/§4.3.8) —
// it covers Google, Entra, Keycloak, Authentik, Auth0 and any spec-clean IdP.

package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// OIDCConfig mirrors the relevant fields of a context_oauth_providers row
// (migration 100) for type='oidc'.
type OIDCConfig struct {
	// Issuer is the trusted issuer (allowlist row, INV-C). Discovery URL =
	// <issuer>/.well-known/openid-configuration.
	Issuer string
	// ClientID is the registered client at the IdP.
	ClientID string
	// IDTokenAlgs is the allowed signature algorithm set (id_token_algs).
	IDTokenAlgs []string
	// AllowedClaim is the generalized tid successor (design/04 F3). Nil for
	// single_tenant_issuer=true providers; MANDATORY (enforced by the config
	// layer, W4/W6) when the issuer is multi-tenant. When set, it is checked
	// after every successful verify — a token failing it is a hard reject.
	AllowedClaim *ClaimFilter
}

type oidcProvider struct {
	cache *Cache
	cfg   OIDCConfig
}

// NewOIDC builds a discovery-based OIDC provider. The cache is shared across
// providers (per-URL buckets). Configuration gaps are construction errors —
// a half-configured provider never becomes a Provider (fail-closed).
func NewOIDC(cache *Cache, cfg OIDCConfig) (Provider, error) {
	if cache == nil {
		return nil, errors.New("oidc: nil cache")
	}
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, errors.New("oidc: provider config missing issuer or client_id")
	}
	if len(cfg.IDTokenAlgs) == 0 {
		return nil, errors.New("oidc: provider config missing id_token_algs")
	}
	return &oidcProvider{cache: cache, cfg: cfg}, nil
}

// discoveryDocument is the subset of the OIDC discovery metadata we consume.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// discover fetches (cached) and validates the discovery document. The
// doc.issuer==configured-issuer match runs BEFORE any endpoint is handed out
// (substitution guard, OIDC Discovery 1.0 §4.3): a document served under our
// discovery URL but claiming a foreign issuer is a reject, and so are its
// endpoints.
func (p *oidcProvider) discover(ctx context.Context) (*discoveryDocument, error) {
	url := strings.TrimSuffix(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	body, err := p.cache.Discovery(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery for %s: %w", p.cfg.Issuer, err)
	}
	var doc discoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("oidc: discovery decode for %s: %w", p.cfg.Issuer, err)
	}
	if doc.Issuer != p.cfg.Issuer {
		return nil, fmt.Errorf("oidc: discovery issuer mismatch: got %q, want %q", doc.Issuer, p.cfg.Issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return nil, fmt.Errorf("oidc: discovery for %s missing required endpoints", p.cfg.Issuer)
	}
	return &doc, nil
}

func (p *oidcProvider) Endpoints(ctx context.Context) (authURL, tokenURL, jwksURL, userinfoURL string, err error) {
	doc, err := p.discover(ctx)
	if err != nil {
		return "", "", "", "", err
	}
	return doc.AuthorizationEndpoint, doc.TokenEndpoint, doc.JWKSURI, doc.UserinfoEndpoint, nil
}

func (p *oidcProvider) Identity(ctx context.Context, tokens Tokens, expectedNonce string) (ExternalIdentity, error) {
	if tokens.IDToken == "" {
		return ExternalIdentity{}, errors.New("oidc: token response carries no id_token")
	}
	// Re-run discovery (cache hit in the common case) so the issuer match is
	// enforced on THIS path too, not only when Endpoints was called first.
	doc, err := p.discover(ctx)
	if err != nil {
		return ExternalIdentity{}, err
	}
	jwksBody, err := p.cache.JWKS(ctx, doc.JWKSURI)
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("oidc: JWKS fetch: %w", err)
	}
	jwks, err := ParseJWKS(jwksBody)
	if err != nil {
		return ExternalIdentity{}, err
	}
	claims, err := VerifyIDToken(tokens.IDToken, jwks, VerifyParams{
		Issuer:   p.cfg.Issuer,
		ClientID: p.cfg.ClientID,
		Nonce:    expectedNonce,
		Algs:     p.cfg.IDTokenAlgs,
	})
	if err != nil {
		return ExternalIdentity{}, err
	}
	// Generalized tid successor: enforced AFTER the cryptographic verify,
	// against the verified claim set (design/04 §4.1 F3).
	if p.cfg.AllowedClaim != nil {
		if err := p.cfg.AllowedClaim.Check(claims.Raw); err != nil {
			return ExternalIdentity{}, err
		}
	}
	displayName := claims.Name
	if displayName == "" {
		displayName = claims.PreferredUsername
	}
	return ExternalIdentity{
		Issuer:        p.cfg.Issuer, // validated config issuer, never the raw claim
		Subject:       claims.Subject,
		Email:         claims.Email, // already gated on email_verified
		DisplayName:   displayName,
		EmailVerified: claims.EmailVerified,
	}, nil
}
