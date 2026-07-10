//go:build integration

// Integration gates for OAuth wave L5 (design/04 §7 W6, negativ-geprobt)
// against a real PG18 testcontainer (migrations through 101) and a mock OIDC
// IdP (httptest: discovery + JWKS + token endpoint, RSA test key):
//
//   - e2e happy path: login → 302 (cookie = row id ≠ URL state param) →
//     callback → identity row refreshed (verified_at/last_login_at/email/
//     display_name) + principal_id in the handover stub; token endpoint saw
//     the matching PKCE verifier, client_secret and redirect_uri
//   - unknown slug → 404; inactive → 404; multi-tenant issuer without
//     allowed_claim → 404 (config honesty, seeded via raw SQL past the
//     store's create guard)
//   - reused state (second callback) → reject, no second exchange
//   - state param mismatch → reject
//   - URL {provider} ≠ sealed provider_slug → reject (mix-up, F2)
//   - IdP error param → structured error, NO exchange
//   - RFC 9207 iss mismatch → reject, NO exchange
//   - missing code / missing cookie → reject
//   - unknown verified (issuer,subject) → 403 admin-invite, no row created
//   - return_to '//evil.example' and '/\evil' → sealed as "/" (F5)
//   - email_verified=false → login succeeds, email NOT refreshed
//   - per-IP rate limit N+1 → 429 (F7)
//   - github e2e: authorize URL without PKCE/nonce (F6), exchange without
//     code_verifier, userinfo-based identity → principal_id
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSSO -count=1 -v
package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/oidc"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	ssoTestClientID = "ctx-client"
	ssoTestSecret   = "client-secret-0123456789"
	ssoTestCode     = "good-code"
	ssoRedirectBase = "https://ctx.example"
)

// mockIdP is a scriptable OIDC IdP: discovery + JWKS + token endpoint. The
// test steers the issued ID token through the public fields.
type mockIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	// token claims for the next exchange
	subject       string
	email         string
	emailVerified bool
	name          string
	nonce         string // captured from the authorize URL by the test

	// recorded state of the last exchange
	exchanges     atomic.Int64
	lastForm      url.Values
	lastFormReady atomic.Bool
}

