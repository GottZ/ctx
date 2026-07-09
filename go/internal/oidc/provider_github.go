// GitHub provider: the deliberately divergent case that proves the
// abstraction is generic (design/04 §4.1/§4.3.8, E4). A classic GitHub OAuth
// app issues NO ID token (no nonce, no JWKS) — identity comes from the
// userinfo API: access_token → GET /user (+/user/emails). Replay/CSRF
// protection therefore rests entirely on state single-use + one-time code
// (design/04 F6), which live in the flow wave (W6).

package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// GitHubIssuer is the fixed issuer value stored in
// context_external_identities for GitHub identities (design/04 §4.3.8).
const GitHubIssuer = "https://github.com"

// Fixed default endpoints of github.com. Overridable via GitHubConfig for
// tests (httptest) and GitHub Enterprise instances.
const (
	defaultGitHubAuthURL  = "https://github.com/login/oauth/authorize"
	defaultGitHubTokenURL = "https://github.com/login/oauth/access_token" //nolint:gosec // endpoint URL, not a credential
	defaultGitHubAPIBase  = "https://api.github.com"
)

// GitHubConfig mirrors the relevant fields of a context_oauth_providers row
// (migration 100) for type='github'. Zero values fall back to github.com.
type GitHubConfig struct {
	// Issuer is stored on the resulting identity. Default: GitHubIssuer.
	Issuer string
	// AuthURL / TokenURL / APIBase override the fixed endpoints
	// (auth_url/token_url/userinfo_url columns; httptest injection).
	AuthURL  string
	TokenURL string
	APIBase  string
	// Client is used for the userinfo calls. Default: client with
	// DefaultFetchTimeout.
	Client *http.Client
}

type githubProvider struct {
	cfg GitHubConfig
}

// NewGitHub builds a GitHub provider with fixed endpoints (no discovery).
func NewGitHub(cfg GitHubConfig) Provider {
	if cfg.Issuer == "" {
		cfg.Issuer = GitHubIssuer
	}
	if cfg.AuthURL == "" {
		cfg.AuthURL = defaultGitHubAuthURL
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = defaultGitHubTokenURL
	}
	if cfg.APIBase == "" {
		cfg.APIBase = defaultGitHubAPIBase
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: DefaultFetchTimeout}
	}
	return &githubProvider{cfg: cfg}
}

func (p *githubProvider) Endpoints(_ context.Context) (authURL, tokenURL, jwksURL, userinfoURL string, err error) {
	// jwksURL stays empty: there is no ID token to verify.
	return p.cfg.AuthURL, p.cfg.TokenURL, "", p.cfg.APIBase + "/user", nil
}

// githubUser is the subset of GET /user we consume. The numeric id is the
// STABLE subject — logins can be renamed and re-registered, ids cannot.
type githubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

// githubEmail is one entry of GET /user/emails.
type githubEmail struct {
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
	Primary  bool   `json:"primary"`
}

// Identity resolves the access token to a verified identity via the GitHub
// API. expectedNonce is ignored — no ID token exists to carry one (F6).
// Both API calls are hard requirements: a failing /user/emails is a reject,
// not a silent "no email" (fail-closed; a missing user:email scope must
// surface as a config error, not as degraded data).
func (p *githubProvider) Identity(ctx context.Context, tokens Tokens, _ string) (ExternalIdentity, error) {
	if tokens.AccessToken == "" {
		return ExternalIdentity{}, errors.New("oidc: github: token response carries no access_token")
	}

	var user githubUser
	if err := p.getJSON(ctx, p.cfg.APIBase+"/user", tokens.AccessToken, &user); err != nil {
		return ExternalIdentity{}, err
	}
	if user.ID == 0 {
		return ExternalIdentity{}, errors.New("oidc: github: userinfo response carries no id")
	}

	var emails []githubEmail
	if err := p.getJSON(ctx, p.cfg.APIBase+"/user/emails", tokens.AccessToken, &emails); err != nil {
		return ExternalIdentity{}, err
	}
	// email only when verified=true: prefer the primary verified address,
	// fall back to the first verified one, otherwise stay empty.
	email := ""
	for _, e := range emails {
		if !e.Verified {
			continue
		}
		if e.Primary {
			email = e.Email
			break
		}
		if email == "" {
			email = e.Email
		}
	}

	displayName := user.Name
	if displayName == "" {
		displayName = user.Login
	}
	return ExternalIdentity{
		Issuer:        p.cfg.Issuer,
		Subject:       strconv.FormatInt(user.ID, 10),
		Email:         email,
		DisplayName:   displayName,
		EmailVerified: email != "",
	}, nil
}

// getJSON performs an authenticated GitHub API GET with timeout, body cap
// and non-200 reject — same fetch discipline as the discovery cache.
func (p *githubProvider) getJSON(ctx context.Context, url, accessToken string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("oidc: github: build request for %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: github: fetch %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return fmt.Errorf("oidc: github: read %s: %w", url, err)
	}
	if len(body) > maxFetchBytes {
		return fmt.Errorf("oidc: github: fetch %s: response exceeds %d bytes", url, maxFetchBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: github: fetch %s returned %d: %s", url, resp.StatusCode, truncate(body, 256))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("oidc: github: decode %s: %w", url, err)
	}
	return nil
}

// Compile-time interface checks.
var (
	_ Provider = (*oidcProvider)(nil)
	_ Provider = (*githubProvider)(nil)
)
