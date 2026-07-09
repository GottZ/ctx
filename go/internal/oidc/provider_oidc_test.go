package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// newIDPServer spins up a mock IdP serving discovery + JWKS. issuerOverride
// lets tests serve a document claiming a FOREIGN issuer (substitution probe);
// empty means "honest" (issuer = server URL).
func newIDPServer(t *testing.T, issuerOverride string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer := srv.URL
		if issuerOverride != "" {
			issuer = issuerOverride
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuer,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
			"userinfo_endpoint":      srv.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwksFor(testKey, testKid))
	})
	return srv
}

func newTestOIDCProvider(t *testing.T, srv *httptest.Server, filter *ClaimFilter) Provider {
	t.Helper()
	p, err := NewOIDC(NewCache(Options{Client: srv.Client()}), OIDCConfig{
		Issuer:       srv.URL,
		ClientID:     testClientID,
		IDTokenAlgs:  []string{"RS256"},
		AllowedClaim: filter,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return p
}

// signFor signs base claims with iss/aud fixed to the mock IdP.
func signFor(t *testing.T, srv *httptest.Server, mut func(jwt.MapClaims)) string {
	t.Helper()
	c := baseClaims()
	c["iss"] = srv.URL
	if mut != nil {
		mut(c)
	}
	return signRS256(t, testKey, testKid, c)
}

// W3 gate: discovered endpoints are returned when doc.issuer matches.
func TestOIDCProviderEndpoints(t *testing.T) {
	srv := newIDPServer(t, "")
	p := newTestOIDCProvider(t, srv, nil)

	authURL, tokenURL, jwksURL, userinfoURL, err := p.Endpoints(context.Background())
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if authURL != srv.URL+"/authorize" || tokenURL != srv.URL+"/token" ||
		jwksURL != srv.URL+"/jwks" || userinfoURL != srv.URL+"/userinfo" {
		t.Fatalf("endpoints = %q %q %q %q", authURL, tokenURL, jwksURL, userinfoURL)
	}
}

// W3 gate (substitution guard): a discovery document claiming a foreign
// issuer is rejected BEFORE its endpoints are used — on both paths.
func TestOIDCProviderDiscoveryIssuerMismatch(t *testing.T) {
	srv := newIDPServer(t, "https://foreign.example.test")
	p := newTestOIDCProvider(t, srv, nil)

	if _, _, _, _, err := p.Endpoints(context.Background()); err == nil {
		t.Fatal("Endpoints: want issuer-mismatch reject, got nil")
	}
	_, err := p.Identity(context.Background(), Tokens{IDToken: signFor(t, srv, nil)}, testNonce)
	if err == nil {
		t.Fatal("Identity: want issuer-mismatch reject, got nil")
	}
}

// W3 gate: full OIDC identity path — valid token → correct ExternalIdentity.
func TestOIDCProviderIdentityValid(t *testing.T) {
	srv := newIDPServer(t, "")
	p := newTestOIDCProvider(t, srv, nil)

	id, err := p.Identity(context.Background(), Tokens{IDToken: signFor(t, srv, nil)}, testNonce)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	want := ExternalIdentity{
		Issuer:        srv.URL,
		Subject:       "user-42",
		Email:         "user@example.test",
		DisplayName:   "User FortyTwo",
		EmailVerified: true,
	}
	if id != want {
		t.Fatalf("identity = %+v, want %+v", id, want)
	}
}

// Missing id_token in the token response → reject (no soft fallback to
// userinfo — the OIDC path verifies or dies).
func TestOIDCProviderIdentityWithoutIDToken(t *testing.T) {
	srv := newIDPServer(t, "")
	p := newTestOIDCProvider(t, srv, nil)

	if _, err := p.Identity(context.Background(), Tokens{AccessToken: "at"}, testNonce); err == nil {
		t.Fatal("want reject without id_token, got nil")
	}
}

// Wrong nonce travels through the provider path as a reject.
func TestOIDCProviderIdentityWrongNonce(t *testing.T) {
	srv := newIDPServer(t, "")
	p := newTestOIDCProvider(t, srv, nil)

	if _, err := p.Identity(context.Background(), Tokens{IDToken: signFor(t, srv, nil)}, "other-nonce"); err == nil {
		t.Fatal("want nonce reject, got nil")
	}
}

// email_verified=false → Email empty on the resulting identity.
func TestOIDCProviderIdentityUnverifiedEmail(t *testing.T) {
	srv := newIDPServer(t, "")
	p := newTestOIDCProvider(t, srv, nil)

	token := signFor(t, srv, func(c jwt.MapClaims) { c["email_verified"] = false })
	id, err := p.Identity(context.Background(), Tokens{IDToken: token}, testNonce)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.Email != "" || id.EmailVerified {
		t.Fatalf("Email = %q verified=%v, want empty/false", id.Email, id.EmailVerified)
	}
}

// W3 gate: allowed_claim filter on the provider — contained value passes,
// missing and foreign values reject AFTER an otherwise valid verify.
func TestOIDCProviderAllowedClaimFilter(t *testing.T) {
	srv := newIDPServer(t, "")
	filter := &ClaimFilter{Claim: "tid", Values: []string{"tenant-a"}}
	p := newTestOIDCProvider(t, srv, filter)

	// contained → pass
	token := signFor(t, srv, func(c jwt.MapClaims) { c["tid"] = "tenant-a" })
	if _, err := p.Identity(context.Background(), Tokens{IDToken: token}, testNonce); err != nil {
		t.Fatalf("contained claim value: %v", err)
	}
	// foreign → reject
	token = signFor(t, srv, func(c jwt.MapClaims) { c["tid"] = "tenant-x" })
	if _, err := p.Identity(context.Background(), Tokens{IDToken: token}, testNonce); err == nil {
		t.Fatal("foreign claim value: want reject")
	}
	// missing → reject
	token = signFor(t, srv, nil)
	if _, err := p.Identity(context.Background(), Tokens{IDToken: token}, testNonce); err == nil {
		t.Fatal("missing claim: want reject")
	}
}

// Discovery down → hard error, no verify bypass (fail-closed).
func TestOIDCProviderDiscoveryDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p, err := NewOIDC(NewCache(Options{Client: srv.Client()}), OIDCConfig{
		Issuer:      srv.URL,
		ClientID:    testClientID,
		IDTokenAlgs: []string{"RS256"},
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	if _, _, _, _, err := p.Endpoints(context.Background()); err == nil {
		t.Fatal("Endpoints: want error while discovery is down")
	}
	if _, err := p.Identity(context.Background(), Tokens{IDToken: "x"}, testNonce); err == nil {
		t.Fatal("Identity: want error while discovery is down")
	}
}

// Construction fails closed on config gaps.
func TestNewOIDCConfigValidation(t *testing.T) {
	cache := NewCache(Options{})
	if _, err := NewOIDC(nil, OIDCConfig{Issuer: "x", ClientID: "y", IDTokenAlgs: []string{"RS256"}}); err == nil {
		t.Error("nil cache: want error")
	}
	if _, err := NewOIDC(cache, OIDCConfig{ClientID: "y", IDTokenAlgs: []string{"RS256"}}); err == nil {
		t.Error("missing issuer: want error")
	}
	if _, err := NewOIDC(cache, OIDCConfig{Issuer: "x", IDTokenAlgs: []string{"RS256"}}); err == nil {
		t.Error("missing client_id: want error")
	}
	if _, err := NewOIDC(cache, OIDCConfig{Issuer: "x", ClientID: "y"}); err == nil {
		t.Error("missing algs: want error")
	}
}
