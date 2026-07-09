// Provider abstraction (design/04 §4.1): one interface, two shapes of IdP.
// The generic OIDC provider discovers its endpoints and verifies an ID token;
// GitHub (classic OAuth app, the deliberately divergent case) has fixed
// endpoints and NO ID token — identity comes from the userinfo API.

package oidc

import "context"

// Provider is a configured external identity provider. Implementations are
// stateless per call; construction happens once per config row.
type Provider interface {
	// Endpoints resolves the provider's OAuth endpoints.
	// OIDC: cached discovery (issuer/.well-known/openid-configuration),
	// with doc.issuer==configured issuer enforced BEFORE any endpoint is
	// used (substitution guard, OIDC Discovery 1.0 §4.3).
	// GitHub: fixed endpoints from the config row; jwksURL is empty.
	Endpoints(ctx context.Context) (authURL, tokenURL, jwksURL, userinfoURL string, err error)

	// Identity turns exchanged tokens into a verified external identity.
	// OIDC: verifies tokens.IDToken against the cached JWKS
	// (iss/aud/azp/exp/nonce, alg ∈ id_token_algs) + optional ClaimFilter.
	// GitHub: tokens.AccessToken → GET /user (+/user/emails); expectedNonce
	// is ignored (no ID token exists to carry one, design/04 F6).
	Identity(ctx context.Context, tokens Tokens, expectedNonce string) (ExternalIdentity, error)
}

// Tokens is the relevant subset of a token-endpoint response. The exchange
// itself lives in the flow handler wave (W6), not in this library.
type Tokens struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
}

// ExternalIdentity is the verified result of an external login. Issuer always
// carries the VALIDATED issuer from the provider config, never a raw claim.
// Email is empty unless the provider marked it verified.
type ExternalIdentity struct {
	Issuer        string
	Subject       string
	Email         string
	DisplayName   string
	EmailVerified bool
}
