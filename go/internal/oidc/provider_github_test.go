package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newGitHubAPI mocks api.github.com with a fixed user and email list.
func newGitHubAPI(t *testing.T, emails []githubEmail) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-token" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(githubUser{ID: 583231, Login: "octocat", Name: "The Octocat"})
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-token" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(emails)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestGitHubProvider(srv *httptest.Server) Provider {
	return NewGitHub(GitHubConfig{APIBase: srv.URL, Client: srv.Client()})
}

// Default endpoints point at github.com; jwksURL stays empty (no ID token).
func TestGitHubProviderDefaultEndpoints(t *testing.T) {
	p := NewGitHub(GitHubConfig{})
	authURL, tokenURL, jwksURL, userinfoURL, err := p.Endpoints(context.Background())
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if authURL != defaultGitHubAuthURL || tokenURL != defaultGitHubTokenURL {
		t.Errorf("endpoints = %q %q", authURL, tokenURL)
	}
	if jwksURL != "" {
		t.Errorf("jwksURL = %q, want empty (no ID token)", jwksURL)
	}
	if userinfoURL != defaultGitHubAPIBase+"/user" {
		t.Errorf("userinfoURL = %q", userinfoURL)
	}
}

// W3 gate: userinfo mapping — numeric id becomes the subject, the primary
// VERIFIED email is taken, the display name comes from the profile.
func TestGitHubProviderIdentityMapping(t *testing.T) {
	srv := newGitHubAPI(t, []githubEmail{
		{Email: "old@example.test", Verified: true, Primary: false},
		{Email: "octocat@example.test", Verified: true, Primary: true},
	})
	p := newTestGitHubProvider(srv)

	id, err := p.Identity(context.Background(), Tokens{AccessToken: "gh-token"}, "")
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	want := ExternalIdentity{
		Issuer:        GitHubIssuer,
		Subject:       "583231",
		Email:         "octocat@example.test",
		DisplayName:   "The Octocat",
		EmailVerified: true,
	}
	if id != want {
		t.Fatalf("identity = %+v, want %+v", id, want)
	}
}

// W3 gate: verified=false → email stays empty.
func TestGitHubProviderUnverifiedEmailDropped(t *testing.T) {
	srv := newGitHubAPI(t, []githubEmail{
		{Email: "octocat@example.test", Verified: false, Primary: true},
	})
	p := newTestGitHubProvider(srv)

	id, err := p.Identity(context.Background(), Tokens{AccessToken: "gh-token"}, "")
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.Email != "" || id.EmailVerified {
		t.Fatalf("Email = %q verified=%v, want empty/false", id.Email, id.EmailVerified)
	}
	if id.Subject != "583231" {
		t.Errorf("Subject = %q", id.Subject)
	}
}

// Non-primary verified email is the fallback when the primary is unverified.
func TestGitHubProviderVerifiedFallbackEmail(t *testing.T) {
	srv := newGitHubAPI(t, []githubEmail{
		{Email: "primary@example.test", Verified: false, Primary: true},
		{Email: "secondary@example.test", Verified: true, Primary: false},
	})
	p := newTestGitHubProvider(srv)

	id, err := p.Identity(context.Background(), Tokens{AccessToken: "gh-token"}, "")
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.Email != "secondary@example.test" || !id.EmailVerified {
		t.Fatalf("Email = %q verified=%v", id.Email, id.EmailVerified)
	}
}

// Bad access token (upstream 401) → hard reject.
func TestGitHubProviderBadTokenRejected(t *testing.T) {
	srv := newGitHubAPI(t, nil)
	p := newTestGitHubProvider(srv)

	if _, err := p.Identity(context.Background(), Tokens{AccessToken: "wrong"}, ""); err == nil {
		t.Fatal("want reject on 401, got nil")
	}
}

// Missing access token → reject before any network call.
func TestGitHubProviderMissingAccessToken(t *testing.T) {
	p := NewGitHub(GitHubConfig{APIBase: "http://127.0.0.1:1"})
	if _, err := p.Identity(context.Background(), Tokens{IDToken: "x"}, ""); err == nil {
		t.Fatal("want reject without access_token, got nil")
	}
}

// Failing /user/emails is a hard reject, not degraded data (fail-closed:
// a missing user:email scope must surface as an error).
func TestGitHubProviderEmailsEndpointFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(githubUser{ID: 1, Login: "x"})
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "requires user:email scope", http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := newTestGitHubProvider(srv)

	if _, err := p.Identity(context.Background(), Tokens{AccessToken: "gh-token"}, ""); err == nil {
		t.Fatal("want reject when /user/emails fails, got nil")
	}
}
