//go:build integration

// Integration gates for OAuth wave R2 (design/05 §4.2 path 3 / §7 W2+W3,
// negativ-geprobt) against a real PG18 testcontainer (migrations through 102):
//
//   - ctx_session cookie resolves through overlay→token→ctx_auth_by_id and
//     the AuthResult is BYTE-IDENTICAL to the raw-key path (the §4.2
//     convergence gate — downstream stays untouched)
//   - header precedence: a Bearer credential ALWAYS beats the cookie (a
//     request carrying both materialises the header identity)
//   - INV-A probe: the principal holds K1 (tenant X) AND K2 (tenant Y); the
//     session minted over K1 materialises ONLY K1 — no union
//   - soft-revoked key (active=false) → cookie answers 401 via the shared
//     ctx_auth_by_id gate (the 05 §4.1 instant-revoke path, own probe —
//     the byte-parity gate cannot catch a missing active-gate)
//   - revoked / expired token row → 401; malformed and unknown session ids
//     → the same silent 401 (no oracle, no 22P02)
//   - SSE re-auth seam: resolveRequestCredential(pool, sessionID, true) —
//     the exact call both re-auth ticks make — resolves, and dies after the
//     token family is revoked (the stream-kill semantics)
//   - a successful resolve stamps last_used_at (write-on-read)
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestWebSession -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// wsSeedSession mints a login-shaped token pair over the given key through
// the PRODUCTION mint path and lays a web-session overlay row over it.
// Returns (sessionID, accessTokenRowID).
func wsSeedSession(t *testing.T, pool *pgxpool.Pool, apiKeyID, principalID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	pair, err := store.MintTokenPair(ctx, pool, store.OAuthToken{
		APIKeyID:    apiKeyID,
		PrincipalID: principalID,
		ClientID:    "ws-login",
		Audiences:   []string{"https://ctx.example/mcp", "https://ctx.example/web"},
		IssuedVia:   "login",
	}, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("mint login pair: %v", err)
	}
	var tokenRowID, refreshFamily string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, refresh_family::text FROM context_access_tokens WHERE token_hash = $1`,
		store.TokenHash(pair.AccessToken)).Scan(&tokenRowID, &refreshFamily); err != nil {
		t.Fatalf("locate token row: %v", err)
	}
	var sessionID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_web_sessions (principal_id, access_token_id, refresh_family, csrf_secret)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'ws-csrf') RETURNING id::text`,
		principalID, tokenRowID, refreshFamily).Scan(&sessionID); err != nil {
		t.Fatalf("seed session overlay: %v", err)
	}
	return sessionID, tokenRowID
}

func wsCookie(sessionID string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionID})
	}
}

