// ID-token verification, ported from serviceportal internal/auth/oidc.go and
// parameterized for multiple providers (design/04 §4.1/§4.3.8):
//
//   - The RS256 hardlock (serviceportal oidc.go:135) becomes VerifyParams.Algs
//     (per-provider id_token_algs). alg:none and any algorithm outside the
//     set are rejected before key lookup — no RS/HS key confusion.
//   - The Entra tid check (serviceportal oidc.go:194) is dropped from the
//     verify core; its generalization is the ClaimFilter hook below, applied
//     by the caller AFTER verification (allowed_claim, design/04 F3).
//   - Audience: aud must contain the client_id; with MULTIPLE audiences the
//     azp claim must additionally equal the client_id (OIDC Core §3.1.3.7).
//   - email is only surfaced when email_verified=true (OIDC Core §5.7) —
//     an unverified provider email never reaches the caller.
//
// Every branch is a hard reject (fail-closed): there is no soft fallback.

package oidc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/golang-jwt/jwt/v5"
)

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK is a single JSON Web Key. RSA keys carry n/e, EC keys carry crv/x/y.
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// ParseJWKS decodes a fetched JWKS body (e.g. from Cache.JWKS) into the
// typed structure.
func ParseJWKS(body []byte) (*JWKS, error) {
	var jwks JWKS
	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("oidc: JWKS decode: %w", err)
	}
	return &jwks, nil
}

// jwkToPublicKey converts a JWK into a crypto public key. Supports RSA
// (RS256/384/512, PS*) and EC (ES256/384/512) key material; anything else
// is rejected.
func jwkToPublicKey(key JWK) (any, error) {
	switch key.Kty {
	case "RSA":
		nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil {
			return nil, fmt.Errorf("oidc: decoding JWK modulus: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
		if err != nil {
			return nil, fmt.Errorf("oidc: decoding JWK exponent: %w", err)
		}
		e := new(big.Int).SetBytes(eBytes)
		if !e.IsInt64() {
			return nil, errors.New("oidc: JWK exponent too large")
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: int(e.Int64()),
		}, nil
	case "EC":
		var curve elliptic.Curve
		switch key.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("oidc: unsupported JWK curve: %s", key.Crv)
		}
		xBytes, err := base64.RawURLEncoding.DecodeString(key.X)
		if err != nil {
			return nil, fmt.Errorf("oidc: decoding JWK x: %w", err)
		}
		yBytes, err := base64.RawURLEncoding.DecodeString(key.Y)
		if err != nil {
			return nil, fmt.Errorf("oidc: decoding JWK y: %w", err)
		}
		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}, nil
	default:
		return nil, fmt.Errorf("oidc: unsupported key type: %s", key.Kty)
	}
}

// VerifyParams carries the per-provider expectations for an ID-token verify.
// Every field is mandatory; empty fields are a hard configuration reject —
// a misconfigured provider must never soft-pass.
type VerifyParams struct {
	// Issuer is the expected issuer from the provider CONFIG ROW — never
	// from the token's own iss claim (INV-C, design/04 §5).
	Issuer string
	// ClientID must be contained in the aud claim.
	ClientID string
	// Nonce is the expected nonce from the sealed login state (single-use).
	Nonce string
	// Algs is the allowed signature algorithm set (id_token_algs).
	Algs []string
}

// IDTokenClaims is the verified, extracted claim set of an ID token.
type IDTokenClaims struct {
	Issuer  string
	Subject string
	// Email is only populated when the token carried email_verified=true;
	// otherwise it stays empty (OIDC Core §5.7, design/04 §4.3.8).
	Email             string
	EmailVerified     bool
	Name              string
	PreferredUsername string
	// Raw is the full verified claim set — input for ClaimFilter.Check.
	Raw map[string]any
}

