//go:build integration

// Integration gates for the /authorize consent screen (K7, advisory §6)
// against a real PG18 testcontainer:
//
//   - GET validates client_id + redirect_uri BEFORE rendering: unknown or
//     missing client, an unregistered redirect for a DCR client, a
//     non-allowlisted redirect for an admin client → 4xx error page WITHOUT
//     an api_key field.
//   - GET for a valid open-DCR client shows the (escaped) client name, the
//     client_id, the redirect origin and the self-registration warning; an
//     empty name shows "unnamed client"; an admin client shows no warning.
//   - The page carries the script-free CSP.
//   - POST without (or with a wrong) consent value → no code, no code row.
//   - POST with consent → 302 with code (positive control); POST with a bad
//     key → 401 re-render, no code.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestOAuthConsentK7_Integration -count=1 -v
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

const k7Warning = "This client registered itself. Only continue if you started this flow from that application and recognise the address above."

func TestOAuthConsentK7_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var principalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('k7-person') RETURNING id::text`).Scan(&principalID); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	plainKey := "ee55" + strings.Repeat("ef", 30)
	keyHash := sha256.Sum256([]byte(plainKey))
	var tenantID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ('k7-x','k7-x') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('k7-scope',$1::uuid)`, tenantID); err != nil {
		t.Fatalf("map scope: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 VALUES (encode($1,'hex'), 'k7-key', 'k7-scope', $2::uuid, $3::uuid)`,
		keyHash[:], tenantID, principalID); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	oh := NewOAuthHandler(pool)

	// An open-mode DCR registration — exactly the advisory §6 attacker
	// shape: anonymous, attacker-owned https callback, hostile client_name.
	evilCB := "https://evil.example/cb/" + strings.Repeat("p", 80)
	register := func(t *testing.T, body string) string {
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
		var doc map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		id, _ := doc["client_id"].(string)
		return id
	}
	evilID := register(t, `{"redirect_uris":["`+evilCB+`"],"client_name":"<script>alert(1)</script>Totally ctx"}`)
	namelessID := register(t, `{"redirect_uris":["https://nameless.example/cb"]}`)
	adminClient, _, err := store.RegisterOAuthClient(ctx, pool, store.RegisterOAuthClientSpec{
		Label: "k7-admin", TokenEndpointAuthMethod: "none", Source: "admin",
	})
	if err != nil {
		t.Fatalf("register admin client: %v", err)
	}
	staticCB := "https://claude.ai/api/mcp/auth_callback"

	verifier := strings.Repeat("k", 64)
	chal := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(chal[:])

	get := func(t *testing.T, q url.Values) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		oh.Authorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
		return rec
	}
	query := func(clientID, redirectURI string) url.Values {
		return url.Values{
			"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st-1"},
		}
	}
	codeRows := func(t *testing.T, clientID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_oauth_codes WHERE client_id = $1`, clientID).Scan(&n); err != nil {
			t.Fatalf("count codes: %v", err)
		}
		return n
	}
	post := func(t *testing.T, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		oh.Authorize(rec, req)
		return rec
	}
	postForm := func(clientID, redirectURI, key string) url.Values {
		f := query(clientID, redirectURI)
		f.Set("api_key", key)
		return f
	}

	// --- GET: invalid client/redirect → error page, never the key field -----
	for name, q := range map[string]url.Values{
		"UnknownClient":           query("ctx_doesnotexist", staticCB),
		"MissingClient":           query("", staticCB),
		"DCRUnregisteredRedirect": query(evilID, "https://evil.example/other"),
		"DCRStaticRedirect":       query(evilID, staticCB), // registered list is exclusive
		"AdminForeignRedirect":    query(adminClient.ClientID, "https://attacker.example/cb"),
		"NonHTTPRedirect":         query(adminClient.ClientID, "javascript:alert(1)"),
	} {
		t.Run("GetRejects_"+name, func(t *testing.T) {
			rec := get(t, q)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("status = %d, want 4xx", rec.Code)
			}
			body := rec.Body.String()
			if strings.Contains(body, `name="api_key"`) || strings.Contains(body, `type="password"`) {
				t.Fatalf("error page offers the key field:\n%s", body)
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != consentCSP {
				t.Errorf("CSP = %q, want %q", got, consentCSP)
			}
		})
	}

	// --- GET: valid DCR client → name, client_id, origin, warning ------------
	t.Run("GetDCRClientShowsIdentityAndWarning", func(t *testing.T) {
		rec := get(t, query(evilID, evilCB))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			`&lt;script&gt;alert(1)&lt;/script&gt;Totally ctx`, // escaped name
			evilID,                 // client_id
			`https://evil.example`, // redirect origin
			k7Warning,
			`name="api_key"`,
			`name="consent"`,
			`value="` + consentFieldValue + `"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("consent page lacks %q", want)
			}
		}
		if strings.Contains(body, "<script>") {
			t.Error("client_name rendered unescaped")
		}
		// The visible path is shortened (the hidden redirect_uri field
		// round-trips the full value, so only the display span is probed).
		if !strings.Contains(body, `<span class="mono">/cb/`+strings.Repeat("p", consentPathMax-4)+"…</span>") {
			t.Error("redirect path not shortened in the display")
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != consentCSP {
			t.Errorf("CSP = %q, want %q", got, consentCSP)
		}
	})

	t.Run("GetNamelessClientSaysUnnamed", func(t *testing.T) {
		rec := get(t, query(namelessID, "https://nameless.example/cb"))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "unnamed client") {
			t.Fatalf("status %d, body lacks \"unnamed client\":\n%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("GetAdminClientNoWarning", func(t *testing.T) {
		rec := get(t, query(adminClient.ClientID, staticCB))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, k7Warning) {
			t.Error("operator-made client carries the self-registration warning")
		}
		if !strings.Contains(body, "k7-admin") || !strings.Contains(body, "https://claude.ai") {
			t.Error("admin client page lacks name or origin")
		}
	})

	// --- POST: consent is checked server-side ---------------------------------
	t.Run("PostWithoutConsentIssuesNoCode", func(t *testing.T) {
		before := codeRows(t, evilID)
		rec := post(t, postForm(evilID, evilCB, plainKey))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Fatalf("redirected without consent: %s", loc)
		}
		if after := codeRows(t, evilID); after != before {
			t.Fatalf("code rows %d → %d without consent", before, after)
		}
		if !strings.Contains(rec.Body.String(), k7Warning) {
			t.Error("re-rendered consent page lost the warning")
		}
	})

	t.Run("PostWrongConsentValueIssuesNoCode", func(t *testing.T) {
		f := postForm(evilID, evilCB, plainKey)
		f.Set("consent", "on")
		rec := post(t, f)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Fatalf("status = %d, Location %q — want 400 without redirect", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("PostBadKeyRerendersNoCode", func(t *testing.T) {
		before := codeRows(t, evilID)
		f := postForm(evilID, evilCB, "ff66"+strings.Repeat("00", 30))
		f.Set("consent", consentFieldValue)
		rec := post(t, f)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Location") != "" {
			t.Fatalf("status = %d, want 401 without redirect", rec.Code)
		}
		if after := codeRows(t, evilID); after != before {
			t.Fatalf("code rows %d → %d on a bad key", before, after)
		}
		if body := rec.Body.String(); !strings.Contains(body, "Invalid API key.") || strings.Contains(body, "javascript:") {
			t.Errorf("bad-key page: want re-rendered form without javascript: link:\n%s", body)
		}
	})

	t.Run("PostUnknownClientIsKeylessError", func(t *testing.T) {
		f := postForm("ctx_doesnotexist", staticCB, plainKey)
		f.Set("consent", consentFieldValue)
		rec := post(t, f)
		if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), `name="api_key"`) {
			t.Fatalf("status = %d, want 400 key-less error page", rec.Code)
		}
	})

	t.Run("PostWithConsentIssuesCode", func(t *testing.T) {
		f := postForm(evilID, evilCB, plainKey)
		f.Set("consent", consentFieldValue)
		rec := post(t, f)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d (%s), want 302", rec.Code, rec.Body.String())
		}
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || loc.Query().Get("code") == "" || loc.Query().Get("state") != "st-1" {
			t.Fatalf("redirect lacks code/state: %q", rec.Header().Get("Location"))
		}
		if !strings.HasPrefix(loc.String(), evilCB) {
			t.Fatalf("redirect target %q, want the registered callback", loc.String())
		}
	})
}
