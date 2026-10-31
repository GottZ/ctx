//go:build integration

// Integration gate for GHSA 2026-10-31 §9c (reported privately): deleting
// (deactivating) an OAuth client must end everything the client holds,
// against a real PG18 testcontainer and the production /authorize, /token
// and Auth-middleware paths.
//
//   - read-side predicate (rows a pre-fix delete left live): a client
//     deactivated by plain SQL — tokens NOT revoked — already loses its
//     access token (Auth chain 401), its refresh rotation (store miss) and
//     /token (401 invalid_client instead of the old secret-less pass)
//   - DeleteOAuthClient revokes the client's access + refresh rows and drops
//     its unredeemed codes in one transaction: access 401, refresh 401
//     invalid_client, pending code 401, and even after a hand re-activation
//     the revoked refresh stays dead (400 invalid_grant)
//   - unaffected: a second client's tokens and a login pair (client_id NULL)
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestOAuthClientDelete -count=1 -v
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestOAuthClientDelete_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('cd-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	plainKey := "cd11" + strings.Repeat("ab", 30)
	_, keyID := otSeedTenantKey(t, pool, "cd-x", "cd-scope-x", plainKey, principalID)

	const cb = "https://claude.ai/api/mcp/auth_callback"
	register := func(label string) string {
		c, _, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
			Label: label, RedirectURIs: []string{cb}, TokenEndpointAuthMethod: "none", Source: "admin",
		})
		if err != nil {
			t.Fatalf("register %s: %v", label, err)
		}
		return c.ClientID
	}
	victim, bystander := register("cd-victim"), register("cd-bystander")

	oh := NewOAuthHandler(pool)
	verifier := strings.Repeat("w", 64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	code := func(clientID string) string {
		st, c := opAuthorize(t, oh, url.Values{
			"api_key": {plainKey}, "client_id": {clientID}, "redirect_uri": {cb},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"},
			"consent": {consentFieldValue}, // K7 (§6): explicit consent at POST /authorize
		})
		if st != http.StatusFound || c == "" {
			t.Fatalf("authorize %s: got %d", clientID, st)
		}
		return c
	}
	codeForm := func(clientID, c string) url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {c},
			"code_verifier": {verifier}, "client_id": {clientID}}
	}
	refreshForm := func(clientID, rt string) url.Values {
		return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {clientID}}
	}
	pair := func(clientID string) (access, refresh string) {
		st, body, _ := caToken(t, oh, codeForm(clientID, code(clientID)), "", "")
		if st != http.StatusOK {
			t.Fatalf("token %s: got %d %v", clientID, st, body)
		}
		access, _ = body["access_token"].(string)
		refresh, _ = body["refresh_token"].(string)
		if access == "" || refresh == "" {
			t.Fatalf("token %s: incomplete pair %v", clientID, body)
		}
		return access, refresh
	}
	bearer := func(tok string) int {
		st, _ := otAuthProbe(t, pool, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
		return st
	}
	setActive := func(clientID string, active bool) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_oauth_clients SET active = $2 WHERE client_id = $1`, clientID, active); err != nil {
			t.Fatalf("set active=%v on %s: %v", active, clientID, err)
		}
	}

	vAccess, vRefresh := pair(victim)
	bAccess, _ := pair(bystander)
	login, err := store.MintTokenPair(ctx, pool, store.OAuthToken{
		APIKeyID: keyID, PrincipalID: principalID, ClientID: "",
		Audiences: []string{"https://ctx.example/mcp", "https://ctx.example/web"}, IssuedVia: "login",
	}, accessTokenTTL(), refreshTokenTTL())
	if err != nil {
		t.Fatalf("mint login pair: %v", err)
	}
	if bearer(vAccess) != http.StatusOK || bearer(bAccess) != http.StatusOK || bearer(login.AccessToken) != http.StatusOK {
		t.Fatal("baseline: all three access tokens must authenticate")
	}

	// --- read-side predicate: deactivated by SQL, tokens NOT revoked --------
	setActive(victim, false)
	if st := bearer(vAccess); st != http.StatusUnauthorized {
		t.Fatalf("access token of a deactivated client: got %d, want 401", st)
	}
	if _, outcome, err := store.RotateRefreshToken(ctx, pool, vRefresh, victim,
		accessTokenTTL(), refreshTokenTTL(), refreshFamilyCap()); err != nil || outcome != store.RotateMiss {
		t.Fatalf("rotation for a deactivated client: outcome %v err %v, want RotateMiss", outcome, err)
	}
	if st, body, _ := caToken(t, oh, refreshForm(victim, vRefresh), "", ""); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("/token refresh for a deactivated client: got %d %v, want 401 invalid_client", st, body["error"])
	}
	setActive(victim, true)
	if st := bearer(vAccess); st != http.StatusOK {
		t.Fatalf("re-activated client: access got %d, want 200 (the predicate, not a revoke, refused it)", st)
	}

	// --- DeleteOAuthClient: revoke + drop codes in one tx -------------------
	pendingCode := code(victim)
	deleted, err := store.DeleteOAuthClient(ctx, pool, victim)
	if err != nil || !deleted {
		t.Fatalf("delete client: deleted=%v err=%v", deleted, err)
	}
	var live, codes int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_access_tokens WHERE client_id = $1 AND revoked_at IS NULL`, victim).Scan(&live); err != nil {
		t.Fatalf("count live tokens: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_oauth_codes WHERE client_id = $1`, victim).Scan(&codes); err != nil {
		t.Fatalf("count codes: %v", err)
	}
	if live != 0 || codes != 0 {
		t.Fatalf("after delete: %d live tokens, %d codes for the client, want 0/0", live, codes)
	}
	if st := bearer(vAccess); st != http.StatusUnauthorized {
		t.Fatalf("access after delete: got %d, want 401", st)
	}
	if st, body, _ := caToken(t, oh, refreshForm(victim, vRefresh), "", ""); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("refresh after delete: got %d %v, want 401 invalid_client", st, body["error"])
	}
	if st, body, _ := caToken(t, oh, codeForm(victim, pendingCode), "", ""); st != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("pending code after delete: got %d %v, want 401 invalid_client", st, body["error"])
	}

	// A hand re-activation does not resurrect the revoked rows.
	setActive(victim, true)
	if st := bearer(vAccess); st != http.StatusUnauthorized {
		t.Fatalf("access after delete + re-activation: got %d, want 401 (revoked)", st)
	}
	if st, body, _ := caToken(t, oh, refreshForm(victim, vRefresh), "", ""); st != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("refresh after delete + re-activation: got %d %v, want 400 invalid_grant", st, body["error"])
	}

	// --- bystanders untouched -----------------------------------------------
	if st := bearer(bAccess); st != http.StatusOK {
		t.Fatalf("other client's access token: got %d, want 200", st)
	}
	if st := bearer(login.AccessToken); st != http.StatusOK {
		t.Fatalf("login access token (client_id NULL): got %d, want 200", st)
	}
	if _, outcome, err := store.RotateRefreshToken(ctx, pool, login.RefreshToken, "",
		accessTokenTTL(), refreshTokenTTL(), refreshFamilyCap()); err != nil || outcome != store.Rotated {
		t.Fatalf("login refresh rotation: outcome %v err %v, want Rotated", outcome, err)
	}
}
