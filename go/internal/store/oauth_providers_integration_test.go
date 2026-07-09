//go:build integration

// Integration gates for OAuth wave W4/L3 (design/04 §7-W4, negative-probed)
// against a real PG18 testcontainer with migration 100 applied:
//
//   - CRUD roundtrip (create → get-by-slug → list → delete)
//   - secret seal/open roundtrip under the REAL name
//     'oauth_provider.<slug>.client_secret' via the production ResolveSecret
//     path — probes ValidSecretName (a ':' separator would go red here)
//   - slug duplicate → ErrOAuthProviderExists, and the secret of the first
//     registration survives untouched
//   - migration-100 CHECK violations (slug regex) map to ErrOAuthProviderInvalid
//   - single_tenant_issuer=false without allowed_claim → invalid, NO row
//   - token_auth='none' with secret → invalid; without → row w/o secret
//   - delete sweeps the sealed secret in the same transaction
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestOAuthProvider -count=1 -v
package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func newTestBox(t *testing.T) *sealbox.Box {
	t.Helper()
	raw := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	box, err := sealbox.New(hex.EncodeToString(raw), "")
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	return box
}

func TestOAuthProviderCRUDAndSecretRoundtrip_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := newTestBox(t)
	ctx := context.Background()

	const plaintext = "oidc-client-secret-plaintext-0123456789"
	spec := store.CreateOAuthProviderSpec{
		Slug:               "corp-sso",
		Type:               "oidc",
		DisplayName:        "Corp SSO",
		Issuer:             "https://idp.example.com",
		ClientID:           "ctx-client",
		ClientSecret:       plaintext,
		SingleTenantIssuer: true,
		AllowedClaim:       json.RawMessage(`{"claim":"hd","values":["example.com"]}`),
	}
	p, err := store.CreateOAuthProvider(ctx, pool, box, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.TokenAuth != "client_secret_post" {
		t.Errorf("token_auth = %q, want schema default client_secret_post", p.TokenAuth)
	}
	if !p.HasSecret || !p.Active || !p.SingleTenantIssuer {
		t.Errorf("has_secret/active/single_tenant_issuer = %v/%v/%v, want true/true/true", p.HasSecret, p.Active, p.SingleTenantIssuer)
	}
	if len(p.Scopes) != 3 || len(p.IDTokenAlgs) != 1 || p.IDTokenAlgs[0] != "RS256" {
		t.Errorf("defaults not applied: scopes=%v algs=%v", p.Scopes, p.IDTokenAlgs)
	}
	if p.AllowedClaim == nil || p.AllowedClaim.Claim != "hd" || len(p.AllowedClaim.Values) != 1 {
		t.Errorf("allowed_claim roundtrip broken: %+v", p.AllowedClaim)
	}

	// Secret roundtrip with the ACTUAL name over the production resolve path.
	// This is the ValidSecretName probe: 'oauth_provider:corp-sso:...' (':'
	// separator) would never have been written — PutSecret rejects it.
	name := store.OAuthProviderSecretName("corp-sso")
	got, err := store.ResolveSecret(ctx, pool, box, name, store.GlobalScope)
	if err != nil {
		t.Fatalf("resolve secret %q: %v", name, err)
	}
	if string(got) != plaintext {
		t.Fatalf("secret roundtrip = %q, want %q", got, plaintext)
	}

	// Get-by-slug + list agree; neither carries secret material beyond the bool.
	fetched, found, err := store.GetOAuthProviderBySlug(ctx, pool, "corp-sso")
	if err != nil || !found {
		t.Fatalf("get-by-slug: found=%v err=%v", found, err)
	}
	if fetched.ID != p.ID || fetched.ClientID != "ctx-client" || !fetched.HasSecret {
		t.Errorf("get-by-slug mismatch: %+v", fetched)
	}
	list, err := store.ListOAuthProviders(ctx, pool)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: n=%d err=%v", len(list), err)
	}

	// Duplicate slug → exists; the FIRST registration's secret stays intact
	// (the failed tx must not have rotated or deleted it).
	if _, err := store.CreateOAuthProvider(ctx, pool, box, spec); !errors.Is(err, store.ErrOAuthProviderExists) {
		t.Fatalf("duplicate slug: err = %v, want ErrOAuthProviderExists", err)
	}
	if got, err := store.ResolveSecret(ctx, pool, box, name, store.GlobalScope); err != nil || string(got) != plaintext {
		t.Fatalf("secret after duplicate attempt: %q/%v, want intact", got, err)
	}

	// Unknown slug → found=false, no error (the 04-W6 flow 404s on this).
	if _, found, err := store.GetOAuthProviderBySlug(ctx, pool, "nope"); found || err != nil {
		t.Fatalf("unknown slug: found=%v err=%v, want false/nil", found, err)
	}

	// Delete sweeps the sealed secret with the row.
	deleted, err := store.DeleteOAuthProvider(ctx, pool, "corp-sso")
	if err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if exists, err := store.SecretExists(ctx, pool, name, store.GlobalScope); err != nil || exists {
		t.Fatalf("secret after delete: exists=%v err=%v, want swept", exists, err)
	}
	if _, found, _ := store.GetOAuthProviderBySlug(ctx, pool, "corp-sso"); found {
		t.Fatal("provider still present after delete")
	}
	// Second delete → found=false (idempotent surface, no oracle beyond it).
	if deleted, err := store.DeleteOAuthProvider(ctx, pool, "corp-sso"); err != nil || deleted {
		t.Fatalf("re-delete: deleted=%v err=%v, want false/nil", deleted, err)
	}
}

