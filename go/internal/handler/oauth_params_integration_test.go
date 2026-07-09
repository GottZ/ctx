//go:build integration

// Integration gates for OAuth wave S5 (design/03 §7 W03-5/W03-6,
// negativ-geprobt) against a real PG18 testcontainer:
//
//   - plain-PKCE → 400 invalid_request; ABSENT method → 400 (RFC 7636 §4.3
//     defaults absent to plain — both shapes rejected)
//   - resource=https://fremd/… at /authorize → invalid_target; the
//     canonical resource passes and lands on the code row
//   - resource mismatch at /token → invalid_target
//   - redirect_uri rebind at /token: presented-but-different → invalid_grant;
//     presented-and-equal → 200; absent → 200 (OAuth 2.1 drops the
//     token-side requirement, PKCE is enforced)
//   - audience membership at the auth chain (canonical issuer configured):
//     a token whose audiences lack the ctx-MCP resource → 401; with it →
//     200 (RFC 8707 Mengen-Check am /mcp)
//   - 401 carries WWW-Authenticate with resource_metadata AND scope
//     (RFC 9728 §5.1)
//   - discovery: scopes_supported ["mcp"] in AS metadata + PRM; the PRM
//     path-insertion form answers the same document
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestOAuthParams -count=1 -v
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

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const opIssuer = "https://ctx.example"

// opAuthorize posts one /authorize form and returns (status, redirect code).
func opAuthorize(t *testing.T, oh *OAuthHandler, form url.Values) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	oh.Authorize(rec, req)
	if rec.Code != http.StatusFound {
		return rec.Code, ""
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad redirect: %v", err)
	}
	return rec.Code, loc.Query().Get("code")
}

