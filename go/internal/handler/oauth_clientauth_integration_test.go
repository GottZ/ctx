//go:build integration

// Integration gates for OAuth wave S6 (design/03 §7 W03-7, negativ-geprobt)
// against a real PG18 testcontainer:
//
//   - per-client redirect allowlist is EXCLUSIVE: a client with registered
//     redirect_uris authorizes its own URI (which is NOT on the static S2
//     list — proves the per-client path) and is rejected for the static
//     claude.ai URI (no fallback escape from a narrow registration)
//   - empty registration keeps the static S2 fallback (claude.ai → 302,
//     attacker host → 400)
//   - confidential client (client_secret_post): correct secret (form) →
//     token; wrong secret → 401 invalid_client; missing secret → 401;
//     correct secret via Basic → token; Basic username contradicting the
//     form client_id → 401
//   - the Basic 401 carries WWW-Authenticate (RFC 6749 §5.2 MUST)
//   - refresh grant: wrong secret → 401 WITHOUT killing the family — the
//     live refresh still rotates afterwards (no family-DoS oracle)
//   - public `none` client presenting a secret → 401 (fail-closed)
//   - unregistered client_id presenting a secret → 401 invalid_client
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestOAuthClientAuth -count=1 -v
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// caToken posts one /token request; basicAuth ("user:secret") is optional.
func caToken(t *testing.T, oh *OAuthHandler, form url.Values, basicUser, basicSecret string) (int, map[string]any, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicUser != "" || basicSecret != "" {
		req.SetBasicAuth(basicUser, basicSecret)
	}
	rec := httptest.NewRecorder()
	oh.Token(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Header()
}

func TestOAuthClientAuthS6_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('ca-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	plainKey := "ee55" + strings.Repeat("ab", 30)
	keyHash := sha256.Sum256([]byte(plainKey))
	var tenantID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ('ca-x','ca-x') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('ca-scope',$1::uuid)`, tenantID); err != nil {
		t.Fatalf("map scope: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 VALUES (encode($1,'hex'), 'ca-key', 'ca-scope', $2::uuid, $3::uuid)`,
		keyHash[:], tenantID, principalID); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	oh := NewOAuthHandler(pool)
	verifier := strings.Repeat("w", 64)
	chal := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(chal[:])

	authorizeForm := func(clientID, redirectURI string) url.Values {
		return url.Values{
			"api_key":               {plainKey},
			"client_id":             {clientID},
			"redirect_uri":          {redirectURI},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}
	}
	tokenForm := func(clientID, code string, extra url.Values) url.Values {
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"code_verifier": {verifier},
		}
		if clientID != "" {
			form.Set("client_id", clientID)
		}
		for k, vs := range extra {
			form[k] = vs
		}
		return form
	}

	// --- per-client allowlist is exclusive ----------------------------------
	registeredURI := "https://app.example/cb" // NOT on the static S2 list
	regClient, _, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
		Label: "ca-registered", RedirectURIs: []string{registeredURI},
		TokenEndpointAuthMethod: "none", Source: "admin",
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	if st, code := opAuthorize(t, oh, authorizeForm(regClient.ClientID, registeredURI)); st != http.StatusFound || code == "" {
		t.Fatalf("registered URI: got %d, want 302 with code (per-client path)", st)
	}
	if st, _ := opAuthorize(t, oh, authorizeForm(regClient.ClientID, "https://claude.ai/api/mcp/auth_callback")); st != http.StatusBadRequest {
		t.Fatalf("static URI for registered client: got %d, want 400 (no fallback escape)", st)
	}

	// --- empty registration keeps the static fallback -----------------------
	fbClient, _, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
		Label: "ca-fallback", TokenEndpointAuthMethod: "none", Source: "admin",
	})
	if err != nil {
		t.Fatalf("register fallback client: %v", err)
	}
	if st, _ := opAuthorize(t, oh, authorizeForm(fbClient.ClientID, "https://claude.ai/api/mcp/auth_callback")); st != http.StatusFound {
		t.Fatalf("static fallback: got %d, want 302", st)
	}
	if st, _ := opAuthorize(t, oh, authorizeForm(fbClient.ClientID, "https://attacker.example/cb")); st != http.StatusBadRequest {
		t.Fatalf("attacker URI on fallback: got %d, want 400", st)
	}

	// --- confidential client: secret enforcement ----------------------------
	confClient, confSecret, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
		Label: "ca-confidential", RedirectURIs: []string{registeredURI},
		TokenEndpointAuthMethod: "client_secret_post", Source: "admin",
	})
	if err != nil {
		t.Fatalf("register confidential client: %v", err)
	}
	if confSecret == "" {
		t.Fatal("confidential registration returned no secret")
	}
	getCode := func() string {
		t.Helper()
		st, code := opAuthorize(t, oh, authorizeForm(confClient.ClientID, registeredURI))
		if st != http.StatusFound || code == "" {
			t.Fatalf("authorize for confidential client: got %d, want 302 with code", st)
		}
		return code
	}

	// Wrong secret → 401 invalid_client.
	if st, body, _ := caToken(t, oh, tokenForm(confClient.ClientID, getCode(),
		url.Values{"client_secret": {"definitely-wrong"}}), "", ""); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("wrong secret: got %d %v, want 401 invalid_client", st, body["error"])
	}
	// Missing secret → 401 invalid_client.
	if st, body, _ := caToken(t, oh, tokenForm(confClient.ClientID, getCode(), nil), "", ""); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("missing secret: got %d %v, want 401 invalid_client", st, body["error"])
	}
	// Correct secret via form (client_secret_post) → token pair.
	st, body, _ := caToken(t, oh, tokenForm(confClient.ClientID, getCode(),
		url.Values{"client_secret": {confSecret}}), "", "")
	if st != http.StatusOK {
		t.Fatalf("correct secret (post): got %d %v, want 200", st, body)
	}
	access, _ := body["access_token"].(string)
	refresh, _ := body["refresh_token"].(string)
	if !strings.HasPrefix(access, store.AccessTokenPrefix) || refresh == "" {
		t.Fatalf("correct secret (post): unexpected pair %q / %q", access, refresh)
	}
	// Correct secret via Basic auth → token pair.
	if st, body, _ := caToken(t, oh, tokenForm("", getCode(), nil), confClient.ClientID, confSecret); st != http.StatusOK {
		t.Fatalf("correct secret (basic): got %d %v, want 200", st, body)
	}
	// Basic username contradicting the form client_id → 401 + WWW-Authenticate.
	stC, bodyC, hdrC := caToken(t, oh, tokenForm(confClient.ClientID, getCode(), nil), fbClient.ClientID, confSecret)
	if stC != http.StatusUnauthorized || bodyC["error"] != "invalid_client" {
		t.Fatalf("basic/form mismatch: got %d %v, want 401 invalid_client", stC, bodyC["error"])
	}
	if hdrC.Get("WWW-Authenticate") == "" {
		t.Fatal("basic 401 lacks WWW-Authenticate (RFC 6749 §5.2 MUST)")
	}

	// --- refresh grant: wrong secret must not kill the family ---------------
	refreshForm := func(secret string) url.Values {
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refresh},
			"client_id":     {confClient.ClientID},
		}
		if secret != "" {
			form.Set("client_secret", secret)
		}
		return form
	}
	if st, body, _ := caToken(t, oh, refreshForm("definitely-wrong"), "", ""); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("refresh wrong secret: got %d %v, want 401 invalid_client", st, body["error"])
	}
	if st, body, _ := caToken(t, oh, refreshForm(confSecret), "", ""); st != http.StatusOK {
		t.Fatalf("refresh after failed auth attempt: got %d %v, want 200 (family must survive a 401)", st, body)
	}

	// --- public `none` client presenting a secret → fail-closed -------------
	fbCodeSt, fbCode := opAuthorize(t, oh, authorizeForm(fbClient.ClientID, "https://claude.ai/api/mcp/auth_callback"))
	if fbCodeSt != http.StatusFound {
		t.Fatalf("authorize for none client: got %d, want 302", fbCodeSt)
	}
	if st, body, _ := caToken(t, oh, tokenForm(fbClient.ClientID, fbCode,
		url.Values{"client_secret": {"should-not-be-here"}}), "", ""); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("none client with secret: got %d %v, want 401 invalid_client", st, body["error"])
	}

	// --- unregistered client_id presenting a secret → 401 -------------------
	if st, body, _ := caToken(t, oh, tokenForm("ctx_never_registered", "no-code",
		url.Values{"client_secret": {"x"}}), "", ""); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("unregistered client with secret: got %d %v, want 401 invalid_client", st, body["error"])
	}
}