func newMockIdP(t *testing.T) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	idp := &mockIdP{key: key, subject: "alice-sub", email: "alice@example.com", emailVerified: true, name: "Alice A"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 idp.srv.URL,
			"authorization_endpoint": idp.srv.URL + "/authorize",
			"token_endpoint":         idp.srv.URL + "/token",
			"jwks_uri":               idp.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := &idp.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "kid": "test-key", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idp.exchanges.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		idp.lastForm = r.PostForm
		idp.lastFormReady.Store(true)
		if r.PostForm.Get("code") != ssoTestCode {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		now := time.Now()
		claims := jwt.MapClaims{
			"iss": idp.srv.URL, "sub": idp.subject, "aud": ssoTestClientID,
			"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
			"nonce": idp.nonce, "name": idp.name,
			"email": idp.email, "email_verified": idp.emailVerified,
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "test-key"
		signed, err := tok.SignedString(idp.key)
		if err != nil {
			http.Error(w, "sign", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "id_token": signed, "token_type": "Bearer"})
	})

	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

// newSSOTestHandler builds the handler with a PLAIN http client (httptest
// lives on loopback — the production ssrfguard client refuses that by
// design, probed in TestNewSSOHandlerDefaultClientBlocksLoopback).
func newSSOTestHandler(pool *pgxpool.Pool, box *sealbox.Box, limit int) (*SSOHandler, chi.Router) {
	client := &http.Client{Timeout: 5 * time.Second}
	h := &SSOHandler{
		pool:    pool,
		openBox: func() (*sealbox.Box, error) { return box, nil },
		client:  client,
		cache:   oidc.NewCache(oidc.Options{Client: client}),
		limiter: newIPLimiter(limit, time.Minute),
		session: NewSessionHandler(pool), // R5: callback issues the web session
	}
	r := chi.NewRouter()
	r.Get("/auth/login/{provider}", h.HandleLogin)
	r.Get("/auth/callback/{provider}", h.HandleCallback)
	return h, r
}

func newSSOTestBox(t *testing.T) *sealbox.Box {
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

// seedIdentity links (issuer, subject) to a fresh principal and returns the
// principal id.
func seedIdentity(t *testing.T, pool *pgxpool.Pool, issuer, subject, email string) string {
	t.Helper()
	ctx := context.Background()
	var pid string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('seeded') RETURNING id`).Scan(&pid); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_external_identities (principal_id, provider, issuer, subject, email)
		 VALUES ($1, 'oidc', $2, $3, NULLIF($4, ''))`, pid, issuer, subject, email); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	return pid
}

// doLogin fires GET /auth/login/{slug} and returns the state cookie value
// and the parsed authorize redirect URL.
func doLogin(t *testing.T, router chi.Router, slug, query string) (cookieVal string, authorize *url.URL) {
	t.Helper()
	req := httptest.NewRequest("GET", "/auth/login/"+slug+query, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("login: status %d, want 302 (body %s)", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == ssoStateCookie {
			if !c.HttpOnly || c.Path != "/auth" || c.SameSite != http.SameSiteLaxMode || !c.Secure {
				t.Fatalf("state cookie attributes wrong: %+v", c)
			}
			cookieVal = c.Value
		}
	}
	if cookieVal == "" {
		t.Fatalf("login set no state cookie")
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return cookieVal, loc
}

// doCallback fires GET /auth/callback/{slug} with the given cookie and query.
func doCallback(t *testing.T, router chi.Router, slug, cookieVal, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/auth/callback/"+slug+query, nil)
	if cookieVal != "" {
		req.AddCookie(&http.Cookie{Name: ssoStateCookie, Value: cookieVal})
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestSSOLoginFlowOIDC_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := newSSOTestBox(t)
	idp := newMockIdP(t)
	ctx := context.Background()

	if _, err := store.CreateOAuthProvider(ctx, pool, box, store.CreateOAuthProviderSpec{
		Slug: "corp", Type: "oidc", DisplayName: "Corp SSO",
		Issuer: idp.srv.URL, ClientID: ssoTestClientID, ClientSecret: ssoTestSecret,
		RedirectBase: ssoRedirectBase, SingleTenantIssuer: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	alicePID := seedIdentity(t, pool, idp.srv.URL, "alice-sub", "old@example.com")
	// R5: the callback issues a real session — alice needs an active key
	// (the key-less shape is probed by TestSSOSessionR5_Integration).
	aliceKey := "aa77" + strings.Repeat("cd", 30)
	_, _ = otSeedTenantKey(t, pool, "sso-x", "sso-scope-x", aliceKey, alicePID)

	_, router := newSSOTestHandler(pool, box, 1000)

	t.Run("happy_e2e", func(t *testing.T) {
		cookieVal, loc := doLogin(t, router, "corp", "?return_to=/blocks%3Fq%3Dtest")
		q := loc.Query()

		// Double-UUID (F1): the URL state param must NOT be the cookie value.
		if q.Get("state") == cookieVal {
			t.Fatalf("URL state equals cookie row id — double-UUID principle violated")
		}
		if q.Get("state") == q.Get("nonce") {
			t.Fatalf("state equals nonce — two distinct values required")
		}
		wantRedirect := ssoRedirectBase + "/auth/callback/corp"
		if q.Get("redirect_uri") != wantRedirect {
			t.Fatalf("redirect_uri = %q, want %q", q.Get("redirect_uri"), wantRedirect)
		}
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
			t.Fatalf("OIDC login without PKCE S256 challenge")
		}
		idp.nonce = q.Get("nonce")

		rr := doCallback(t, router, "corp", cookieVal, "?code="+ssoTestCode+"&state="+q.Get("state")+"&iss="+url.QueryEscape(idp.srv.URL))
		// R5: the callback issues the web session and redirects to return_to.
		if rr.Code != http.StatusFound {
			t.Fatalf("callback: status %d, want 302 (body %s)", rr.Code, rr.Body.String())
		}
		if loc := rr.Header().Get("Location"); loc != "/blocks?q=test" {
			t.Fatalf("callback Location = %q, want /blocks?q=test", loc)
		}
		if loc := rr.Header().Get("Location"); strings.Contains(loc, "ctxt_") || strings.Contains(loc, "ctxr_") {
			t.Fatalf("callback Location leaks a token: %s", loc)
		}
		res := rr.Result()
		defer res.Body.Close()
		var ssoSess, ssoRef string
		for _, c := range res.Cookies() {
			switch c.Name {
			case sessionCookieName:
				ssoSess = c.Value
			case refreshCookieName:
				ssoRef = c.Value
			}
		}
		if ssoSess == "" || !strings.HasPrefix(ssoRef, store.RefreshTokenPrefix) {
			t.Fatalf("callback must set session + ctxr_ refresh cookies (session=%q ref-prefix=%v)", ssoSess, strings.HasPrefix(ssoRef, "ctxr_"))
		}
		// The session materialises alice's key identity end-to-end.
		if st, body := otAuthProbe(t, pool, func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: ssoSess})
		}); st != http.StatusOK || !strings.Contains(body, "sso-scope-x") {
			t.Fatalf("SSO session probe: status %d body %s, want 200 with alice's scope", st, body)
		}

		// The exchange carried the matching PKCE verifier + secret + redirect_uri.
		if !idp.lastFormReady.Load() {
			t.Fatalf("token endpoint never hit")
		}
		verifier := idp.lastForm.Get("code_verifier")
		if verifier == "" {
			t.Fatalf("exchange without code_verifier (OIDC must use PKCE)")
		}
		if got := s256of(verifier); got != q.Get("code_challenge") {
			t.Fatalf("code_verifier does not match the login code_challenge")
		}
		if idp.lastForm.Get("client_secret") != ssoTestSecret {
			t.Fatalf("exchange without the unsealed client_secret (client_secret_post)")
		}
		if idp.lastForm.Get("redirect_uri") != wantRedirect {
			t.Fatalf("exchange redirect_uri = %q, want %q", idp.lastForm.Get("redirect_uri"), wantRedirect)
		}

		// Identity row refreshed: verified_at/last_login_at stamped, email +
		// display_name updated (email_verified=true).
		var email, displayName string
		var verifiedAt, lastLoginAt *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(email,''), COALESCE(display_name,''), verified_at, last_login_at
			   FROM context_external_identities WHERE issuer=$1 AND subject='alice-sub'`,
			idp.srv.URL).Scan(&email, &displayName, &verifiedAt, &lastLoginAt); err != nil {
			t.Fatalf("read identity row: %v", err)
		}
		if verifiedAt == nil || lastLoginAt == nil {
			t.Fatalf("verified_at/last_login_at not stamped: %v / %v", verifiedAt, lastLoginAt)
		}
		if email != "alice@example.com" || displayName != "Alice A" {
			t.Fatalf("refresh wrote email=%q display_name=%q", email, displayName)
		}

		// Replay: the SAME callback again must reject (state single-use) and
		// must NOT reach the token endpoint again.
		before := idp.exchanges.Load()
		rr2 := doCallback(t, router, "corp", cookieVal, "?code="+ssoTestCode+"&state="+q.Get("state"))
		if rr2.Code != http.StatusBadRequest {
			t.Fatalf("replayed callback: status %d, want 400", rr2.Code)
		}
		if idp.exchanges.Load() != before {
			t.Fatalf("replayed callback reached the token endpoint")
		}
	})

	t.Run("state_param_mismatch_rejected", func(t *testing.T) {
		cookieVal, _ := doLogin(t, router, "corp", "")
		before := idp.exchanges.Load()
		rr := doCallback(t, router, "corp", cookieVal, "?code="+ssoTestCode+"&state=00000000-0000-4000-8000-000000000000")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rr.Code)
		}
		if idp.exchanges.Load() != before {
			t.Fatalf("mismatching state reached the token endpoint")
		}
	})

	t.Run("provider_slug_mismatch_rejected", func(t *testing.T) {
		cookieVal, loc := doLogin(t, router, "corp", "")
		before := idp.exchanges.Load()
		// Mix-up (F2): cookie sealed for corp, callback path claims another.
		rr := doCallback(t, router, "other", cookieVal, "?code="+ssoTestCode+"&state="+loc.Query().Get("state"))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rr.Code)
		}
		if idp.exchanges.Load() != before {
			t.Fatalf("provider mix-up reached the token endpoint")
		}
	})

	t.Run("idp_error_param_no_exchange", func(t *testing.T) {
		cookieVal, loc := doLogin(t, router, "corp", "")
		before := idp.exchanges.Load()
		rr := doCallback(t, router, "corp", cookieVal, "?error=access_denied&state="+loc.Query().Get("state"))
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", rr.Code)
		}
		if idp.exchanges.Load() != before {
			t.Fatalf("IdP error param still triggered an exchange")
		}
	})

	t.Run("iss_param_mismatch_rejected", func(t *testing.T) {
		cookieVal, loc := doLogin(t, router, "corp", "")
		before := idp.exchanges.Load()
		rr := doCallback(t, router, "corp", cookieVal,
			"?code="+ssoTestCode+"&state="+loc.Query().Get("state")+"&iss="+url.QueryEscape("https://evil.example"))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rr.Code)
		}
		if idp.exchanges.Load() != before {
			t.Fatalf("RFC 9207 iss mismatch reached the token endpoint")
		}
	})

	t.Run("missing_code_rejected", func(t *testing.T) {
		cookieVal, loc := doLogin(t, router, "corp", "")
		rr := doCallback(t, router, "corp", cookieVal, "?state="+loc.Query().Get("state"))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rr.Code)
		}
	})

	t.Run("missing_cookie_rejected", func(t *testing.T) {
		_, loc := doLogin(t, router, "corp", "")
		rr := doCallback(t, router, "corp", "", "?code="+ssoTestCode+"&state="+loc.Query().Get("state"))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rr.Code)
		}
	})

	t.Run("unknown_identity_403_admin_invite", func(t *testing.T) {
		cookieVal, loc := doLogin(t, router, "corp", "")
		idp.nonce = loc.Query().Get("nonce")
		idp.subject = "mallory-sub" // verified by the IdP, but not linked
		defer func() { idp.subject = "alice-sub" }()

		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_external_identities`).Scan(&rows); err != nil {
			t.Fatalf("count: %v", err)
		}
		rr := doCallback(t, router, "corp", cookieVal, "?code="+ssoTestCode+"&state="+loc.Query().Get("state"))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403 (E4b admin-invite), body %s", rr.Code, rr.Body.String())
		}
		var after int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_external_identities`).Scan(&after); err != nil {
			t.Fatalf("count: %v", err)
		}
		if after != rows {
			t.Fatalf("unknown identity created a row (%d → %d) — E4b forbids auto-create", rows, after)
		}
	})

	t.Run("email_unverified_not_refreshed", func(t *testing.T) {
		// Reset the seeded email so the refresh (or its absence) is visible.
		if _, err := pool.Exec(ctx,
			`UPDATE context_external_identities SET email='old@example.com' WHERE subject='alice-sub'`); err != nil {
			t.Fatalf("reset email: %v", err)
		}
		cookieVal, loc := doLogin(t, router, "corp", "")
		idp.nonce = loc.Query().Get("nonce")
		idp.emailVerified = false
		defer func() { idp.emailVerified = true }()

		rr := doCallback(t, router, "corp", cookieVal, "?code="+ssoTestCode+"&state="+loc.Query().Get("state"))
		if rr.Code != http.StatusFound {
			t.Fatalf("unverified email must not block the login itself: status %d", rr.Code)
		}
		var email string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(email,'') FROM context_external_identities WHERE subject='alice-sub'`).Scan(&email); err != nil {
			t.Fatalf("read email: %v", err)
		}
		if email != "old@example.com" {
			t.Fatalf("unverified provider email overwrote the stored one: %q", email)
		}
	})

	t.Run("return_to_open_redirect_filtered", func(t *testing.T) {
		for _, evil := range []string{"//evil.example", `/\evil`} {
			cookieVal, _ := doLogin(t, router, "corp", "?return_to="+url.QueryEscape(evil))
			var sealed string
			if err := pool.QueryRow(ctx,
				`SELECT state_data->>'return_to' FROM context_sso_states WHERE id=$1`, cookieVal).Scan(&sealed); err != nil {
				t.Fatalf("read sealed state: %v", err)
			}
			if sealed != "/" {
				t.Fatalf("return_to %q sealed as %q, want \"/\" (F5)", evil, sealed)
			}
		}
	})

	t.Run("unknown_slug_404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/auth/login/nope", nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rr.Code)
		}
	})

	t.Run("inactive_404", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE context_oauth_providers SET active=false WHERE slug='corp'`); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		defer func() {
			if _, err := pool.Exec(ctx, `UPDATE context_oauth_providers SET active=true WHERE slug='corp'`); err != nil {
				t.Fatalf("reactivate: %v", err)
			}
		}()
		req := httptest.NewRequest("GET", "/auth/login/corp", nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rr.Code)
		}
	})

	t.Run("multitenant_issuer_without_claim_404", func(t *testing.T) {
		// Seeded via raw SQL: the store's create path refuses this shape, the
		// runtime check must hold independently (config honesty, §4.1).
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_oauth_providers (slug, type, display_name, issuer, client_id, single_tenant_issuer)
			 VALUES ('halfbaked', 'oidc', 'Half', 'https://mt.example', 'cid', false)`); err != nil {
			t.Fatalf("seed half-configured provider: %v", err)
		}
		req := httptest.NewRequest("GET", "/auth/login/halfbaked", nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404 (multi-tenant issuer without allowed_claim is INACTIVE)", rr.Code)
		}
	})

	t.Run("rate_limit_429", func(t *testing.T) {
		_, limitedRouter := newSSOTestHandler(pool, box, 2)
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest("GET", "/auth/login/corp", nil)
			req.RemoteAddr = "198.51.100.20:1234"
			rr := httptest.NewRecorder()
			limitedRouter.ServeHTTP(rr, req)
			if rr.Code != http.StatusFound {
				t.Fatalf("request %d: status %d, want 302", i+1, rr.Code)
			}
		}
		req := httptest.NewRequest("GET", "/auth/login/corp", nil)
		req.RemoteAddr = "198.51.100.20:1234"
		rr := httptest.NewRecorder()
		limitedRouter.ServeHTTP(rr, req)
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("request N+1: status %d, want 429 (F7)", rr.Code)
		}
	})
}

