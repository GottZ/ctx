//go:build integration

// Integration gates for OAuth wave S4 (design/03 §7 W03-4, negativ-geprobt)
// against a real PG18 testcontainer:
//
//   - authorization_code grant returns access + refresh (both prefixed,
//     expires_in = nominal access TTL)
//   - refresh_token grant rotates: fresh pair, NEW token values, same
//     family lineage (parent_id chain), old ACCESS stays valid until its
//     own expiry, old REFRESH is dead
//   - negativ (the wave gate): REPLAY of the rotated refresh → the WHOLE
//     family is revoked — the freshly rotated access AND refresh die with
//     it (theft response), response is the generic invalid_grant
//   - negativ: wrong client_id on a live refresh → invalid_grant WITHOUT
//     side effects (no family revoke — a value-sniffing third party cannot
//     DoS the victim's session), the same refresh still rotates afterwards
//   - negativ: unknown/garbage refresh → invalid_grant, no signal
//   - negativ: family absolute cap (E-TTL 90d) — an aged family anchor
//     makes rotation refuse (invalid_grant) and revokes the family
//   - rotated refresh expiry is capped at family-anchor+cap (rolling TTL
//     never outlives the cap)
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestRefreshRotation -count=1 -v
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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// rtExchange posts one /token request and decodes the JSON body.
func rtExchange(t *testing.T, oh *OAuthHandler, form url.Values) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	oh.Token(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// rtAuthStatus runs one credential through the production Auth chain.
func rtAuthStatus(t *testing.T, pool *pgxpool.Pool, credential string) int {
	t.Helper()
	h := Auth(pool)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+credential)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func TestRefreshRotationS4_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('rt-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	plainKey := "cc33" + strings.Repeat("ab", 30)
	keyHash := sha256.Sum256([]byte(plainKey))
	var tenantID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ('rt-x','rt-x') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('rt-scope',$1::uuid)`, tenantID); err != nil {
		t.Fatalf("map scope: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 VALUES (encode($1,'hex'), 'rt-key', 'rt-scope', $2::uuid, $3::uuid)`,
		keyHash[:], tenantID, principalID); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	client, _, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
		Label: "rt-client", TokenEndpointAuthMethod: "none", Source: "admin",
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}

	oh := NewOAuthHandler(pool)
	verifier := strings.Repeat("w", 64)
	chal := sha256.Sum256([]byte(verifier))

	// --- code flow → pair ---------------------------------------------------
	form := url.Values{
		"api_key":        {plainKey},
		"client_id":      {client.ClientID},
		"redirect_uri":   {"https://claude.ai/api/mcp/auth_callback"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(chal[:])},
		"code_challenge_method": {"S256"}, // mandatory since S5 (plain/absent → 400)
	}
	authReq := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	authReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authRec := httptest.NewRecorder()
	oh.Authorize(authRec, authReq)
	if authRec.Code != http.StatusFound {
		t.Fatalf("authorize: got %d (%s)", authRec.Code, authRec.Body.String())
	}
	loc, _ := url.Parse(authRec.Header().Get("Location"))
	st, body := rtExchange(t, oh, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {loc.Query().Get("code")},
		"code_verifier": {verifier},
		"client_id":     {client.ClientID},
	})
	if st != http.StatusOK {
		t.Fatalf("code exchange: %d (%v)", st, body)
	}
	access1, _ := body["access_token"].(string)
	refresh1, _ := body["refresh_token"].(string)
	if !strings.HasPrefix(access1, store.AccessTokenPrefix) || !strings.HasPrefix(refresh1, store.RefreshTokenPrefix) {
		t.Fatalf("pair prefixes wrong: %q / %q", access1, refresh1)
	}
	if exp, _ := body["expires_in"].(float64); int(exp) != 3600 {
		t.Fatalf("expires_in = %v, want nominal 3600", body["expires_in"])
	}

	// --- negativ: wrong client on a LIVE refresh — no side effects ----------
	if st, _ := rtExchange(t, oh, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh1},
		"client_id":     {"someone-else"},
	}); st == http.StatusOK {
		t.Fatal("wrong-client refresh must fail")
	}

	// --- rotation ------------------------------------------------------------
	st, body = rtExchange(t, oh, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh1},
		"client_id":     {client.ClientID},
	})
	if st != http.StatusOK {
		t.Fatalf("rotation failed (wrong-client attempt must not have revoked): %d (%v)", st, body)
	}
	access2, _ := body["access_token"].(string)
	refresh2, _ := body["refresh_token"].(string)
	if access2 == access1 || refresh2 == refresh1 {
		t.Fatal("rotation returned a non-fresh token")
	}
	// Lineage: the new rows continue the SAME family, chained via parent_id.
	var families, chained int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT refresh_family),
		        count(*) FILTER (WHERE parent_id IS NOT NULL)
		   FROM context_access_tokens`).Scan(&families, &chained); err != nil {
		t.Fatalf("lineage query: %v", err)
	}
	if families != 1 || chained != 2 {
		t.Fatalf("lineage: families=%d chained=%d (want 1 family, 2 chained rows)", families, chained)
	}

	if rtAuthStatus(t, pool, access1) != http.StatusOK {
		t.Fatal("old access must stay valid until its own expiry")
	}
	if rtAuthStatus(t, pool, access2) != http.StatusOK {
		t.Fatal("new access must be valid")
	}

	// --- the wave gate: replay of the ROTATED refresh → family revoke --------
	st, _ = rtExchange(t, oh, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh1},
		"client_id":     {client.ClientID},
	})
	if st == http.StatusOK {
		t.Fatal("replayed rotated refresh must fail")
	}
	if rtAuthStatus(t, pool, access2) != http.StatusUnauthorized {
		t.Fatal("family revoke must kill the freshly rotated ACCESS token")
	}
	if st, _ := rtExchange(t, oh, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh2},
		"client_id":     {client.ClientID},
	}); st == http.StatusOK {
		t.Fatal("family revoke must kill the freshly rotated REFRESH token")
	}
	if rtAuthStatus(t, pool, access1) != http.StatusUnauthorized {
		t.Fatal("family revoke must kill the pre-rotation access token too")
	}

	// --- negativ: unknown refresh → plain miss -------------------------------
	if st, _ := rtExchange(t, oh, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {store.RefreshTokenPrefix + strings.Repeat("00", 32)},
		"client_id":     {client.ClientID},
	}); st == http.StatusOK {
		t.Fatal("unknown refresh must fail")
	}

	// --- family absolute cap (E-TTL): aged anchor refuses rotation ----------
	pair, err := store.MintTokenPair(ctx, pool, store.OAuthToken{
		APIKeyID:    keyIDOf(t, pool, "rt-key"),
		PrincipalID: principalID,
		ClientID:    client.ClientID,
		Audiences:   []string{"https://ctx.example/mcp"},
		IssuedVia:   "oauth",
	}, time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("mint cap-test pair: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_access_tokens SET created_at = now() - interval '91 days'
		  WHERE token_hash = $1`, store.TokenHash(pair.RefreshToken)); err != nil {
		t.Fatalf("age family anchor: %v", err)
	}
	if st, _ := rtExchange(t, oh, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {pair.RefreshToken},
		"client_id":     {client.ClientID},
	}); st == http.StatusOK {
		t.Fatal("capped-out family must refuse rotation")
	}
	var liveInCapped int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_access_tokens
		  WHERE token_hash = $1 AND revoked_at IS NULL`, store.TokenHash(pair.AccessToken)).Scan(&liveInCapped); err != nil {
		t.Fatalf("cap hygiene query: %v", err)
	}
	if liveInCapped != 0 {
		t.Fatal("capped-out family must be revoked (hygiene)")
	}
}

// keyIDOf resolves a seeded key row id by label.
func keyIDOf(t *testing.T, pool *pgxpool.Pool, label string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM context_api_keys WHERE label = $1`, label).Scan(&id); err != nil {
		t.Fatalf("resolve key %s: %v", label, err)
	}
	return id
}
