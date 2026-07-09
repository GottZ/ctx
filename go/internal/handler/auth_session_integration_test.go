//go:build integration

// Integration gates for OAuth wave R3 (design/05 §4.3/§4.4, negativ-geprobt)
// against a real PG18 testcontainer:
//
//   - login: wrong key → 401; valid key → 200 + csrf_token + BOTH httpOnly
//     cookies (ctx_session Path=/, ctx_refresh Path=/auth)
//   - CSRF synchronizer at the Auth middleware: cookie GET passes, cookie
//     mutation without/with-wrong X-CSRF-Token → 403, with the token → 200
//   - MCP-Bearer untouched (the LOOP gate): a header-credential mutation
//     passes with NO csrf token — nothing ambient, nothing gated
//   - whoami delivers csrf_token on the cookie path ONLY (omitempty keeps
//     the header path byte-compatible)
//   - refresh: rotates the refresh cookie, repoints the overlay onto the
//     fresh access row (family-bound); REPLAY of the rotated refresh → 401
//     AND the whole family is revoked — the session cookie dies with it
//     (the LOOP gate "Refresh-Replay → Familien-Revoke" on the web path)
//   - cross-family: session A + refresh B never repoints A (fail-closed
//     401), and A keeps resolving untouched
//   - logout: cookies cleared, overlay gone, family revoked (idempotent)
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestAuthSessionR3 -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// asServer stands up the R3 routes plus an Auth-gated echo route (GET+POST)
// so the CSRF gate is probed through the REAL middleware chain.
func asServer(pool *pgxpool.Pool) *httptest.Server {
	sessH := NewSessionHandler(pool)
	whoH := NewWhoamiHandler(pool)
	r := chi.NewRouter()
	r.Post("/auth/login", sessH.HandleLogin)
	r.Post("/auth/refresh", sessH.HandleRefresh)
	r.Post("/auth/logout", sessH.HandleLogout)
	r.Group(func(r chi.Router) {
		r.Use(Auth(pool))
		r.Get("/api/whoami", whoH.HandleWhoami)
		r.HandleFunc("/api/echo", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})
	return httptest.NewServer(r)
}

// asDo fires one request with optional cookies/headers and returns the
// response (body decoded into a map when JSON).
func asDo(t *testing.T, method, url string, body string, mod func(*http.Request)) (*http.Response, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp, decoded
}

// cookieByName pulls one Set-Cookie by name (nil when absent).
func cookieByName(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestAuthSessionR3_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('as-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	plainKey := "ff66" + strings.Repeat("ab", 30)
	_, _ = otSeedTenantKey(t, pool, "as-x", "as-scope-x", plainKey, principalID)

	srv := asServer(pool)
	defer srv.Close()

	// --- login: wrong key → 401 --------------------------------------------
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/auth/login",
		`{"api_key":"deadbeef"}`, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login wrong key: got %d, want 401", resp.StatusCode)
	}

	// --- login: valid key → 200 + csrf + both cookies ------------------------
	loginResp, loginBody := asDo(t, http.MethodPost, srv.URL+"/auth/login",
		`{"api_key":"`+plainKey+`"}`, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d, want 200", loginResp.StatusCode)
	}
	csrfToken, _ := loginBody["csrf_token"].(string)
	if csrfToken == "" {
		t.Fatal("login response carries no csrf_token")
	}
	sessC := cookieByName(loginResp, sessionCookieName)
	refC := cookieByName(loginResp, refreshCookieName)
	if sessC == nil || refC == nil {
		t.Fatalf("login must set both cookies (session=%v refresh=%v)", sessC != nil, refC != nil)
	}
	if !sessC.HttpOnly || !refC.HttpOnly || sessC.Path != "/" || refC.Path != "/auth" {
		t.Fatalf("cookie attributes wrong: session(httpOnly=%v path=%q) refresh(httpOnly=%v path=%q)",
			sessC.HttpOnly, sessC.Path, refC.HttpOnly, refC.Path)
	}
	if !strings.HasPrefix(refC.Value, store.RefreshTokenPrefix) {
		t.Fatalf("refresh cookie does not carry a ctxr_ token: %q", refC.Value[:8])
	}
	withSession := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessC.Value}) }

	// --- cookie GET passes, cookie mutation is CSRF-gated ---------------------
	if resp, _ := asDo(t, http.MethodGet, srv.URL+"/api/echo", "", withSession); resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie GET: got %d, want 200", resp.StatusCode)
	}
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/api/echo", "", withSession); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie POST without CSRF: got %d, want 403", resp.StatusCode)
	}
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/api/echo", "", func(r *http.Request) {
		withSession(r)
		r.Header.Set("X-CSRF-Token", "definitely-wrong")
	}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie POST with wrong CSRF: got %d, want 403", resp.StatusCode)
	}
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/api/echo", "", func(r *http.Request) {
		withSession(r)
		r.Header.Set("X-CSRF-Token", csrfToken)
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie POST with CSRF: got %d, want 200", resp.StatusCode)
	}

	// --- MCP-Bearer untouched: header mutation needs NO csrf ------------------
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/api/echo", "", func(r *http.Request) {
		r.Header.Set("X-Context-Key", plainKey)
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("header POST without CSRF: got %d, want 200 (MCP-Bearer untouched)", resp.StatusCode)
	}

	// --- whoami: csrf_token on the cookie path only ---------------------------
	if _, body := asDo(t, http.MethodGet, srv.URL+"/api/whoami", "", withSession); body["csrf_token"] != csrfToken {
		t.Fatalf("whoami via cookie: csrf_token %v, want the login secret", body["csrf_token"])
	}
	if _, body := asDo(t, http.MethodGet, srv.URL+"/api/whoami", "", func(r *http.Request) {
		r.Header.Set("X-Context-Key", plainKey)
	}); body["csrf_token"] != nil {
		t.Fatalf("whoami via header: csrf_token present (%v), want absent", body["csrf_token"])
	}

	// --- refresh: rotation + overlay repoint ----------------------------------
	var tokenBefore string
	if err := pool.QueryRow(ctx,
		`SELECT access_token_id::text FROM context_web_sessions WHERE id = $1`, sessC.Value).Scan(&tokenBefore); err != nil {
		t.Fatalf("read overlay before refresh: %v", err)
	}
	refreshResp, _ := asDo(t, http.MethodPost, srv.URL+"/auth/refresh", "", func(r *http.Request) {
		withSession(r)
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: refC.Value})
	})
	if refreshResp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: got %d, want 200", refreshResp.StatusCode)
	}
	newRefC := cookieByName(refreshResp, refreshCookieName)
	if newRefC == nil || newRefC.Value == refC.Value {
		t.Fatal("refresh must set a NEW refresh cookie")
	}
	var tokenAfter string
	if err := pool.QueryRow(ctx,
		`SELECT access_token_id::text FROM context_web_sessions WHERE id = $1`, sessC.Value).Scan(&tokenAfter); err != nil {
		t.Fatalf("read overlay after refresh: %v", err)
	}
	if tokenAfter == tokenBefore {
		t.Fatal("refresh did not repoint the overlay onto the fresh access row")
	}
	if resp, _ := asDo(t, http.MethodGet, srv.URL+"/api/echo", "", withSession); resp.StatusCode != http.StatusOK {
		t.Fatalf("session after refresh: got %d, want 200", resp.StatusCode)
	}

	// --- REPLAY of the rotated refresh → 401 + family revoked -----------------
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/auth/refresh", "", func(r *http.Request) {
		withSession(r)
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: refC.Value}) // the OLD one
	}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh replay: got %d, want 401", resp.StatusCode)
	}
	if resp, _ := asDo(t, http.MethodGet, srv.URL+"/api/echo", "", withSession); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session after replay: got %d, want 401 (family revoked)", resp.StatusCode)
	}

	// --- cross-family: session A + refresh B never repoints A -----------------
	loginA, bodyA := asDo(t, http.MethodPost, srv.URL+"/auth/login", `{"api_key":"`+plainKey+`"}`, nil)
	loginB, _ := asDo(t, http.MethodPost, srv.URL+"/auth/login", `{"api_key":"`+plainKey+`"}`, nil)
	sessA := cookieByName(loginA, sessionCookieName)
	refB := cookieByName(loginB, refreshCookieName)
	if sessA == nil || refB == nil || bodyA["csrf_token"] == "" {
		t.Fatal("second/third login failed to set cookies")
	}
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/auth/refresh", "", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessA.Value})
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: refB.Value})
	}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-family refresh: got %d, want 401 (fail-closed)", resp.StatusCode)
	}
	if resp, _ := asDo(t, http.MethodGet, srv.URL+"/api/echo", "", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessA.Value})
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("session A after cross-family attempt: got %d, want 200 (untouched)", resp.StatusCode)
	}

	// --- logout: cookies cleared, family revoked, idempotent ------------------
	logoutResp, _ := asDo(t, http.MethodPost, srv.URL+"/auth/logout", "", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessA.Value})
	})
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout: got %d, want 200", logoutResp.StatusCode)
	}
	if c := cookieByName(logoutResp, sessionCookieName); c == nil || c.MaxAge >= 0 {
		t.Fatal("logout must expire the session cookie")
	}
	if resp, _ := asDo(t, http.MethodGet, srv.URL+"/api/echo", "", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessA.Value})
	}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session after logout: got %d, want 401", resp.StatusCode)
	}
	var overlayCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_web_sessions WHERE id = $1`, sessA.Value).Scan(&overlayCount); err != nil || overlayCount != 0 {
		t.Fatalf("overlay after logout: count=%d err=%v, want 0", overlayCount, err)
	}
	// Idempotent second logout.
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/auth/logout", "", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessA.Value})
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("second logout: got %d, want 200 (idempotent)", resp.StatusCode)
	}
}
