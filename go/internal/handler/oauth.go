// Package handler — Minimal OAuth 2.1 PKCE server for MCP remote auth.
// Maps existing ctx API keys to OAuth Bearer tokens. No external OAuth provider needed.
//
// Flow: claude.ai → /authorize → user enters API key → redirect with code →
// claude.ai → /token → gets Bearer token (= API key) → MCP calls with Bearer.
package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OAuthHandler implements the minimal OAuth 2.1 endpoints for MCP remote auth.
type OAuthHandler struct {
	pool  *pgxpool.Pool
	codes *codeStore
}

// NewOAuthHandler creates a new OAuthHandler.
func NewOAuthHandler(pool *pgxpool.Pool) *OAuthHandler {
	return &OAuthHandler{
		pool:  pool,
		codes: newCodeStore(),
	}
}

// authCode is a pending authorization code.
type authCode struct {
	code          string
	apiKey        string
	codeChallenge string
	redirectURI   string
	expiresAt     time.Time
}

// codeStore is a thread-safe in-memory store for pending auth codes.
type codeStore struct {
	mu    sync.Mutex
	codes map[string]*authCode
}

func newCodeStore() *codeStore {
	return &codeStore{codes: make(map[string]*authCode)}
}

func (s *codeStore) put(code *authCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code.code] = code
}

func (s *codeStore) take(code string) *authCode {
	s.mu.Lock()
	defer s.mu.Unlock()
	ac, ok := s.codes[code]
	if !ok {
		return nil
	}
	delete(s.codes, code) // Single use.
	if time.Now().After(ac.expiresAt) {
		return nil
	}
	return ac
}

// Metadata serves GET /.well-known/oauth-authorization-server.
func (h *OAuthHandler) Metadata(w http.ResponseWriter, r *http.Request) {
	scheme := "https"
	issuer := scheme + "://" + r.Host

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":       []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// ProtectedResource serves GET /.well-known/oauth-protected-resource.
func (h *OAuthHandler) ProtectedResource(w http.ResponseWriter, r *http.Request) {
	scheme := "https"
	issuer := scheme + "://" + r.Host

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":                issuer + "/mcp",
		"authorization_servers":   []string{issuer},
		"bearer_methods_supported": []string{"header"},
	})
}

