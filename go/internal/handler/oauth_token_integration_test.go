//go:build integration

// Integration gates for OAuth wave S3 (design/03 §7 W03-3, negativ-geprobt)
// against a real PG18 testcontainer (migrations through 099+):
//
//   - e2e: /authorize (form POST, real key) → 302 code → /token → OPAQUE
//     ctxt_ access token + expires_in (the raw api key no longer leaves
//     the server through /token)
//   - /mcp-chain (production Auth middleware): ctxt_ Bearer → 200, and the
//     AuthResult is BYTE-IDENTICAL to the raw-key path (same key, same
//     ctx_auth_by_id materialisation — the W03-3 regression gate)
//   - ctxt_ via X-Context-Key works (RVW-Vollst-F6: prefix survives the
//     non-Bearer header, no hex-stripping into the raw-key path)
//   - raw api key as Bearer keeps working (E2 legacy path, byte-identical)
//   - INV-A probe (synthetic multi-tenant fixtures, RVW-Vollst-F7): the
//     principal holds K1 (tenant X) AND K2 (tenant Y); the token minted
//     from K1 materialises ONLY K1 — TenantID==X, ReadScopes contain no
//     Y-scope (no union over the principal's keys, Masterplan K4)
//   - negativ: ctxr_ at the auth chain → 401 (refresh is not an access
//     credential); expired token → 401; unknown ctxt_ → 401 (NO fallback
//     to the raw-key path); soft-revoked key (active=false) → token dead
//     via the shared ctx_auth_by_id gate (no separate token revocation)
//   - SSE re-auth seam: resolveCredential(pool, storedRawCredential) — the
//     exact call the events.go/project_events.go re-auth tick makes — keeps
//     resolving the ctxt_ token (RVW-Vollst-F2: the stream survives the tick)
//   - code single-use stays intact post-S3 (second /token → invalid_grant)
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestOpaqueToken -count=1 -v
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// otSeedTenantKey creates tenant+scope+key fixtures for the INV-A probe. The
// key row is hand-seeded like the sibling integration tests; the parity gate
// below does not depend on the seed path — BOTH auth paths (raw key, token)
// materialise through the production ctx_auth/ctx_auth_by_id SQL, and the
// token row itself is minted through the production /authorize+/token flow.
func otSeedTenantKey(t *testing.T, pool *pgxpool.Pool, slug, scope, plainKey, principalID string) (tenantID, keyID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant %s: %v", slug, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scope, tenantID); err != nil {
		t.Fatalf("map scope %s: %v", scope, err)
	}
	keyHash := sha256.Sum256([]byte(plainKey))
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 VALUES ($1, $2, $3, $4::uuid, $5::uuid) RETURNING id::text`,
		hex.EncodeToString(keyHash[:]), "ot-"+slug, scope, tenantID, principalID).Scan(&keyID); err != nil {
		t.Fatalf("seed key for %s: %v", slug, err)
	}
	return tenantID, keyID
}

// otAuthProbe runs one request through the PRODUCTION Auth middleware chain
// and returns (status, marshalled AuthResult). The echo handler serialises
// the full AuthResult so the byte-identity gate compares everything at once.
func otAuthProbe(t *testing.T, pool *pgxpool.Pool, set func(*http.Request)) (int, string) {
	t.Helper()
	var body string
	h := Auth(pool)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := json.Marshal(AuthResultFromContext(r.Context()))
		if err != nil {
			t.Fatalf("marshal auth result: %v", err)
		}
		body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	set(r)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, body
}

func TestOpaqueTokenS3_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// One principal, two tenants, two keys (synthetic multi-tenant fixture,
	// RVW-Vollst-F7 — prod is single-tenant and cannot probe INV-A).
	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('ot-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	keyX := "aa11" + strings.Repeat("cd", 30) // pure hex, raw-key shaped
	tenantX, keyXID := otSeedTenantKey(t, pool, "ot-x", "ot-scope-x", keyX, principalID)
	_, _ = otSeedTenantKey(t, pool, "ot-y", "ot-scope-y", "bb22"+strings.Repeat("ef", 30), principalID)

	client, _, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
		Label:                   "ot-client",
		RedirectURIs:            []string{"https://claude.ai/api/mcp/auth_callback"},
		TokenEndpointAuthMethod: "none",
		Source:                  "admin",
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}

	oh := NewOAuthHandler(pool)
	verifier := strings.Repeat("v", 64)
	chal := sha256.Sum256([]byte(verifier))

	// --- e2e: /authorize → code → /token → opaque token --------------------
	form := url.Values{
		"api_key":        {keyX},
		"client_id":      {client.ClientID},
		"redirect_uri":   {"https://claude.ai/api/mcp/auth_callback"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(chal[:])},
		"code_challenge_method": {"S256"}, // mandatory since S5 (plain/absent → 400)
		"state":                 {"ot-state"},
	}
	authReq := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	authReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authRec := httptest.NewRecorder()
	oh.Authorize(authRec, authReq)
	if authRec.Code != http.StatusFound {
		t.Fatalf("authorize: got %d, want 302 (%s)", authRec.Code, authRec.Body.String())
	}
	loc, err := url.Parse(authRec.Header().Get("Location"))
	if err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("authorize redirect without code: %q", authRec.Header().Get("Location"))
	}
	code := loc.Query().Get("code")

	exchange := func(code string) (*httptest.ResponseRecorder, map[string]any) {
		tf := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"code_verifier": {verifier},
			"client_id":     {client.ClientID},
		}
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(tf.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		oh.Token(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec, body
	}

	rec, body := exchange(code)
	if rec.Code != http.StatusOK {
		t.Fatalf("token: got %d (%s)", rec.Code, rec.Body.String())
	}
	token, _ := body["access_token"].(string)
	if !strings.HasPrefix(token, store.AccessTokenPrefix) {
		t.Fatalf("access_token is not opaque (ctxt_): %q", token)
	}
	if token == keyX || strings.Contains(token, keyX) {
		t.Fatal("raw api key leaked through /token")
	}
	if exp, _ := body["expires_in"].(float64); int(exp) != 3600 {
		t.Fatalf("expires_in: got %v, want 3600 (E-03c)", body["expires_in"])
	}

	// --- code single-use stays intact post-S3 ------------------------------
	if rec2, body2 := exchange(code); rec2.Code == http.StatusOK {
		t.Fatalf("code reuse must fail, got 200 (%v)", body2)
	}

	// --- production Auth chain: token == raw key, byte-identical -----------
	stKey, arKey := otAuthProbe(t, pool, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+keyX)
	})
	stTok, arTok := otAuthProbe(t, pool, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	if stKey != http.StatusOK || stTok != http.StatusOK {
		t.Fatalf("auth chain: key=%d token=%d, want 200/200", stKey, stTok)
	}
	if arKey != arTok {
		t.Fatalf("AuthResult drift between raw-key and token path:\nkey:   %s\ntoken: %s", arKey, arTok)
	}

	// --- ctxt_ via X-Context-Key (RVW-Vollst-F6) ----------------------------
	if st, ar := otAuthProbe(t, pool, func(r *http.Request) {
		r.Header.Set("X-Context-Key", token)
	}); st != http.StatusOK || ar != arTok {
		t.Fatalf("X-Context-Key token path: status %d, drift=%v", st, ar != arTok)
	}

	// --- INV-A: token from K1 (tenant X) never unions in tenant Y ----------
	var arParsed struct {
		TenantID   string   `json:"TenantID"`
		ReadScopes []string `json:"ReadScopes"`
		ApiKeyID   string   `json:"ApiKeyID"`
	}
	if err := json.Unmarshal([]byte(arTok), &arParsed); err != nil {
		t.Fatalf("parse token AuthResult: %v", err)
	}
	if arParsed.TenantID != tenantX || arParsed.ApiKeyID != keyXID {
		t.Fatalf("INV-A: token resolved to tenant %q / key %q, want %q / %q",
			arParsed.TenantID, arParsed.ApiKeyID, tenantX, keyXID)
	}
	for _, s := range arParsed.ReadScopes {
		if s == "ot-scope-y" {
			t.Fatalf("INV-A violated: token from K1 (tenant X) sees Y-scope: %v", arParsed.ReadScopes)
		}
	}

	// --- SSE re-auth seam (RVW-Vollst-F2): the exact tick call -------------
	if res, err := resolveCredential(ctx, pool, token); err != nil || res == nil || !res.IsValid {
		t.Fatalf("resolveCredential (SSE re-auth seam) rejected a live ctxt_ token: %v / %+v", err, res)
	}

	// --- negativ: ctxr_ / unknown ctxt_ / expired / revoked key ------------
	if st, _ := otAuthProbe(t, pool, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer ctxr_"+strings.Repeat("ab", 32))
	}); st != http.StatusUnauthorized {
		t.Fatalf("ctxr_ at auth chain: got %d, want 401", st)
	}
	if st, _ := otAuthProbe(t, pool, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer ctxt_"+strings.Repeat("00", 32))
	}); st != http.StatusUnauthorized {
		t.Fatalf("unknown ctxt_ must NOT fall back to the raw-key path: got %d, want 401", st)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE context_access_tokens SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`,
		store.TokenHash(token)); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	if st, _ := otAuthProbe(t, pool, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	}); st != http.StatusUnauthorized {
		t.Fatalf("expired token: got %d, want 401", st)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_access_tokens SET expires_at = now() + interval '1 hour' WHERE token_hash = $1`,
		store.TokenHash(token)); err != nil {
		t.Fatalf("revive token: %v", err)
	}

	// Soft-revoke of the KEY kills the token through the shared
	// ctx_auth_by_id gate — no token-side bookkeeping involved.
	if _, err := pool.Exec(ctx,
		`UPDATE context_api_keys SET active = false WHERE id = $1::uuid`, keyXID); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	if st, _ := otAuthProbe(t, pool, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	}); st != http.StatusUnauthorized {
		t.Fatalf("token of a soft-revoked key: got %d, want 401", st)
	}

	// GC sweep spares fresh rows (7d grace) — the revived row must survive.
	if n, err := store.EvictExpiredOAuthTokens(ctx, pool); err != nil || n != 0 {
		t.Fatalf("token GC on fresh rows: deleted %d, err %v (want 0, nil)", n, err)
	}
}
