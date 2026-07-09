// Package handler — Minimal OAuth 2.1 PKCE server for MCP remote auth.
// Maps existing ctx API keys to OAuth Bearer tokens. No external OAuth provider needed.
//
// Flow: claude.ai → /authorize → user enters API key → redirect with code →
// claude.ai → /token → gets opaque Bearer token (ctxt_, key-bound, S3) →
// MCP calls with Bearer. Raw api keys stay valid as Bearer (E2 legacy path).
package handler

import (
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
	"os"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OAuthHandler implements the minimal OAuth 2.1 endpoints for MCP remote auth.
type OAuthHandler struct {
	pool *pgxpool.Pool
	// issuer is the canonical issuer (CTX_CANONICAL_ISSUER, no trailing
	// slash) — the RFC 9207 `iss` value and, when set, the base for the
	// discovery documents. Empty → discovery keeps deriving from r.Host
	// (pre-S2 behaviour) and NO `iss` is emitted: an r.Host-derived `iss`
	// would be client-influencable and make the mix-up protection
	// illusory (design 03 RVW-Spec-F4) — no iss beats a forgeable one.
	issuer string
}

// EnvCanonicalIssuer names the canonical-issuer environment variable (S2).
const EnvCanonicalIssuer = "CTX_CANONICAL_ISSUER"

// NewOAuthHandler creates a new OAuthHandler.
func NewOAuthHandler(pool *pgxpool.Pool) *OAuthHandler {
	return &OAuthHandler{
		pool:   pool,
		issuer: strings.TrimRight(strings.TrimSpace(os.Getenv(EnvCanonicalIssuer)), "/"),
	}
}

// redirectAllowlist is the static exact-match set of known MCP-client
// callback URIs (S2/W03-2, design 03 E-03e): the transitional,
// Achse-02-independent closure of GHSA-p3jf-x27v-4jg6 — W03-7 replaces it
// with the per-client redirect_uris registration. Extend per deployment via
// CTX_OAUTH_REDIRECT_EXTRA (comma-separated, exact URIs). Loopback redirects
// (RFC 8252 §7.3) are allowed by rule in redirectAllowed, not listed here.
var redirectAllowlist = map[string]bool{
	"https://claude.ai/api/mcp/auth_callback":            true,
	"https://claude.com/api/mcp/auth_callback":           true,
	"https://chatgpt.com/connector_platform_oauth_redirect": true,
}

// EnvRedirectExtra names the deployment-extension env var for the static
// redirect allowlist (comma-separated exact URIs).
const EnvRedirectExtra = "CTX_OAUTH_REDIRECT_EXTRA"

// redirectAllowed reports whether the (already scheme-checked and parsed)
// redirect target passes the static allowlist: exact match against the known
// callbacks or the env extension, or a loopback target (RFC 8252 §7.3 —
// 127.0.0.1 / ::1 / localhost with any port and path, native-app pattern).
// No substring or wildcard matching anywhere (OAuth 2.1 §4.1.3).
func redirectAllowed(raw string, parsed *url.URL) bool {
	if redirectAllowlist[raw] {
		return true
	}
	for _, extra := range strings.Split(os.Getenv(EnvRedirectExtra), ",") {
		if extra = strings.TrimSpace(extra); extra != "" && extra == raw {
			return true
		}
	}
	host := parsed.Hostname()
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// validateRegisteredRedirectURI enforces the REGISTRATION rule for redirect
// URIs (design 02 §4b, OAuth 2.1 §5.2): https for real hosts, plain http ONLY
// for loopback (localhost / 127.0.0.1 / ::1 — RFC 8252 native apps), anything
// else rejected. Deliberately STRICTER than the runtime scheme gate above:
// this list is what 03 will match EXACTLY at /authorize — a registered
// plaintext-http foreign host would be a permanent code/key exfiltration
// channel. Shared by the CLI pre-registration path (mcp-client-create) and
// the DCR /register handler (02-W4).
func validateRegisteredRedirectURI(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("invalid redirect_uri %q: not an absolute URL", raw)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("invalid redirect_uri %q: plain http is allowed for loopback hosts only", raw)
	default:
		return fmt.Errorf("invalid redirect_uri %q: scheme must be https (or http on loopback)", raw)
	}
}

// codeHash is the DB lookup form of an authorization code — the plaintext
// code never reaches the DB (design 03 §3).
func codeHash(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// Token lifetimes (E-TTL, decided: access 1h · refresh 30d rolling with
// rotation · family absolute cap 90d — "Config-Werte"): the defaults are the
// decision values; the env vars are the deployment override knobs, Go
// duration syntax ("30m", "720h" — hours are the largest unit Go parses).
// The short access TTL bounds revocation latency; continuity comes from the
// rotating refresh (S4); the family cap bounds how long rolling rotation
// can keep one authorization alive without a fresh consent.
const (
	defaultAccessTokenTTL  = time.Hour
	defaultRefreshTokenTTL = 30 * 24 * time.Hour
	defaultRefreshFamilyCap = 90 * 24 * time.Hour

	// EnvAccessTokenTTL / EnvRefreshTokenTTL / EnvRefreshFamilyCap name the
	// optional TTL override env vars (E-TTL "Config-Werte").
	EnvAccessTokenTTL   = "CTX_OAUTH_ACCESS_TTL"  //nolint:gosec // env var NAME, not a credential
	EnvRefreshTokenTTL  = "CTX_OAUTH_REFRESH_TTL" //nolint:gosec // env var NAME, not a credential
	EnvRefreshFamilyCap = "CTX_OAUTH_FAMILY_CAP"
)

// ttlFromEnv reads one duration override; unset, unparsable or non-positive
// values fall back to the decided default (fail-safe — a typo must never
// yield eternal or instant-dead tokens).
func ttlFromEnv(envName string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("oauth: invalid TTL override ignored", "env", envName, "value", raw)
		return def
	}
	return d
}

func accessTokenTTL() time.Duration  { return ttlFromEnv(EnvAccessTokenTTL, defaultAccessTokenTTL) }
func refreshTokenTTL() time.Duration { return ttlFromEnv(EnvRefreshTokenTTL, defaultRefreshTokenTTL) }
func refreshFamilyCap() time.Duration {
	return ttlFromEnv(EnvRefreshFamilyCap, defaultRefreshFamilyCap)
}

// canonicalOrHostIssuer returns the configured canonical issuer (S2,
// RVW-Spec-F4) or falls back to the pre-S2 r.Host derivation. The RFC 9207
// `iss` redirect parameter uses ONLY the canonical value (never the
// fallback) — see the issuer field doc.
func (h *OAuthHandler) canonicalOrHostIssuer(r *http.Request) string {
	if h.issuer != "" {
		return h.issuer
	}
	return "https://" + r.Host
}

// EnvDCRMode names the DCR mode flag (design 02 §4b/E3): open | admin | off.
// Empty/unset counts as off — the /register route only exists once 02-W4
// mounts it, so the default advertises nothing that is not served.
const EnvDCRMode = "CTX_OAUTH_DCR_MODE"

// dcrMode returns the normalized DCR mode. Unknown values fall back to off
// (fail-closed: a typo must never open registration).
func dcrMode() string {
	switch mode := strings.TrimSpace(os.Getenv(EnvDCRMode)); mode {
	case "open", "admin":
		return mode
	default:
		return "off"
	}
}

// Metadata serves GET /.well-known/oauth-authorization-server (RFC 8414).
// Coupling rules (design 02 §4c, 02-W3): grant_types_supported includes
// refresh_token since S4 actually issues and rotates them (the coupling
// rule paid off — advertised exactly when served); scopes_supported is
// omitted until a scope catalog exists that /token enforces — the discovery
// document never advertises what the server does not do.
// registration_endpoint appears only when DCR is switched on (02-W4;
// the flag default is off).
func (h *OAuthHandler) Metadata(w http.ResponseWriter, r *http.Request) {
	issuer := h.canonicalOrHostIssuer(r)

	doc := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		// The requestable scope catalog (S5/W03-5, E-03d): "mcp" is what a
		// client MAY ask for; it is NEVER an authorisation input (INV-B —
		// the key's own scope stays the hard authority, token.scope is not
		// read by any authorisation decision).
		"scopes_supported": []string{"mcp"},
		"code_challenge_methods_supported":       []string{"S256"},
		// none | client_secret_basic | client_secret_post (design 02 §3/§4c;
		// no private_key_jwt — there is no jwks path in the MVP). The
		// constant-time secret ENFORCEMENT at /token is 03/S6.
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		// CIMD (client_id as metadata URL) is a post-MVP expansion; the
		// SSRF-hardened fetch path does not exist yet, so: false.
		"client_id_metadata_document_supported": false,
	}
	if dcrMode() != "off" {
		doc["registration_endpoint"] = issuer + "/register"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// ProtectedResource serves GET /.well-known/oauth-protected-resource — both
// the root form and the RFC 9728 §3.1 path-insertion form for the /mcp
// resource (/.well-known/oauth-protected-resource/mcp, S5/W03-5): clients
// derive the latter from the resource URL <issuer>/mcp, so both spellings
// must answer with the same document.
func (h *OAuthHandler) ProtectedResource(w http.ResponseWriter, r *http.Request) {
	issuer := h.canonicalOrHostIssuer(r)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":                issuer + "/mcp",
		"authorization_servers":   []string{issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":        []string{"mcp"},
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
  <input type="hidden" name="resource" value="` + e(q.Get("resource")) + `">
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

	// PKCE method MUST be S256 (S5/W03-6; OAuth 2.1 §7.5.2 — `plain` leaks
	// the verifier to anything that sees the authorization request, and an
	// ABSENT method defaults to plain per RFC 7636 §4.3, so both reject).
	// The /token side verifies with SHA-256 unconditionally, but rejecting
	// here beats a doomed round trip through the user's key entry.
	if r.FormValue("code_challenge_method") != "S256" {
		oauthError(w, "invalid_request", "code_challenge_method must be S256")
		return
	}

	// RFC 8707 resource validation (S5/W03-6, RVW-Spec-F2/Vollst-F5): ctx is
	// a single-resource server — a present `resource` that is not OUR
	// canonical MCP resource is a mix-up vector and answers invalid_target;
	// the value stored on the code row is only ever the validated canonical
	// one, never the raw client input.
	resource := r.FormValue("resource")
	if resource != "" && resource != h.canonicalOrHostIssuer(r)+"/mcp" {
		oauthError(w, "invalid_target", "unknown resource")
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

	// Static redirect allowlist (S2/W03-2) — the GHSA-p3jf-x27v-4jg6 core:
	// without it, an attacker-controlled redirect_uri carries the code (and
	// with it the victim's key) off-site. Exact match or loopback only;
	// per-client registration replaces this in W03-7.
	if !redirectAllowed(redirectURI, parsedRedirect) {
		slog.Warn("oauth: redirect_uri not in allowlist", "redirect_uri", redirectURI)
		http.Error(w, "invalid redirect_uri: not an allowed callback", http.StatusBadRequest)
		return
	}

	// client_id is MANDATORY (S2/W03-2; OAuth 2.1 §4.1.1). The former
	// `if clientID != ""` anonymous flow was half the takeover surface —
	// without a client identity there is nothing to bind the code to.
	clientID := r.FormValue("client_id")
	if clientID == "" {
		http.Error(w, "client_id is required", http.StatusBadRequest)
		return
	}
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
	// plaintext key exists only transiently in this handler and is gone
	// after this request (since S3 /token mints an opaque token from
	// api_key_id; the S1-transitional sealed-key blob is dropped). The
	// plaintext code never reaches the DB either (code_hash).
	if err := store.PutOAuthCode(r.Context(), h.pool, codeHash(code), store.OAuthCode{
		APIKeyID:      result.ApiKeyID,
		PrincipalID:   result.PrincipalID,
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		CodeChallenge: codeChallenge,
		Resource:      resource, // validated above — canonical or empty
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
	// RFC 9207 `iss` — ONLY from the configured canonical issuer, never an
	// r.Host derivation (a client-influencable iss would defeat the mix-up
	// protection it exists for, RVW-Spec-F4). Unset → omitted, as before S2.
	if h.issuer != "" {
		q.Set("iss", h.issuer)
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

	// client_id arrives in the form body (public, auth method `none`) or as
	// the Basic-Auth username (confidential, enforcement wired in S6). Both
	// grants bind on it: the code binding (S2) and the refresh binding (S4).
	presentedClient := r.FormValue("client_id")
	if presentedClient == "" {
		if u, _, ok := r.BasicAuth(); ok {
			presentedClient = u
		}
	}

	// All form values are extracted HERE, behind the MaxBytesReader +
	// ParseForm above — the grant handlers receive plain strings (gosec
	// G120 cannot see the cap across the function boundary, and neither
	// should a future caller have to remember it).
	req := tokenRequest{
		client:       presentedClient,
		code:         r.FormValue("code"),
		codeVerifier: r.FormValue("code_verifier"),
		refreshToken: r.FormValue("refresh_token"),
		redirectURI:  r.FormValue("redirect_uri"),
		resource:     r.FormValue("resource"),
	}
	switch r.FormValue("grant_type") {
	case "authorization_code":
		h.tokenAuthorizationCode(w, r, req)
	case "refresh_token":
		h.tokenRefresh(w, r, req)
	default:
		oauthError(w, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
	}
}

// tokenRequest carries the /token form values across the grant handlers —
// extracted once in Token() behind the body cap (gosec G120 boundary).
type tokenRequest struct {
	client       string
	code         string
	codeVerifier string
	refreshToken string
	redirectURI  string
	resource     string
}

// tokenRefresh handles the refresh_token grant (S4/W03-4): rotation with
// reuse detection. Every failure class — unknown, expired, wrong client,
// capped-out family AND detected reuse — answers the SAME invalid_grant (no
// oracle); the reuse case additionally revoked the whole family server-side.
func (h *OAuthHandler) tokenRefresh(w http.ResponseWriter, r *http.Request, req tokenRequest) {
	if req.refreshToken == "" || !strings.HasPrefix(req.refreshToken, store.RefreshTokenPrefix) {
		oauthError(w, "invalid_grant", "refresh token invalid or expired")
		return
	}
	pair, outcome, err := store.RotateRefreshToken(r.Context(), h.pool, req.refreshToken, req.client,
		accessTokenTTL(), refreshTokenTTL(), refreshFamilyCap())
	if err != nil {
		slog.Error("oauth: rotate refresh token", "error", err)
		oauthError(w, "invalid_grant", "refresh token invalid or expired")
		return
	}
	switch outcome {
	case store.Rotated:
		writeTokenPair(w, pair)
	case store.RotateReuse:
		// Theft signal: family already revoked inside the rotation tx. Log
		// loudly server-side, answer the standard invalid_grant (RFC 6749
		// §5.2) — the wire carries no detection oracle.
		slog.Warn("oauth: refresh token reuse detected — family revoked", "client_id", req.client)
		oauthError(w, "invalid_grant", "refresh token invalid or expired")
	default:
		oauthError(w, "invalid_grant", "refresh token invalid or expired")
	}
}

// tokenAuthorizationCode handles the authorization_code grant: the S1/S2
// hardened code redemption, minting the S3 opaque pair (S4: + refresh).
func (h *OAuthHandler) tokenAuthorizationCode(w http.ResponseWriter, r *http.Request, req tokenRequest) {
	// Atomic single-use take (098, S1): DELETE … RETURNING — of two racing
	// /token calls exactly one gets the row, on ANY instance sharing the DB
	// (the in-memory map's single-instance gap, closed). Expiry check follows
	// the same order as the previous in-memory take (consume, then reject).
	ac, err := store.TakeOAuthCode(r.Context(), h.pool, codeHash(req.code))
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
	hash := sha256.Sum256([]byte(req.codeVerifier))
	computed := base64.RawURLEncoding.EncodeToString(hash[:])
	if computed != ac.CodeChallenge {
		oauthError(w, "invalid_grant", "code_verifier mismatch")
		return
	}

	// code↔client binding (S2/W03-2; OAuth 2.1 §4.1.3 MUST: "the code MUST
	// have been issued to the requesting client"). For public clients this
	// equality is the ONLY client binding there is; without it client B can
	// redeem client A's code. Since S2 makes client_id mandatory at
	// /authorize, every code row carries one — an empty ac.ClientID can
	// only be a pre-S2 leftover row and fails closed.
	if ac.ClientID == "" || req.client != ac.ClientID {
		oauthError(w, "invalid_grant", "code was not issued to this client")
		return
	}

	// redirect_uri rebind (S5/W03-6; RFC 6749 §4.1.3): a redirect_uri
	// presented at /token must equal the one the code was issued for. An
	// ABSENT parameter is accepted — OAuth 2.1 drops the token-side
	// requirement in favour of PKCE (which S2/S5 enforce), so requiring it
	// would break spec-current clients for no security gain; the check bites
	// exactly the historic code-injection shape it exists for (a presented
	// but different URI).
	if req.redirectURI != "" && req.redirectURI != ac.RedirectURI {
		oauthError(w, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}

	// resource consistency (S5/W03-6; RFC 8707 §2.2): a resource presented
	// at /token must be OUR canonical MCP resource — same single-resource
	// rule as at /authorize. (The code row's stored value is already
	// validated-or-empty, so equality against the canonical value covers
	// both the rebind and the fresh-parameter case.)
	if req.resource != "" && req.resource != h.canonicalOrHostIssuer(r)+"/mcp" {
		oauthError(w, "invalid_target", "unknown resource")
		return
	}

	// Issue tokens — an OPAQUE, revocable, key-bound access token (S3/W03-3;
	// E-03a decided ja) plus its rotating refresh token (S4/W03-4): the raw
	// api key no longer circulates through the OAuth client's token storage.
	// The rows carry the INV-A single-key selector from the code; resolution
	// at /mcp materialises through the same ctx_auth_by_id gates as the
	// raw-key path. audiences always contains the ctx-MCP resource (design
	// 03 §4: E2 × RFC-8707 — the membership CHECK at /mcp lands in S5).
	pair, err := store.MintTokenPair(r.Context(), h.pool, store.OAuthToken{
		APIKeyID:    ac.APIKeyID,
		PrincipalID: ac.PrincipalID,
		ClientID:    ac.ClientID,
		Audiences:   []string{h.canonicalOrHostIssuer(r) + "/mcp"},
		IssuedVia:   "oauth",
	}, accessTokenTTL(), refreshTokenTTL())
	if err != nil {
		slog.Error("oauth: mint token pair", "error", err)
		oauthError(w, "invalid_grant", "code expired or not found")
		return
	}
	writeTokenPair(w, pair)
}

// writeTokenPair emits the RFC 6749 §5.1 success response for a fresh or
// rotated pair. expires_in carries the pair's nominal access TTL (a
// rotation near the family cap shortens only the refresh lifetime, never
// the access TTL).
func writeTokenPair(w http.ResponseWriter, pair *store.TokenPair) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  pair.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    int(pair.AccessTTL / time.Second),
		"refresh_token": pair.RefreshToken,
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
