// Unit tests (DB-less) for the fail-closed create validation of
// context_oauth_providers (OAuth L3, 04-W4) and the sealbox-name contract.
// validate() runs BEFORE any pool access, so CreateOAuthProvider with a nil
// pool proves the ordering: a validation error can never be preceded by a
// write. The full CRUD + secret roundtrip lives in the integration test.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestOAuthProviderSecretName pins the design/04 §3.2 name contract: the
// '.' separator keeps the name ValidSecretName-compatible — a ':' separator
// (the serviceportal habit) would fail the secret-name regex.
func TestOAuthProviderSecretName(t *testing.T) {
	name := OAuthProviderSecretName("corp-sso")
	if name != "oauth_provider.corp-sso.client_secret" {
		t.Fatalf("secret name = %q, want oauth_provider.corp-sso.client_secret", name)
	}
	if !ValidSecretName(name) {
		t.Fatalf("secret name %q fails ValidSecretName — the sealbox write would reject it", name)
	}
	if strings.Contains(name, ":") {
		t.Fatalf("secret name %q contains ':' — forbidden by the secret-name regex", name)
	}
}

// validSpec is a minimal spec that passes validation (secret-bearing OIDC).
func validSpec() CreateOAuthProviderSpec {
	return CreateOAuthProviderSpec{
		Slug:               "corp-sso",
		Type:               "oidc",
		DisplayName:        "Corp SSO",
		Issuer:             "https://idp.example.com",
		ClientID:           "ctx-client",
		ClientSecret:       "s3cret",
		SingleTenantIssuer: true,
	}
}

func TestCreateOAuthProvider_FailClosedValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateOAuthProviderSpec)
	}{
		{"missing slug", func(s *CreateOAuthProviderSpec) { s.Slug = "" }},
		{"missing issuer", func(s *CreateOAuthProviderSpec) { s.Issuer = "" }},
		{"bad type", func(s *CreateOAuthProviderSpec) { s.Type = "saml" }},
		{"bad token_auth", func(s *CreateOAuthProviderSpec) { s.TokenAuth = "private_key_jwt" }},
		{"secret required for post", func(s *CreateOAuthProviderSpec) { s.ClientSecret = "" }},
		{"secret required for basic", func(s *CreateOAuthProviderSpec) {
			s.TokenAuth = "client_secret_basic"
			s.ClientSecret = ""
		}},
		// token_auth='none' declares a public client — a configured secret is
		// a config contradiction, rejected instead of silently dropped.
		{"none with secret", func(s *CreateOAuthProviderSpec) { s.TokenAuth = "none" }},
		// F3 (design/04 §4.1): multi-tenant issuer without claim filter would
		// admit every account worldwide under that issuer.
		{"multi-tenant issuer without allowed_claim", func(s *CreateOAuthProviderSpec) { s.SingleTenantIssuer = false }},
		{"allowed_claim not an object", func(s *CreateOAuthProviderSpec) { s.AllowedClaim = json.RawMessage(`"tid"`) }},
		{"allowed_claim unknown field", func(s *CreateOAuthProviderSpec) {
			s.AllowedClaim = json.RawMessage(`{"claim":"tid","values":["x"],"value":"typo"}`)
		}},
		{"allowed_claim empty claim", func(s *CreateOAuthProviderSpec) { s.AllowedClaim = json.RawMessage(`{"claim":"","values":["x"]}`) }},
		{"allowed_claim empty values", func(s *CreateOAuthProviderSpec) { s.AllowedClaim = json.RawMessage(`{"claim":"tid","values":[]}`) }},
		{"allowed_claim empty value member", func(s *CreateOAuthProviderSpec) { s.AllowedClaim = json.RawMessage(`{"claim":"tid","values":[""]}`) }},
	}
	for _, c := range cases {
		spec := validSpec()
		c.mutate(&spec)
		// nil pool + nil box: reaching either would panic/err differently —
		// the validation error proves fail-closed ordering.
		_, err := CreateOAuthProvider(context.Background(), nil, nil, spec)
		if !errors.Is(err, ErrOAuthProviderInvalid) {
			t.Errorf("%s: err = %v, want ErrOAuthProviderInvalid", c.name, err)
		}
	}
}

// TestCreateOAuthProvider_SealboxRequired: a secret-bearing spec with no
// sealbox loaded fails closed with the dedicated sentinel (handler → 503) —
// AFTER validation (nil pool untouched), BEFORE any write.
func TestCreateOAuthProvider_SealboxRequired(t *testing.T) {
	_, err := CreateOAuthProvider(context.Background(), nil, nil, validSpec())
	if !errors.Is(err, ErrOAuthProviderSealbox) {
		t.Fatalf("err = %v, want ErrOAuthProviderSealbox", err)
	}
}

// TestDeleteOAuthProvider_EmptySlug: an empty slug is a validation error,
// never a silent zero-row delete.
func TestDeleteOAuthProvider_EmptySlug(t *testing.T) {
	_, err := DeleteOAuthProvider(context.Background(), nil, "")
	if !errors.Is(err, ErrOAuthProviderInvalid) {
		t.Fatalf("err = %v, want ErrOAuthProviderInvalid", err)
	}
}