func TestOAuthParamsS5_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	t.Setenv(EnvCanonicalIssuer, opIssuer)

	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('op-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	plainKey := "dd44" + strings.Repeat("cd", 30)
	keyHash := sha256.Sum256([]byte(plainKey))
	var tenantID, keyID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ('op-x','op-x') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('op-scope',$1::uuid)`, tenantID); err != nil {
		t.Fatalf("map scope: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 VALUES (encode($1,'hex'), 'op-key', 'op-scope', $2::uuid, $3::uuid) RETURNING id::text`,
		keyHash[:], tenantID, principalID).Scan(&keyID); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	client, _, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
		Label: "op-client", TokenEndpointAuthMethod: "none", Source: "admin",
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}

	oh := NewOAuthHandler(pool)
	verifier := strings.Repeat("q", 64)
	chal := sha256.Sum256([]byte(verifier))
	baseForm := func() url.Values {
		return url.Values{
			"api_key":               {plainKey},
			"client_id":             {client.ClientID},
			"redirect_uri":          {"https://claude.ai/api/mcp/auth_callback"},
			"code_challenge":        {base64.RawURLEncoding.EncodeToString(chal[:])},
			"code_challenge_method": {"S256"},
		}
	}

	// --- PKCE method strictness ----------------------------------------------
	plainForm := baseForm()
	plainForm.Set("code_challenge_method", "plain")
	if st, _ := opAuthorize(t, oh, plainForm); st != http.StatusBadRequest {
		t.Fatalf("plain PKCE: got %d, want 400", st)
	}
	absentForm := baseForm()
	absentForm.Del("code_challenge_method")
	if st, _ := opAuthorize(t, oh, absentForm); st != http.StatusBadRequest {
		t.Fatalf("absent PKCE method: got %d, want 400 (defaults to plain)", st)
	}

	// --- resource validation at /authorize -----------------------------------
	foreignForm := baseForm()
	foreignForm.Set("resource", "https://fremd.example/api")
	if st, _ := opAuthorize(t, oh, foreignForm); st != http.StatusBadRequest {
		t.Fatalf("foreign resource: got %d, want 400 invalid_target", st)
	}
	okForm := baseForm()
	okForm.Set("resource", opIssuer+"/mcp")
	st, code := opAuthorize(t, oh, okForm)
	if st != http.StatusFound || code == "" {
		t.Fatalf("canonical resource must pass: %d", st)
	}
	var storedResource string
	if err := pool.QueryRow(ctx,
		`SELECT resource FROM context_oauth_codes LIMIT 1`).Scan(&storedResource); err != nil {
		t.Fatalf("read code row resource: %v", err)
	}
	if storedResource != opIssuer+"/mcp" {
		t.Fatalf("code row resource = %q, want canonical", storedResource)
	}

	exchange := func(code string, extra url.Values) (int, map[string]any) {
		tf := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"code_verifier": {verifier},
			"client_id":     {client.ClientID},
		}
		for k, vs := range extra {
			tf[k] = vs
		}
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(tf.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		oh.Token(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	// --- /token: resource mismatch, rebind mismatch (each consumes a code) ---
	if st, body := exchange(code, url.Values{"resource": {"https://fremd.example/api"}}); st == http.StatusOK || body["error"] != "invalid_target" {
		t.Fatalf("token resource mismatch: %d %v (want invalid_target)", st, body)
	}
	_, code = opAuthorize(t, oh, okForm)
	if st, body := exchange(code, url.Values{"redirect_uri": {"https://other.example/cb"}}); st == http.StatusOK || body["error"] != "invalid_grant" {
		t.Fatalf("rebind mismatch: %d %v (want invalid_grant)", st, body)
	}
	// Rebind equal + canonical resource → 200.
	_, code = opAuthorize(t, oh, okForm)
	st, body := exchange(code, url.Values{
		"redirect_uri": {"https://claude.ai/api/mcp/auth_callback"},
		"resource":     {opIssuer + "/mcp"},
	})
	if st != http.StatusOK {
		t.Fatalf("equal rebind + canonical resource must pass: %d %v", st, body)
	}
	goodToken, _ := body["access_token"].(string)

	// --- audience membership at the auth chain -------------------------------
	authStatus := func(cred string) (int, http.Header) {
		h := Auth(pool)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+cred)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Header()
	}
	if st, _ := authStatus(goodToken); st != http.StatusOK {
		t.Fatalf("token with canonical audience: got %d, want 200", st)
	}
	// A pair minted with a FOREIGN audience (store-level, simulating a row
	// that does not carry our resource) must fail the membership check.
	foreignPair, err := store.MintTokenPair(ctx, pool, store.OAuthToken{
		APIKeyID: keyID, PrincipalID: principalID, ClientID: client.ClientID,
		Audiences: []string{"https://fremd.example/api"}, IssuedVia: "oauth",
	}, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("mint foreign-audience pair: %v", err)
	}
	st, hdr := authStatus(foreignPair.AccessToken)
	if st != http.StatusUnauthorized {
		t.Fatalf("token without ctx-MCP audience: got %d, want 401", st)
	}

	// --- 401 WWW-Authenticate carries resource_metadata + scope --------------
	www := hdr.Get("WWW-Authenticate")
	if !strings.Contains(www, `resource_metadata="`+opIssuer+`/.well-known/oauth-protected-resource/mcp"`) ||
		!strings.Contains(www, `scope="mcp"`) {
		t.Fatalf("401 WWW-Authenticate incomplete: %q", www)
	}

	// --- discovery: scopes_supported + PRM (root and path-insertion form) ----
	fetchDoc := func(path string, serve func(http.ResponseWriter, *http.Request)) map[string]any {
		rec := httptest.NewRecorder()
		serve(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return doc
	}
	asDoc := fetchDoc("/.well-known/oauth-authorization-server", oh.Metadata)
	if sc, _ := asDoc["scopes_supported"].([]any); len(sc) != 1 || sc[0] != "mcp" {
		t.Fatalf("AS scopes_supported = %v", asDoc["scopes_supported"])
	}
	prmRoot := fetchDoc("/.well-known/oauth-protected-resource", oh.ProtectedResource)
	prmPath := fetchDoc("/.well-known/oauth-protected-resource/mcp", oh.ProtectedResource)
	a, _ := json.Marshal(prmRoot)
	b, _ := json.Marshal(prmPath)
	if string(a) != string(b) {
		t.Fatalf("PRM path-insertion form diverges from root form:\n%s\n%s", a, b)
	}
	if prmRoot["resource"] != opIssuer+"/mcp" {
		t.Fatalf("PRM resource = %v", prmRoot["resource"])
	}
}