func TestOAuthProviderFailClosed_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := newTestBox(t)
	ctx := context.Background()

	// single_tenant_issuer=false without allowed_claim: rejected BEFORE the
	// insert — no half-configured row appears (design/04 §4.1 F3).
	multi := store.CreateOAuthProviderSpec{
		Slug: "azure-common", Type: "oidc", DisplayName: "Azure",
		Issuer: "https://login.microsoftonline.com/common/v2.0",
		ClientID: "app", ClientSecret: "s", SingleTenantIssuer: false,
	}
	if _, err := store.CreateOAuthProvider(ctx, pool, box, multi); !errors.Is(err, store.ErrOAuthProviderInvalid) {
		t.Fatalf("multi-tenant issuer w/o claim: err = %v, want ErrOAuthProviderInvalid", err)
	}
	if _, found, _ := store.GetOAuthProviderBySlug(ctx, pool, "azure-common"); found {
		t.Fatal("rejected spec left a row behind")
	}
	// ...and its secret name never reached context_secrets either.
	if exists, _ := store.SecretExists(ctx, pool, store.OAuthProviderSecretName("azure-common"), store.GlobalScope); exists {
		t.Fatal("rejected spec left a sealed secret behind")
	}

	// token_auth='none' with a secret: config contradiction, rejected.
	pub := store.CreateOAuthProviderSpec{
		Slug: "native-app", Type: "oidc", DisplayName: "Native",
		Issuer: "https://idp.example.com", ClientID: "native",
		TokenAuth: "none", ClientSecret: "must-not-exist", SingleTenantIssuer: true,
	}
	if _, err := store.CreateOAuthProvider(ctx, pool, box, pub); !errors.Is(err, store.ErrOAuthProviderInvalid) {
		t.Fatalf("none+secret: err = %v, want ErrOAuthProviderInvalid", err)
	}

	// token_auth='none' WITHOUT secret is the legal public-client shape —
	// works even with a nil sealbox (no key configured), and no secret row.
	pub.ClientSecret = ""
	p, err := store.CreateOAuthProvider(ctx, pool, nil, pub)
	if err != nil {
		t.Fatalf("public client create: %v", err)
	}
	if p.HasSecret {
		t.Error("public client reports has_secret=true")
	}
	if exists, _ := store.SecretExists(ctx, pool, store.OAuthProviderSecretName("native-app"), store.GlobalScope); exists {
		t.Fatal("public client grew a sealed secret")
	}
	// Its delete sweeps nothing but still succeeds (DeleteSecret tolerates absence).
	if deleted, err := store.DeleteOAuthProvider(ctx, pool, "native-app"); err != nil || !deleted {
		t.Fatalf("public client delete: deleted=%v err=%v", deleted, err)
	}

	// Migration-100 slug CHECK (uppercase / ':') maps to the invalid sentinel —
	// the DB regex is the single authoritative format gate (no Go copy).
	for _, slug := range []string{"UPPER", "has:colon", "-leading-dash"} {
		bad := store.CreateOAuthProviderSpec{
			Slug: slug, Type: "oidc", DisplayName: "x",
			Issuer: "https://idp.example.com", ClientID: "c",
			ClientSecret: "s", SingleTenantIssuer: true,
		}
		if _, err := store.CreateOAuthProvider(ctx, pool, box, bad); !errors.Is(err, store.ErrOAuthProviderInvalid) {
			t.Errorf("slug %q: err = %v, want ErrOAuthProviderInvalid (CHECK mapped)", slug, err)
		}
	}
}