func TestWebSessionR2_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('ws-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	keyX := "cc33" + strings.Repeat("ab", 30)
	keyY := "dd44" + strings.Repeat("ef", 30)
	_, keyXID := otSeedTenantKey(t, pool, "ws-x", "ws-scope-x", keyX, principalID)
	_, _ = otSeedTenantKey(t, pool, "ws-y", "ws-scope-y", keyY, principalID)

	sessionID, tokenRowID := wsSeedSession(t, pool, keyXID, principalID)

	// --- cookie → AuthResult byte-identical to the raw-key path -------------
	stKey, bodyKey := otAuthProbe(t, pool, func(r *http.Request) { r.Header.Set("X-Context-Key", keyX) })
	if stKey != http.StatusOK {
		t.Fatalf("raw key path: got %d, want 200", stKey)
	}
	stCookie, bodyCookie := otAuthProbe(t, pool, wsCookie(sessionID))
	if stCookie != http.StatusOK {
		t.Fatalf("cookie path: got %d, want 200", stCookie)
	}
	if bodyKey != bodyCookie {
		t.Fatalf("cookie AuthResult differs from raw-key path:\nkey:    %s\ncookie: %s", bodyKey, bodyCookie)
	}

	// --- INV-A: session over K1 materialises ONLY tenant X ------------------
	// AuthResult carries no json tags — keys are the Go field names.
	var res struct {
		TenantID   string   `json:"TenantID"`
		ReadScopes []string `json:"ReadScopes"`
	}
	if err := json.Unmarshal([]byte(bodyCookie), &res); err != nil {
		t.Fatalf("unmarshal cookie AuthResult: %v", err)
	}
	for _, s := range res.ReadScopes {
		if s == "ws-scope-y" {
			t.Fatalf("INV-A violated: cookie session over K1 exposes tenant-Y scope (scopes=%v)", res.ReadScopes)
		}
	}

	// --- header precedence: Bearer (key Y) beats the cookie (session X) -----
	stBoth, bodyBoth := otAuthProbe(t, pool, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+keyY)
		wsCookie(sessionID)(r)
	})
	if stBoth != http.StatusOK || !strings.Contains(bodyBoth, "ws-scope-y") || strings.Contains(bodyBoth, "ws-scope-x") {
		t.Fatalf("header precedence: got %d %s, want the tenant-Y identity", stBoth, bodyBoth)
	}

	// --- last_used_at stamped by the resolve --------------------------------
	var touched bool
	if err := pool.QueryRow(ctx,
		`SELECT last_used_at IS NOT NULL FROM context_web_sessions WHERE id = $1`,
		sessionID).Scan(&touched); err != nil || !touched {
		t.Fatalf("last_used_at not stamped (err=%v, touched=%v)", err, touched)
	}

	// --- malformed / unknown session ids → silent 401 -----------------------
	if st, _ := otAuthProbe(t, pool, wsCookie("not-a-uuid")); st != http.StatusUnauthorized {
		t.Fatalf("malformed session id: got %d, want 401", st)
	}
	if st, _ := otAuthProbe(t, pool, wsCookie("00000000-0000-7000-8000-000000000000")); st != http.StatusUnauthorized {
		t.Fatalf("unknown session id: got %d, want 401", st)
	}

	// --- SSE re-auth seam: the exact tick call ------------------------------
	if ar, _, err := resolveRequestCredential(ctx, pool, sessionID, true); err != nil || ar == nil || !ar.IsValid {
		t.Fatalf("SSE seam resolve: err=%v valid=%v, want valid", err, ar != nil && ar.IsValid)
	}

	// --- soft-revoked key → 401 (instant revoke via ctx_auth_by_id) ---------
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET active = false WHERE id = $1::uuid`, keyXID); err != nil {
		t.Fatalf("soft-revoke key: %v", err)
	}
	if st, _ := otAuthProbe(t, pool, wsCookie(sessionID)); st != http.StatusUnauthorized {
		t.Fatalf("soft-revoked key via cookie: got %d, want 401", st)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET active = true WHERE id = $1::uuid`, keyXID); err != nil {
		t.Fatalf("re-activate key: %v", err)
	}
	if st, _ := otAuthProbe(t, pool, wsCookie(sessionID)); st != http.StatusOK {
		t.Fatalf("re-activated key via cookie: got %d, want 200 (probe hygiene)", st)
	}

	// --- revoked token row → 401, and the SSE seam dies with the family -----
	if _, err := pool.Exec(ctx,
		`UPDATE context_access_tokens SET revoked_at = now() WHERE id = $1::uuid`, tokenRowID); err != nil {
		t.Fatalf("revoke token row: %v", err)
	}
	if st, _ := otAuthProbe(t, pool, wsCookie(sessionID)); st != http.StatusUnauthorized {
		t.Fatalf("revoked token via cookie: got %d, want 401", st)
	}
	if ar, _, err := resolveRequestCredential(ctx, pool, sessionID, true); err != nil || ar == nil || ar.IsValid {
		t.Fatalf("SSE seam after revoke: err=%v valid=%v, want invalid (stream must die)", err, ar != nil && ar.IsValid)
	}

	// --- expired token row → 401 (fresh session, expiry forced) -------------
	session2, tokenRow2 := wsSeedSession(t, pool, keyXID, principalID)
	if _, err := pool.Exec(ctx,
		`UPDATE context_access_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1::uuid`, tokenRow2); err != nil {
		t.Fatalf("expire token row: %v", err)
	}
	if st, _ := otAuthProbe(t, pool, wsCookie(session2)); st != http.StatusUnauthorized {
		t.Fatalf("expired token via cookie: got %d, want 401", st)
	}
}