// TestSSOLoginFlowGitHub_Integration proves the divergent provider shape
// (F6/E4): no PKCE, no nonce, no ID token — identity via userinfo.
func TestSSOLoginFlowGitHub_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := newSSOTestBox(t)
	ctx := context.Background()

	var sawVerifier atomic.Bool
	var exchanges atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_ = r.ParseForm()
		if r.PostForm.Get("code_verifier") != "" {
			sawVerifier.Store(true)
		}
		if r.PostForm.Get("code") != ssoTestCode {
			http.Error(w, `{"error":"bad_verification_code"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_test", "token_type": "bearer"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "octo", "name": "Octo Cat"})
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"email": "octo@example.com", "verified": true, "primary": true},
		})
	})
	gh := httptest.NewServer(mux)
	defer gh.Close()

	if _, err := store.CreateOAuthProvider(ctx, pool, box, store.CreateOAuthProviderSpec{
		Slug: "github", Type: "github", DisplayName: "GitHub",
		Issuer: "https://github.com", ClientID: ssoTestClientID, ClientSecret: ssoTestSecret,
		RedirectBase: ssoRedirectBase, SingleTenantIssuer: true,
		Scopes:  []string{"read:user", "user:email"},
		AuthURL: gh.URL + "/login/oauth/authorize", TokenURL: gh.URL + "/login/oauth/access_token",
		UserinfoURL: gh.URL, // API base for type=github
	}); err != nil {
		t.Fatalf("create github provider: %v", err)
	}
	pid := seedIdentity(t, pool, "https://github.com", "4242", "")
	// R5: the callback issues a real session — the principal needs a key.
	_, _ = otSeedTenantKey(t, pool, "gh-x", "gh-scope-x", "bb88"+strings.Repeat("ef", 30), pid)

	_, router := newSSOTestHandler(pool, box, 1000)

	cookieVal, loc := doLogin(t, router, "github", "")
	q := loc.Query()
	if q.Get("code_challenge") != "" || q.Get("nonce") != "" {
		t.Fatalf("github authorize URL carries PKCE/nonce (F6): %s", loc.String())
	}
	if q.Get("scope") != "read:user user:email" {
		t.Fatalf("scope = %q, want space-joined provider scopes", q.Get("scope"))
	}

	rr := doCallback(t, router, "github", cookieVal, "?code="+ssoTestCode+"&state="+q.Get("state"))
	// R5: success is now the session redirect (return_to unset → "/").
	if rr.Code != http.StatusFound {
		t.Fatalf("github callback: status %d, want 302 (body %s)", rr.Code, rr.Body.String())
	}
	res := rr.Result()
	defer res.Body.Close()
	var ghSess string
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName {
			ghSess = c.Value
		}
	}
	if ghSess == "" {
		t.Fatal("github callback set no session cookie")
	}
	if st, body := otAuthProbe(t, pool, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: ghSess})
	}); st != http.StatusOK || !strings.Contains(body, "gh-scope-x") {
		t.Fatalf("github session probe: status %d body %s, want 200 with the principal's scope", st, body)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("exchanges = %d, want 1", exchanges.Load())
	}
	if sawVerifier.Load() {
		t.Fatalf("github exchange carried a code_verifier — PKCE is OIDC-only (F6)")
	}

	var email, displayName string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(email,''), COALESCE(display_name,'') FROM context_external_identities
		  WHERE issuer='https://github.com' AND subject='4242'`).Scan(&email, &displayName); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if email != "octo@example.com" || displayName != "Octo Cat" {
		t.Fatalf("github refresh wrote email=%q display_name=%q", email, displayName)
	}
}