// VerifyIDToken verifies an OIDC ID token against a JWKS and validates
// signature algorithm, issuer, audience (+azp on multi-audience), expiry and
// nonce. jwks must already be fetched (Cache.JWKS + ParseJWKS) — I/O and
// crypto stay separated so cached JWKS bytes are reusable.
func VerifyIDToken(tokenString string, jwks *JWKS, p VerifyParams) (*IDTokenClaims, error) {
	// Fail-closed on configuration: a provider without issuer/client_id/
	// nonce/algs must never verify anything.
	if p.Issuer == "" || p.ClientID == "" {
		return nil, errors.New("oidc: verify params missing issuer or client_id")
	}
	if p.Nonce == "" {
		return nil, errors.New("oidc: verify params missing expected nonce")
	}
	if len(p.Algs) == 0 {
		return nil, errors.New("oidc: verify params missing allowed algorithms")
	}
	if jwks == nil {
		return nil, errors.New("oidc: nil JWKS")
	}

	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		// Algorithm pin: alg must be in the per-provider set. Rejects
		// alg:none and HS*-against-RSA key confusion before any key is
		// handed to the parser. WithValidMethods below enforces the same
		// set inside the library (belt and suspenders).
		alg := t.Method.Alg()
		if !slices.Contains(p.Algs, alg) {
			return nil, fmt.Errorf("oidc: signing method %q not in allowed set %v", alg, p.Algs)
		}
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("oidc: missing kid in token header")
		}
		for _, key := range jwks.Keys {
			if key.Kid == kid {
				return jwkToPublicKey(key)
			}
		}
		return nil, fmt.Errorf("oidc: no matching JWKS key for kid %q", kid)
	},
		// jwt/v5 validates exp automatically when present; require it. The
		// audience check is done manually below because multi-audience
		// needs the additional azp rule.
		jwt.WithExpirationRequired(),
		jwt.WithValidMethods(p.Algs),
	)
	if err != nil {
		return nil, fmt.Errorf("oidc: token verification failed: %w", err)
	}
	if !token.Valid {
		return nil, errors.New("oidc: token is not valid")
	}

	// Issuer: token iss must equal the CONFIGURED issuer.
	iss, err := claims.GetIssuer()
	if err != nil || iss != p.Issuer {
		return nil, fmt.Errorf("oidc: issuer mismatch: got %q, want %q", iss, p.Issuer)
	}

	// Audience: aud must contain client_id …
	aud, err := claims.GetAudience()
	if err != nil || len(aud) == 0 {
		return nil, errors.New("oidc: audience claim missing")
	}
	if !slices.Contains(aud, p.ClientID) {
		return nil, fmt.Errorf("oidc: audience mismatch: got %v, want %q", aud, p.ClientID)
	}
	// … and with MULTIPLE audiences azp must equal client_id
	// (OIDC Core §3.1.3.7). Missing azp on multi-audience is a reject.
	if len(aud) > 1 {
		azp, _ := claims["azp"].(string)
		if azp == "" {
			return nil, errors.New("oidc: multiple audiences but azp claim missing")
		}
		if azp != p.ClientID {
			return nil, fmt.Errorf("oidc: azp mismatch: got %q, want %q", azp, p.ClientID)
		}
	}

	// Nonce: binds the token to this login round trip (anti-replay).
	nonce, _ := claims["nonce"].(string)
	if nonce != p.Nonce {
		return nil, errors.New("oidc: nonce mismatch")
	}

	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return nil, errors.New("oidc: subject claim missing")
	}

	out := &IDTokenClaims{
		Issuer:  iss,
		Subject: sub,
		Raw:     map[string]any(claims),
	}
	out.Name, _ = claims["name"].(string)
	out.PreferredUsername, _ = claims["preferred_username"].(string)
	// email only when email_verified is boolean true — a missing or
	// non-boolean email_verified drops the email (fail-closed).
	if verified, _ := claims["email_verified"].(bool); verified {
		out.Email, _ = claims["email"].(string)
		out.EmailVerified = out.Email != ""
	}
	return out, nil
}

// ClaimFilter is the generalized successor of the serviceportal Entra tid
// check (design/04 §4.1 F3): a {claim, values[]} allowlist checked against
// the verified claims. Providers with single_tenant_issuer=false MUST carry
// one — issuer+aud alone do not constrain WHO authenticates under a
// multi-tenant issuer (Azure common, Google without hd).
//
// JSON shape matches context_oauth_providers.allowed_claim (migration 100).
type ClaimFilter struct {
	Claim  string   `json:"claim"`
	Values []string `json:"values"`
}

// Check validates the filter against a verified claim set. The token's claim
// value (string, or array of strings — any overlap counts) must be contained
// in Values. Missing claim, foreign value, empty filter: all hard rejects.
func (f *ClaimFilter) Check(claims map[string]any) error {
	if f == nil || f.Claim == "" || len(f.Values) == 0 {
		return errors.New("oidc: allowed_claim filter is empty")
	}
	raw, ok := claims[f.Claim]
	if !ok {
		return fmt.Errorf("oidc: claim %q missing from token", f.Claim)
	}
	var got []string
	switch v := raw.(type) {
	case string:
		got = []string{v}
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("oidc: claim %q has non-string array member", f.Claim)
			}
			got = append(got, s)
		}
	default:
		return fmt.Errorf("oidc: claim %q has unsupported type %T", f.Claim, raw)
	}
	for _, g := range got {
		if slices.Contains(f.Values, g) {
			return nil
		}
	}
	return fmt.Errorf("oidc: claim %q value not in allowed set", f.Claim)
}