// Authorize handles GET /authorize — shows a form to enter the API key.
// Handles POST /authorize — validates the key, issues a code, redirects.
func (h *OAuthHandler) Authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.authorizeForm(w, r)
		return
	}
	if r.Method == http.MethodPost {
		h.authorizeSubmit(w, r)
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (h *OAuthHandler) authorizeForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	e := html.EscapeString // Shorthand for XSS prevention.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	//nolint:gosec // All dynamic values escaped via html.EscapeString.
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html><head><title>ctx — Authorize</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  body { font-family: system-ui; max-width: 400px; margin: 80px auto; padding: 0 20px; }
  h1 { font-size: 1.4em; }
  input[type=password] { width: 100%; padding: 10px; font-size: 1em; font-family: monospace; box-sizing: border-box; }
  button { margin-top: 12px; padding: 10px 24px; font-size: 1em; cursor: pointer; }
  .hint { color: #666; font-size: 0.85em; margin-top: 8px; }
</style>
</head><body>
<h1>ctx — Authorize MCP Access</h1>
<form method="POST" action="/authorize">
  <label for="key">API Key:</label><br>
  <input type="password" id="key" name="api_key" required autofocus><br>
  <p class="hint">Enter your ctx API key to authorize this client.</p>
  <input type="hidden" name="response_type" value="` + e(q.Get("response_type")) + `">
  <input type="hidden" name="client_id" value="` + e(q.Get("client_id")) + `">
  <input type="hidden" name="redirect_uri" value="` + e(q.Get("redirect_uri")) + `">
  <input type="hidden" name="code_challenge" value="` + e(q.Get("code_challenge")) + `">
  <input type="hidden" name="code_challenge_method" value="` + e(q.Get("code_challenge_method")) + `">
  <input type="hidden" name="state" value="` + e(q.Get("state")) + `">
  <button type="submit">Authorize</button>
</form>
</body></html>`))
}

func (h *OAuthHandler) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	apiKey := auth.SanitizeKey(r.FormValue("api_key"))
	redirectURI := r.FormValue("redirect_uri")
	codeChallenge := r.FormValue("code_challenge")
	state := r.FormValue("state")

	if redirectURI == "" || codeChallenge == "" {
		http.Error(w, "missing redirect_uri or code_challenge", http.StatusBadRequest)
		return
	}

	// Reject non-http(s) redirect targets (javascript:, data:, file:, …) —
	// open-redirect mitigation. Reconstructing the URL through net/url also
	// sanitises any control characters that would otherwise be reflected
	// straight into the Location header.
	parsedRedirect, err := url.Parse(redirectURI)
	if err != nil || parsedRedirect.Host == "" || (parsedRedirect.Scheme != "http" && parsedRedirect.Scheme != "https") {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	// Validate client_id.
	clientID := r.FormValue("client_id")
	if clientID != "" {
		valid, err := store.ValidateOAuthClient(r.Context(), h.pool, clientID)
		if err != nil || !valid {
			slog.Warn("oauth: invalid client_id in authorize", "client_id", clientID)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>
				<h1>Unknown Client</h1><p>This client_id is not registered. Use <code>ctx mcp add</code> to create one.</p>
			</body></html>`))
			return
		}
	}

	// Validate the API key.
	result, err := auth.Authenticate(r.Context(), h.pool, apiKey)
	if err != nil || !result.IsValid {
		slog.Warn("oauth: invalid API key in authorize", "error", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>
			<h1>Invalid API Key</h1><p><a href="javascript:history.back()">Try again</a></p>
		</body></html>`))
		return
	}

	// Generate authorization code.
	codeBytes := make([]byte, 32)
	_, _ = rand.Read(codeBytes)
	code := hex.EncodeToString(codeBytes)

	h.codes.put(&authCode{
		code:          code,
		apiKey:        apiKey,
		codeChallenge: codeChallenge,
		redirectURI:   redirectURI,
		expiresAt:     time.Now().Add(5 * time.Minute),
	})

	// Redirect back to client with code. RFC 6749 §3.1.2 calls for matching
	// redirect_uri against pre-registered values; until oauth_clients carries
	// a redirect_uris column (separate wave), we mitigate open-redirect
	// abuse to two layers: scheme-allowlist above, and reconstruction here
	// (the Location header carries only fields that survived url.Parse).
	q := parsedRedirect.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	parsedRedirect.RawQuery = q.Encode()
	// #nosec G710 -- redirectURI is validated above (scheme allowlist + Host
	// non-empty) and reconstructed via net/url; the residual taint flow
	// goes away once redirect_uris are pre-registered (TODO).
	http.Redirect(w, r, parsedRedirect.String(), http.StatusFound)
}

// Token handles POST /token — exchanges authorization code for access token.
func (h *OAuthHandler) Token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		oauthError(w, "invalid_request", "bad form data")
		return
	}

	grantType := r.FormValue("grant_type")
	code := r.FormValue("code")
	codeVerifier := r.FormValue("code_verifier")

	if grantType != "authorization_code" {
		oauthError(w, "unsupported_grant_type", "only authorization_code is supported")
		return
	}

	ac := h.codes.take(code)
	if ac == nil {
		oauthError(w, "invalid_grant", "code expired or not found")
		return
	}

	// Verify PKCE: SHA256(code_verifier) must match code_challenge.
	hash := sha256.Sum256([]byte(codeVerifier))
	computed := base64.RawURLEncoding.EncodeToString(hash[:])
	if computed != ac.codeChallenge {
		oauthError(w, "invalid_grant", "code_verifier mismatch")
		return
	}

	// Issue token — the API key IS the token.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": ac.apiKey,
		"token_type":   "Bearer",
	})
}

func oauthError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}
