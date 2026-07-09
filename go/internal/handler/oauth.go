// Package handler — Minimal OAuth 2.1 PKCE server for MCP remote auth.
// Maps existing ctx API keys to OAuth Bearer tokens. No external OAuth provider needed.
//
// Flow: claude.ai → /authorize → user enters API key → redirect with code →
// claude.ai → /token → gets Bearer token (= API key) → MCP calls with Bearer.
package handler

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OAuthHandler implements the minimal OAuth 2.1 endpoints for MCP remote auth.
type OAuthHandler struct {
	pool *pgxpool.Pool
}

// NewOAuthHandler creates a new OAuthHandler.
func NewOAuthHandler(pool *pgxpool.Pool) *OAuthHandler {
	return &OAuthHandler{pool: pool}
}

// codeSealDomain versions the code→key derivation for the transitional
// api_key_sealed blob (098, W03-1→W03-3). Part of the persisted format.
const codeSealDomain = "ctx:oauth-code-seal:v1:"

// codeHash is the DB lookup form of an authorization code — the plaintext
// code never reaches the DB (design 03 §3).
func codeHash(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// codeSealAEAD derives the AES-256-GCM instance for one code's transitional
// api_key_sealed blob. The key is derived FROM THE CODE (domain-separated
// SHA-256) — the DB stores only code_hash, so a dump/backup alone cannot
// decrypt the blob; only the code bearer (the OAuth client at /token) can.
// Weaker-alternative note: sealbox (CTX_SECRETS_KEY) would put the unseal
// key in the same env as the DB credentials and is optional at boot; the
// code-derived key needs no config and is strictly bound to the flow.
func codeSealAEAD(code string) (cipher.AEAD, error) {
	key := sha256.Sum256([]byte(codeSealDomain + code))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("oauth: seal cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// sealAPIKey encrypts the plaintext api key under the code-derived key.
// Layout: nonce || ciphertext (GCM tag included).
func sealAPIKey(code, apiKey string) ([]byte, error) {
	aead, err := codeSealAEAD(code)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("oauth: seal nonce: %w", err)
	}
	return append(nonce, aead.Seal(nil, nonce, []byte(apiKey), nil)...), nil
}

// openAPIKey decrypts the transitional blob with the presented code. Fails
// (authentication error) on any tamper or on a wrong code — which cannot
// normally happen, since the row was looked up by this code's hash.
func openAPIKey(code string, sealed []byte) (string, error) {
	aead, err := codeSealAEAD(code)
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize() {
		return "", fmt.Errorf("oauth: sealed blob too short")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("oauth: unseal: %w", err)
	}
	return string(plain), nil
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

	// Persist the pending code (098, S1): the DB row carries the INV-A
	// single-key selector + principal anchor from the AuthResult — the
	// plaintext key exists only transiently here and inside the code-sealed
	// transitional blob (see codeSealAEAD; W03-3 removes it with the opaque
	// tokens). The plaintext code never reaches the DB either (code_hash).
	sealed, err := sealAPIKey(code, apiKey)
	if err != nil {
		slog.Error("oauth: seal api key for code row", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := store.PutOAuthCode(r.Context(), h.pool, codeHash(code), store.OAuthCode{
		APIKeyID:      result.ApiKeyID,
		PrincipalID:   result.PrincipalID,
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		CodeChallenge: codeChallenge,
		APIKeySealed:  sealed,
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	}); err != nil {
		slog.Error("oauth: persist authorization code", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

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

	// Atomic single-use take (098, S1): DELETE … RETURNING — of two racing
	// /token calls exactly one gets the row, on ANY instance sharing the DB
	// (the in-memory map's single-instance gap, closed). Expiry check follows
	// the same order as the previous in-memory take (consume, then reject).
	ac, err := store.TakeOAuthCode(r.Context(), h.pool, codeHash(code))
	if err != nil {
		slog.Error("oauth: take authorization code", "error", err)
		oauthError(w, "invalid_grant", "code expired or not found")
		return
	}
	if ac == nil || time.Now().After(ac.ExpiresAt) {
		oauthError(w, "invalid_grant", "code expired or not found")
		return
	}

	// Verify PKCE: SHA256(code_verifier) must match code_challenge.
	hash := sha256.Sum256([]byte(codeVerifier))
	computed := base64.RawURLEncoding.EncodeToString(hash[:])
	if computed != ac.CodeChallenge {
		oauthError(w, "invalid_grant", "code_verifier mismatch")
		return
	}

	// Issue token — the API key IS the token (E2 passthrough until W03-3/S3
	// mints opaque tokens). The plaintext is recovered from the transitional
	// code-sealed blob; only the code bearer can perform this step.
	apiKey, err := openAPIKey(code, ac.APIKeySealed)
	if err != nil {
		slog.Error("oauth: unseal api key", "error", err)
		oauthError(w, "invalid_grant", "code expired or not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": apiKey,
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
