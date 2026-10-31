//go:build integration

// Integration gates for the registered grant_types at /token (advisory §6
// follow-up) against a real PG18 testcontainer:
//
//   - A DCR client registered with grant_types [authorization_code] gets an
//     access token WITHOUT refresh_token; the access token works at /mcp;
//     a refresh_token grant for it is 400 unauthorized_client and leaves a
//     store-minted refresh row untouched (no rotation, no revoke).
//   - A DCR client registered with [refresh_token] only cannot redeem a
//     code: 400 unauthorized_client, and the code is NOT consumed.
//   - A DCR client registered WITHOUT grant_types gets the default
//     [authorization_code, refresh_token]: pair issued, refresh rotates.
//   - An operator (admin) client with the 097 column default
//     [authorization_code] keeps both grants (legacy rows, claude.ai).
//   - A deactivated client and an unregistered client_id are 401
//     invalid_client on the refresh grant (§9c, authenticateClient); an
//     empty client_id still reaches the grant_types gate: invalid_grant.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestOAuthGrantTypes_Integration -count=1 -v
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestOAuthGrantTypes_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('gt-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	plainKey := "ab77" + strings.Repeat("12", 30)
	keyHash := sha256.Sum256([]byte(plainKey))
	var tenantID, keyID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ('gt-x','gt-x') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('gt-scope',$1::uuid)`, tenantID); err != nil {
		t.Fatalf("map scope: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 VALUES (encode($1,'hex'), 'gt-key', 'gt-scope', $2::uuid, $3::uuid) RETURNING id::text`,
		keyHash[:], tenantID, principalID).Scan(&keyID); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	oh := NewOAuthHandler(pool)
	verifier := strings.Repeat("g", 64)
	chal := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(chal[:])

	// register runs one open-mode DCR registration and returns the client_id
	// plus the grant_types the server stored.
	register := func(t *testing.T, body string) (string, []string) {
		t.Helper()
		t.Setenv(EnvDCRMode, "open")
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = dcrTestAddr()
		rec := httptest.NewRecorder()
		oh.Register(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("register: %d (%s)", rec.Code, rec.Body.String())
		}
		var doc struct {
			ClientID   string   `json:"client_id"`
			GrantTypes []string `json:"grant_types"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		return doc.ClientID, doc.GrantTypes
	}
	// code runs the consented /authorize POST and returns the code.
	code := func(t *testing.T, clientID, redirectURI string) string {
		t.Helper()
		st, c := opAuthorize(t, oh, url.Values{
			"api_key":               {plainKey},
			"client_id":             {clientID},
			"redirect_uri":          {redirectURI},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"consent":               {consentFieldValue},
		})
		if st != http.StatusFound || c == "" {
			t.Fatalf("authorize %s: status %d", clientID, st)
		}
		return c
	}
	exchange := func(t *testing.T, clientID, c string) (int, map[string]any) {
		t.Helper()
		return rtExchange(t, oh, url.Values{
			"grant_type": {"authorization_code"}, "code": {c},
			"code_verifier": {verifier}, "client_id": {clientID},
		})
	}
	refresh := func(t *testing.T, clientID, rt string) (int, map[string]any) {
		t.Helper()
		return rtExchange(t, oh, url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {clientID},
		})
	}
	// storeRefresh mints a pair at store level — a refresh row this client
	// could only hold from before the enforcement (or from a bug).
	storeRefresh := func(t *testing.T, clientID string) string {
		t.Helper()
		pair, err := store.MintTokenPair(ctx, pool, store.OAuthToken{
			APIKeyID: keyID, PrincipalID: principalID, ClientID: clientID,
			Audiences: []string{"https://example.test/mcp"}, IssuedVia: "oauth",
		}, time.Hour, 24*time.Hour)
		if err != nil {
			t.Fatalf("mint store pair: %v", err)
		}
		return pair.RefreshToken
	}
	refreshRowLive := func(t *testing.T, rt string) bool {
		t.Helper()
		var live bool
		if err := pool.QueryRow(ctx,
			`SELECT revoked_at IS NULL FROM context_access_tokens WHERE token_hash = $1`, store.TokenHash(rt)).Scan(&live); err != nil {
			t.Fatalf("refresh row lookup: %v", err)
		}
		return live
	}

	t.Run("CodeOnlyClientGetsNoRefresh", func(t *testing.T) {
		cb := "https://code-only.example/cb"
		id, gts := register(t, `{"redirect_uris":["`+cb+`"],"grant_types":["authorization_code"]}`)
		if !slices.Equal(gts, []string{"authorization_code"}) {
			t.Fatalf("registered grant_types = %v", gts)
		}
		st, body := exchange(t, id, code(t, id, cb))
		if st != http.StatusOK {
			t.Fatalf("code exchange: %d (%v)", st, body)
		}
		access, _ := body["access_token"].(string)
		if !strings.HasPrefix(access, store.AccessTokenPrefix) {
			t.Fatalf("access_token = %q", access)
		}
		if _, has := body["refresh_token"]; has {
			t.Fatalf("code-only client received a refresh_token: %v", body)
		}
		if got := rtAuthStatus(t, pool, access); got != http.StatusOK {
			t.Fatalf("access-only token at /mcp: %d, want 200", got)
		}
		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_access_tokens WHERE client_id = $1 AND token_type = 'refresh'`, id).Scan(&rows); err != nil {
			t.Fatalf("count refresh rows: %v", err)
		}
		if rows != 0 {
			t.Fatalf("%d refresh rows minted for a code-only client", rows)
		}

		rt := storeRefresh(t, id)
		st, body = refresh(t, id, rt)
		if st != http.StatusBadRequest || body["error"] != "unauthorized_client" {
			t.Fatalf("refresh grant for code-only client: %d %v, want 400 unauthorized_client", st, body)
		}
		if !refreshRowLive(t, rt) {
			t.Fatal("refused refresh grant revoked/rotated the row (must have no side effect)")
		}
	})

	t.Run("RefreshOnlyClientCannotRedeemCode", func(t *testing.T) {
		cb := "https://refresh-only.example/cb"
		id, _ := register(t, `{"redirect_uris":["`+cb+`"],"grant_types":["refresh_token"]}`)
		c := code(t, id, cb)
		st, body := exchange(t, id, c)
		if st != http.StatusBadRequest || body["error"] != "unauthorized_client" {
			t.Fatalf("code grant for refresh-only client: %d %v, want 400 unauthorized_client", st, body)
		}
		var left int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_oauth_codes WHERE code_hash = $1`, codeHash(c)).Scan(&left); err != nil {
			t.Fatalf("code row lookup: %v", err)
		}
		if left != 1 {
			t.Fatal("refused code grant consumed the code")
		}
	})

	t.Run("DefaultRegistrationKeepsRefresh", func(t *testing.T) {
		cb := "https://default.example/cb"
		id, gts := register(t, `{"redirect_uris":["`+cb+`"]}`)
		if !slices.Equal(gts, []string{"authorization_code", "refresh_token"}) {
			t.Fatalf("default grant_types = %v, want [authorization_code refresh_token]", gts)
		}
		st, body := exchange(t, id, code(t, id, cb))
		rt, _ := body["refresh_token"].(string)
		if st != http.StatusOK || !strings.HasPrefix(rt, store.RefreshTokenPrefix) {
			t.Fatalf("default client exchange: %d %v, want a pair", st, body)
		}
		if st, body = refresh(t, id, rt); st != http.StatusOK {
			t.Fatalf("default client refresh: %d %v", st, body)
		}
	})

	t.Run("AdminLegacyDefaultKeepsBothGrants", func(t *testing.T) {
		admin, _, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
			Label: "gt-admin", TokenEndpointAuthMethod: "none", Source: "admin",
		})
		if err != nil {
			t.Fatalf("register admin client: %v", err)
		}
		if !slices.Equal(admin.GrantTypes, []string{"authorization_code"}) {
			t.Fatalf("admin fixture grant_types = %v, want the 097 default", admin.GrantTypes)
		}
		st, body := exchange(t, admin.ClientID, code(t, admin.ClientID, "https://claude.ai/api/mcp/auth_callback"))
		rt, _ := body["refresh_token"].(string)
		if st != http.StatusOK || rt == "" {
			t.Fatalf("admin exchange: %d %v, want a pair", st, body)
		}
		if st, body = refresh(t, admin.ClientID, rt); st != http.StatusOK {
			t.Fatalf("admin refresh: %d %v", st, body)
		}
	})

	t.Run("InactiveOrUnknownClientRefreshIsInvalidClient", func(t *testing.T) {
		cb := "https://gone.example/cb"
		id, _ := register(t, `{"redirect_uris":["`+cb+`"]}`)
		rt := storeRefresh(t, id)
		if _, err := store.DeleteOAuthClient(ctx, pool, id); err != nil {
			t.Fatalf("deactivate client: %v", err)
		}
		if st, body := refresh(t, id, rt); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
			t.Fatalf("refresh for deactivated client: %d %v, want 401 invalid_client", st, body)
		}
		if st, body := refresh(t, "ctx_neverregistered", rt); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
			t.Fatalf("refresh for unknown client: %d %v, want 401 invalid_client", st, body)
		}
		if st, body := refresh(t, "", rt); st != http.StatusBadRequest || body["error"] != "invalid_grant" {
			t.Fatalf("refresh without client_id: %d %v, want 400 invalid_grant", st, body)
		}
	})
}