// s256of mirrors the S256 code-challenge derivation for assertions.
func s256of(verifier string) string {
	sum := sha256Sum(verifier)
	return base64.RawURLEncoding.EncodeToString(sum)
}

func sha256Sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// TestSSOSessionR5_Integration proves the R5 onboarding gates (design/05
// §4.5, E4b admin-invite):
//
//   - a linked identity whose principal holds NO active key → 403 and NO
//     session cookie ("null Autorität bis gekeyt" — SSO is identity, never
//     authorization; the LOOP gate)
//   - an INACTIVE key is the same 403 (PickPrincipalKey's active predicate,
//     the Rot-Probe target)
//   - store-level link semantics: idempotent re-link (same principal),
//     ErrIdentityConflict for a different principal, unlink → the login
//     path answers the admin-invite 403 again
func TestSSOSessionR5_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := newSSOTestBox(t)
	idp := newMockIdP(t)
	ctx := context.Background()

	if _, err := store.CreateOAuthProvider(ctx, pool, box, store.CreateOAuthProviderSpec{
		Slug: "corp5", Type: "oidc", DisplayName: "Corp5 SSO",
		Issuer: idp.srv.URL, ClientID: ssoTestClientID, ClientSecret: ssoTestSecret,
		RedirectBase: ssoRedirectBase, SingleTenantIssuer: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	_, router := newSSOTestHandler(pool, box, 1000)

	// login helper: full round trip for the seeded identity, returns the
	// callback recorder.
	roundTrip := func(subject string) *httptest.ResponseRecorder {
		cookieVal, loc := doLogin(t, router, "corp5", "")
		q := loc.Query()
		idp.nonce = q.Get("nonce")
		idp.subject = subject
		return doCallback(t, router, "corp5", cookieVal, "?code="+ssoTestCode+"&state="+q.Get("state")+"&iss="+url.QueryEscape(idp.srv.URL))
	}

	// --- key-less principal → 403, no cookie ---------------------------------
	keylessPID := seedIdentity(t, pool, idp.srv.URL, "keyless-sub", "")
	rr := roundTrip("keyless-sub")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("key-less login: status %d, want 403 (body %s)", rr.Code, rr.Body.String())
	}
	res := rr.Result()
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Fatal("key-less login must not set a session cookie (null authority until keyed)")
		}
	}

	// --- inactive key is the same 403 (active predicate) ----------------------
	inactiveKey := "cc99" + strings.Repeat("ab", 30)
	_, inactiveKeyID := otSeedTenantKey(t, pool, "r5-x", "r5-scope-x", inactiveKey, keylessPID)
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET active=false WHERE id=$1::uuid`, inactiveKeyID); err != nil {
		t.Fatalf("deactivate key: %v", err)
	}
	if rr := roundTrip("keyless-sub"); rr.Code != http.StatusForbidden {
		t.Fatalf("inactive-key login: status %d, want 403", rr.Code)
	}
	// Re-activating turns the SAME identity into a full session (302).
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET active=true WHERE id=$1::uuid`, inactiveKeyID); err != nil {
		t.Fatalf("re-activate key: %v", err)
	}
	if rr := roundTrip("keyless-sub"); rr.Code != http.StatusFound {
		t.Fatalf("keyed login: status %d, want 302", rr.Code)
	}

	// --- link semantics (store level) -----------------------------------------
	issuer := idp.srv.URL
	if err := store.LinkExternalIdentity(ctx, pool, "oidc", issuer, "keyless-sub", keylessPID, "", ""); err != nil {
		t.Fatalf("idempotent re-link (same principal): %v", err)
	}
	var otherPID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('other') RETURNING id::text`).Scan(&otherPID); err != nil {
		t.Fatalf("seed other principal: %v", err)
	}
	if err := store.LinkExternalIdentity(ctx, pool, "oidc", issuer, "keyless-sub", otherPID, "", ""); !errors.Is(err, store.ErrIdentityConflict) {
		t.Fatalf("re-link to different principal: err=%v, want ErrIdentityConflict", err)
	}

	// --- unlink → the admin-invite 403 returns --------------------------------
	removed, err := store.UnlinkExternalIdentity(ctx, pool, issuer, "keyless-sub")
	if err != nil || !removed {
		t.Fatalf("unlink: removed=%v err=%v", removed, err)
	}
	if rr := roundTrip("keyless-sub"); rr.Code != http.StatusForbidden {
		t.Fatalf("login after unlink: status %d, want 403 (admin-invite reject)", rr.Code)
	}
}
