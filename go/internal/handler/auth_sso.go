// External-login flow handlers (OAuth L5, design/04 §4.2/§4.3, wave W6):
//
//	GET /auth/login/{provider}    → 302 to the IdP authorize endpoint
//	GET /auth/callback/{provider} → state consume, code exchange, identity
//	                                verify, (issuer,subject)→principal_id
//
// Mounted in the public route region (server.go — these ARE the auth flow).
// Security posture, all fail-closed:
//
//   - Provider allowlist: only active context_oauth_providers rows resolve;
//     a multi-tenant-issuer row without its mandatory allowed_claim filter is
//     treated as INACTIVE (config honesty, §4.1 F3).
//   - Double-UUID state (F1): the context_sso_states row id lives ONLY in an
//     httpOnly cookie (the consume key); the OAuth `state` URL param is a
//     SECOND uuid compared against the sealed state_data afterwards.
//   - Mix-up defense (F2, OAuth 2.1 §4.15): the provider is resolved
//     EXCLUSIVELY from state_data.provider_slug; a diverging URL {provider}
//     is a hard reject. RFC 9207 `iss` is checked when present (§4.3.5).
//   - PKCE + nonce are OIDC-only (F6): a classic GitHub OAuth app has no ID
//     token and ignores code_challenge — its replay protection is state
//     single-use + one-time code.
//   - E4b admin-invite: an unknown verified (issuer,subject) is a 403, never
//     an auto-created principal and never an email-based auto-link.
//   - Every callback reject is ONE generic message — no oracle for which
//     check failed. Secrets/verifiers/nonces never reach logs or responses.
//   - Per-IP rate limit on both routes (F7): process-local fixed window,
//     RemoteAddr-keyed; X-Forwarded-For is honored ONLY when the direct peer
//     is the configured CTX_TRUSTED_PROXY.
//   - Upstream fetches (discovery/JWKS/userinfo/token) run over an
//     ssrfguard-hardened client (dial-time deny-list) with timeout + body cap.

package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/oidc"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/ssrfguard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// ssoStateCookie holds the context_sso_states ROW ID (the consume key) —
	// never the OAuth `state` param (double-UUID, design/04 §3.3).
	ssoStateCookie = "ctx_sso_state"
	// ssoStateTTL is the login round-trip budget (E-TTL: 1h).
	ssoStateTTL = time.Hour
	// ssoMaxTokenBody caps the token-endpoint response (fetch discipline,
	// mirrors the internal/oidc cap).
	ssoMaxTokenBody = 1 << 20
	// ssoRateLimit / ssoRateWindow bound unauthenticated resource
	// amplification per client IP (F7): every login mints a DB state row.
	ssoRateLimit  = 30
	ssoRateWindow = time.Minute
	// EnvTrustedProxy (the reverse-proxy hop whose X-Forwarded-For is
	// trusted for rate-limit keying) is declared once in
	// oauth_register_guard.go — both flows share the same trusted-proxy
	// contract.
)

// ssoRejectMsg is THE generic callback reject — CSRF, replay, expiry, state
// mismatch, provider mismatch and verification failures all read identically
// (design/04 §4.3: no oracle for which check failed).
const ssoRejectMsg = "login attempt is invalid or has expired"

// SSOHandler serves the external-login consumer flow. All fields are set by
// NewSSOHandler; tests construct the struct directly to inject a plain HTTP
// client (httptest servers live on loopback, which the ssrfguard default
// refuses by design) and a fixed sealbox.
type SSOHandler struct {
	pool *pgxpool.Pool
	// issuer is the canonical public origin (CTX_CANONICAL_ISSUER, no
	// trailing slash) — the same source OAuthHandler uses (S2). It is the
	// fallback base for redirect_uri when the provider row has no
	// redirect_base. Deliberately NEVER derived from r.Host: a
	// client-influenced redirect_uri would have to be registered at the IdP
	// to work and could be abused to probe registration.
	issuer string
	// trustedProxy is the peer IP whose X-Forwarded-For is believed.
	trustedProxy string
	// openBox yields the process sealbox (client_secret unseal). Lazy like
	// ManageHandler.sealboxOrNil; tests inject a fixed box.
	openBox func() (*sealbox.Box, error)
	// client performs token exchange + GitHub userinfo calls and feeds the
	// discovery/JWKS cache. Default: ssrfguard-guarded dialer + timeout.
	client *http.Client
	// cache is the shared discovery/JWKS TTL cache (per-URL buckets).
	cache *oidc.Cache
	// limiter is the per-IP fixed-window limiter for both routes.
	limiter *ipLimiter
	// session issues the web session at the callback end (R5, 05 §4.5) —
	// the same cookie/mint helpers the key-entry login uses (R3).
	session *SessionHandler
}

// NewSSOHandler wires the production SSO flow handler: ssrfguard-hardened
// HTTP client (dial-time deny-list against discovery/JWKS/token/userinfo
// targets resolving into private ranges), shared OIDC cache, env-derived
// canonical issuer and trusted proxy.
func NewSSOHandler(pool *pgxpool.Pool) *SSOHandler {
	client := &http.Client{
		Timeout: oidc.DefaultFetchTimeout,
		Transport: &http.Transport{
			Proxy:       http.ProxyFromEnvironment,
			DialContext: ssrfguard.GuardedDialer().DialContext,
		},
	}
	return &SSOHandler{
		pool:         pool,
		issuer:       strings.TrimRight(strings.TrimSpace(os.Getenv(EnvCanonicalIssuer)), "/"),
		trustedProxy: strings.TrimSpace(os.Getenv(EnvTrustedProxy)),
		openBox:      sealbox.FromEnv,
		client:       client,
		cache:        oidc.NewCache(oidc.Options{Client: client}),
		limiter:      newIPLimiter(ssoRateLimit, ssoRateWindow),
		session:      NewSessionHandler(pool),
	}
}

// HandleLogin implements GET /auth/login/{provider} (design/04 §4.2).
func (h *SSOHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(clientIP(r, h.trustedProxy)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "rate limit exceeded"})
		return
	}

	// §4.2.1 — allowlist resolve, fail-closed 404 (unknown, inactive and
	// misconfigured multi-tenant-issuer rows are indistinguishable outward).
	p := h.activeProvider(r.Context(), chi.URLParam(r, "provider"))
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "unknown provider"})
		return
	}
	prov, err := h.buildProvider(p)
	if err != nil {
		slog.Error("sso: provider construction failed", "provider", p.Slug, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "unknown provider"})
		return
	}
	redirectURI, ok := h.redirectURI(p)
	if !ok {
		// Config gap, not a caller error: neither redirect_base nor
		// CTX_CANONICAL_ISSUER is set — there is no trustworthy base for the
		// redirect_uri (r.Host would be client-influenced, see field doc).
		slog.Error("sso: no redirect base — set provider redirect_base or CTX_CANONICAL_ISSUER", "provider", p.Slug)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "login unavailable"})
		return
	}

	// §4.2.2 — endpoints via the provider abstraction (OIDC: cached
	// discovery with the doc.issuer==provider.issuer match inside the lib).
	authURL, _, _, _, err := prov.Endpoints(r.Context())
	if err != nil {
		slog.Warn("sso: endpoint resolution failed", "provider", p.Slug, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": "login unavailable"})
		return
	}

	// §4.2.3 — state + nonce are TWO distinct UUIDs; PKCE is OIDC-only (F6).
	state := uuid.NewString()
	nonce := uuid.NewString()
	verifier, challenge := "", ""
	if p.Type == "oidc" {
		verifier, challenge, err = generatePKCE()
		if err != nil {
			slog.Error("sso: pkce generation failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
			return
		}
	}

	// §4.2.4 — sealed server-side state; the row id goes ONLY into the
	// cookie, never into any URL. return_to is filtered against open
	// redirects (F5) BEFORE it is sealed.
	rowID, err := store.StoreSSOState(r.Context(), h.pool, store.SSOPurposeLogin, store.SSOStateData{
		ProviderSlug: p.Slug,
		State:        state,
		PKCEVerifier: verifier,
		Nonce:        nonce,
		ReturnTo:     safeReturnTo(r.URL.Query().Get("return_to")),
	}, ssoStateTTL)
	if err != nil {
		slog.Error("sso: state store failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     ssoStateCookie,
		Value:    rowID,
		Path:     "/auth",
		MaxAge:   int(ssoStateTTL / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	// §4.2.5 — the `state` URL param carries state_data.state, NOT the
	// cookie row id.
	http.Redirect(w, r, buildAuthorizeURL(authURL, p, redirectURI, challenge, state, nonce), http.StatusFound)
}

// HandleCallback implements GET /auth/callback/{provider} (design/04 §4.3).
// Every reject is the ONE generic ssoRejectMsg — no oracle.
func (h *SSOHandler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(clientIP(r, h.trustedProxy)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "rate limit exceeded"})
		return
	}
	ctx := r.Context()

	// §4.3.1 — read + immediately clear the state cookie (whatever happens
	// below, the browser never re-presents this consume key), then consume
	// the row atomically (single-use DELETE … RETURNING). A miss covers
	// CSRF, replay, cross-use and expiry uniformly.
	cookie, cookieErr := r.Cookie(ssoStateCookie)
	http.SetCookie(w, &http.Cookie{
		Name:     ssoStateCookie,
		Value:    "",
		Path:     "/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	if cookieErr != nil || cookie.Value == "" {
		h.reject(w, r, "missing state cookie")
		return
	}
	data, err := store.ConsumeSSOState(ctx, h.pool, cookie.Value, store.SSOPurposeLogin)
	if err != nil {
		slog.Error("sso: state consume failed", "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	if data == nil {
		h.reject(w, r, "state miss (replay/expiry/unknown)")
		return
	}

	// §4.3.2 — the URL `state` param against the SEALED state: two distinct
	// values (double-UUID), not a value against itself.
	q := r.URL.Query()
	if q.Get("state") == "" || q.Get("state") != data.State {
		h.reject(w, r, "state param mismatch")
		return
	}

	// §4.3.3 — mix-up defense (F2): the provider comes EXCLUSIVELY from the
	// sealed state; the URL {provider} is cosmetic and must agree.
	if chi.URLParam(r, "provider") != data.ProviderSlug {
		h.reject(w, r, "url provider diverges from sealed state")
		return
	}
	// Re-load the row and re-run the SAME activity checks as at login — a
	// provider deactivated mid-flight must not complete a login.
	p := h.activeProvider(ctx, data.ProviderSlug)
	if p == nil {
		h.reject(w, r, "provider no longer resolvable/active")
		return
	}

	// §4.3.4 — IdP error return (RFC 6749 §4.1.2.1): structured, NO exchange.
	if idpErr := q.Get("error"); idpErr != "" {
		slog.Info("sso: idp returned error", "provider", p.Slug, "idp_error", truncateString(idpErr, 64))
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": "identity provider reported an error"})
		return
	}
	code := q.Get("code")
	if code == "" {
		h.reject(w, r, "missing code")
		return
	}

	// §4.3.5 — RFC 9207: when the IdP echoes `iss`, it MUST match the
	// configured issuer. Absent param is non-fatal (§4.3.3 carries).
	if iss := q.Get("iss"); iss != "" && iss != p.Issuer {
		h.reject(w, r, "iss param mismatch")
		return
	}

	prov, err := h.buildProvider(p)
	if err != nil {
		slog.Error("sso: provider construction failed", "provider", p.Slug, "error", err, "request_id", RequestIDFromContext(ctx))
		h.reject(w, r, "provider construction")
		return
	}
	_, tokenURL, _, _, err := prov.Endpoints(ctx)
	if err != nil {
		slog.Warn("sso: endpoint resolution failed", "provider", p.Slug, "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": "login unavailable"})
		return
	}
	redirectURI, ok := h.redirectURI(p)
	if !ok {
		slog.Error("sso: no redirect base — set provider redirect_base or CTX_CANONICAL_ISSUER", "provider", p.Slug)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "login unavailable"})
		return
	}

	// §4.3.6 — client_secret unseal, ONLY for confidential clients
	// (token_auth != 'none'; public/native clients exchange PKCE-only).
	clientSecret := ""
	if p.TokenAuth != "none" {
		box, boxErr := h.openBox()
		if boxErr != nil {
			slog.Error("sso: sealbox unavailable", "provider", p.Slug, "error", boxErr)
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "login unavailable"})
			return
		}
		secret, resErr := store.ResolveSecret(ctx, h.pool, box, store.OAuthProviderSecretName(p.Slug), store.GlobalScope)
		if resErr != nil {
			// ResolveSecret's error contract carries no secret material.
			slog.Error("sso: client_secret resolution failed", "provider", p.Slug, "error", resErr)
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "login unavailable"})
			return
		}
		clientSecret = string(secret)
	}

	// §4.3.7 — token exchange (code_verifier only when the login minted one,
	// i.e. OIDC — F6).
	tokens, err := h.exchangeCode(ctx, tokenURL, code, redirectURI, p, clientSecret, data.PKCEVerifier)
	if err != nil {
		slog.Warn("sso: token exchange failed", "provider", p.Slug, "error", err, "request_id", RequestIDFromContext(ctx))
		h.reject(w, r, "token exchange")
		return
	}

	// §4.3.8 — identity verification inside the provider abstraction (OIDC:
	// discovery-issuer match, JWKS verify incl. alg/iss/aud/azp/exp/nonce +
	// allowed_claim; GitHub: userinfo with verified-email gating).
	ident, err := prov.Identity(ctx, tokens, data.Nonce)
	if err != nil {
		slog.Warn("sso: identity verification failed", "provider", p.Slug, "error", err, "request_id", RequestIDFromContext(ctx))
		h.reject(w, r, "identity verification")
		return
	}

	// §4.3.9 — (issuer,subject) → principal_id under INV-C. email refresh is
	// double-gated on the provider's verified flag (the oidc library already
	// blanks unverified addresses; belt and suspenders here).
	verifiedEmail := ""
	if ident.EmailVerified {
		verifiedEmail = ident.Email
	}
	principalID, found, err := store.TouchExternalIdentityLogin(ctx, h.pool, ident.Issuer, ident.Subject, verifiedEmail, ident.DisplayName)
	if err != nil {
		slog.Error("sso: identity mapping failed", "provider", p.Slug, "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	if !found {
		// E4b admin-invite (DECISIONS.md): no auto-create, no email link. A
		// verified-but-unlinked identity gets a neutral 403 — INV-B, login
		// success is never authorization.
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false,
			"error":   "identity not linked — ask an operator to invite or link this account",
		})
		return
	}

	// §4.3.10 / 05 §4.5 (R5) — session issuance (extracted for the cyclop
	// budget; the callback above stays the validation spine).
	h.issueWebSession(w, r, p.Slug, principalID, data.ReturnTo)
}

// issueWebSession ends a successful SSO callback in a real web session (R5,
// 05 §4.5): the identity is established; authorization is the principal's
// KEY (INV-B — SSO never grants access). A key-less principal answers
// fail-closed: the session row structurally requires a key (INV-A), so
// "logged in but sees nothing" materialises as no-session-at-all plus an
// honest operator hint. The key choice is the most recently used active key
// (PickPrincipalKey); the R6 selector switches tenants afterwards. The
// inactive-key shape is double-gated: the pick predicate AND the full
// AuthenticateByID gate set (key active / tenant status / principal
// is_active) both stand before anything is minted.
func (h *SSOHandler) issueWebSession(w http.ResponseWriter, r *http.Request, slug, principalID, returnTo string) {
	ctx := r.Context()
	apiKeyID, hasKey, err := store.PickPrincipalKey(ctx, h.pool, principalID)
	if err != nil {
		slog.Error("sso: pick principal key", "provider", slug, "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	noAccess := func() {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false,
			"error":   "no access yet — ask an operator to mint an api key for this account",
		})
	}
	if !hasKey {
		noAccess()
		return
	}
	result, err := auth.AuthenticateByID(ctx, h.pool, apiKeyID)
	if err != nil || !result.IsValid {
		slog.Warn("sso: picked key fails the auth gates", "provider", slug, "request_id", RequestIDFromContext(ctx))
		noAccess()
		return
	}

	base := h.session.audienceBase(r)
	pair, err := store.MintTokenPair(ctx, h.pool, store.OAuthToken{
		APIKeyID:    result.ApiKeyID,
		PrincipalID: result.PrincipalID,
		ClientID:    "", // web session, no OAuth client (same shape as key login)
		Audiences:   []string{base + "/mcp", base + "/web"},
		IssuedVia:   "login",
	}, accessTokenTTL(), refreshTokenTTL())
	if err != nil {
		slog.Error("sso: mint login pair", "provider", slug, "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	csrfBytes := make([]byte, 32)
	if _, err := rand.Read(csrfBytes); err != nil {
		slog.Error("sso: csrf rand", "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	sessionID, err := store.CreateWebSession(ctx, h.pool,
		pair.AccessToken, result.PrincipalID, hex.EncodeToString(csrfBytes), r.UserAgent(), clientIP(r, h.trustedProxy))
	if err != nil {
		slog.Error("sso: create overlay", "provider", slug, "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	h.session.setSessionCookies(w, sessionID, pair.RefreshToken)

	// Browser flow: land the person where they wanted to go (return_to was
	// open-redirect-filtered at login, "" folds to "/"); the SPA picks the
	// csrf token up from whoami.
	if returnTo == "" {
		returnTo = "/"
	}
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// reject writes the ONE generic callback reject and logs the real reason
// server-side (never token/state/nonce/secret values, only the branch name).
func (h *SSOHandler) reject(w http.ResponseWriter, r *http.Request, reason string) {
	slog.Info("sso: callback rejected", "reason", reason, "request_id", RequestIDFromContext(r.Context()))
	writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": ssoRejectMsg})
}

// activeProvider resolves a provider slug fail-closed: unknown, inactive and
// "multi-tenant issuer without allowed_claim" (§4.1 F3 config honesty) all
// yield nil — outward indistinguishable. DB errors also yield nil (fail
// closed); the log carries the truth.
func (h *SSOHandler) activeProvider(ctx context.Context, slug string) *store.OAuthProvider {
	if slug == "" {
		return nil
	}
	p, found, err := store.GetOAuthProviderBySlug(ctx, h.pool, slug)
	if err != nil {
		slog.Error("sso: provider lookup failed", "error", err)
		return nil
	}
	if !found || !p.Active {
		return nil
	}
	if !p.SingleTenantIssuer && p.AllowedClaim == nil {
		return nil
	}
	return p
}

// buildProvider maps a config row onto the L2 provider abstraction. For
// type='github' the userinfo_url column carries the API BASE (e.g.
// https://api.github.com — Endpoints appends /user), matching the migration
// 100 "github fest" semantics; auth_url/token_url override the github.com
// defaults (GitHub Enterprise, httptest).
func (h *SSOHandler) buildProvider(p *store.OAuthProvider) (oidc.Provider, error) {
	switch p.Type {
	case "oidc":
		return oidc.NewOIDC(h.cache, oidc.OIDCConfig{
			Issuer:       p.Issuer,
			ClientID:     p.ClientID,
			IDTokenAlgs:  p.IDTokenAlgs,
			AllowedClaim: p.AllowedClaim,
		})
	case "github":
		cfg := oidc.GitHubConfig{Issuer: p.Issuer, Client: h.client}
		if p.AuthURL != nil {
			cfg.AuthURL = *p.AuthURL
		}
		if p.TokenURL != nil {
			cfg.TokenURL = *p.TokenURL
		}
		if p.UserinfoURL != nil {
			cfg.APIBase = *p.UserinfoURL
		}
		return oidc.NewGitHub(cfg), nil
	default:
		return nil, fmt.Errorf("sso: unsupported provider type %q", p.Type)
	}
}

// redirectURI derives the callback URL registered at the IdP:
// <redirect_base|canonical issuer>/auth/callback/<slug> (§3.2). ok=false when
// neither base exists — fail-closed, never r.Host (client-influenced).
func (h *SSOHandler) redirectURI(p *store.OAuthProvider) (string, bool) {
	base := ""
	if p.RedirectBase != nil {
		base = strings.TrimRight(strings.TrimSpace(*p.RedirectBase), "/")
	}
	if base == "" {
		base = h.issuer
	}
	if base == "" {
		return "", false
	}
	return base + "/auth/callback/" + p.Slug, true
}

// buildAuthorizeURL assembles the §4.2.5 authorize redirect (port of
// serviceportal buildAuthorizeURL, sso_handlers.go:688). code_challenge and
// nonce are attached for OIDC only (F6): a classic GitHub OAuth app ignores
// PKCE and has no ID token to carry a nonce.
func buildAuthorizeURL(authURL string, p *store.OAuthProvider, redirectURI, codeChallenge, state, nonce string) string {
	q := url.Values{}
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(p.Scopes, " "))
	q.Set("state", state)
	if p.Type == "oidc" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
		q.Set("nonce", nonce)
	}
	sep := "?"
	if strings.Contains(authURL, "?") {
		sep = "&"
	}
	return authURL + sep + q.Encode()
}

// generatePKCE creates a PKCE verifier/challenge pair (port of serviceportal
// generatePKCE, sso_handlers.go:673): 32 random bytes → base64url verifier
// (43 chars), challenge = base64url(SHA-256(verifier)), method S256.
func generatePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("sso: pkce random: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// exchangeCode POSTs the authorization code to the token endpoint (§4.3.7,
// port of serviceportal exchangeCodeForTokens, sso_handlers.go:702).
// token_auth selects the client authentication: client_secret_post (form
// field), client_secret_basic (RFC 6749 §2.3.1: id and secret are
// form-urlencoded BEFORE basic auth) or none (public client, PKCE-only).
//
// at_hash (OIDC Core §3.2.2.9) is deliberately NOT validated: this consumer
// never uses the access_token on the OIDC branch — identity comes from the
// verified ID token — so an at_hash check would protect nothing here.
// Revisit if an OIDC userinfo call over the access_token ever lands
// (L2 review point).
func (h *SSOHandler) exchangeCode(ctx context.Context, tokenURL, code, redirectURI string, p *store.OAuthProvider, clientSecret, codeVerifier string) (oidc.Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", p.ClientID)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	if p.TokenAuth == "client_secret_post" {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode())) //nolint:gosec // G704: tokenURL comes from the admin-allowlisted provider row / issuer-matched discovery doc (INV-C), and SSRF is enforced dial-time by the ssrfguard transport on h.client — same pattern as forge/camo.
	if err != nil {
		return oidc.Tokens{}, fmt.Errorf("sso: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// GitHub's token endpoint answers form-encoded unless JSON is requested;
	// spec-clean OIDC IdPs answer JSON either way.
	req.Header.Set("Accept", "application/json")
	if p.TokenAuth == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(clientSecret))
	}

	resp, err := h.client.Do(req) //nolint:gosec // G704: see request construction above — allowlisted target, dial-time ssrfguard.
	if err != nil {
		return oidc.Tokens{}, fmt.Errorf("sso: token exchange: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, ssoMaxTokenBody+1))
	if err != nil {
		return oidc.Tokens{}, fmt.Errorf("sso: read token response: %w", err)
	}
	if len(body) > ssoMaxTokenBody {
		return oidc.Tokens{}, fmt.Errorf("sso: token response exceeds %d bytes", ssoMaxTokenBody)
	}
	if resp.StatusCode != http.StatusOK {
		// The error detail stays in server logs only; the body is capped so a
		// hostile IdP cannot flood them.
		return oidc.Tokens{}, fmt.Errorf("sso: token endpoint returned %d: %s", resp.StatusCode, truncateString(string(body), 256))
	}
	var tokens oidc.Tokens
	if err := json.Unmarshal(body, &tokens); err != nil {
		return oidc.Tokens{}, fmt.Errorf("sso: decode token response: %w", err)
	}
	return tokens, nil
}

// truncateString shortens s for log lines.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// returnToRe implements the F5 open-redirect filter: exactly one leading
// slash followed by anything but '/' or '\' — rejects '//evil.example',
// '/\evil', absolute and protocol-relative URLs.
var returnToRe = regexp.MustCompile(`^/[^/\\]`)

// safeReturnTo filters a caller-supplied post-login path (design/04 §4.2.4 /
// §5 F5). Anything outside the allowed shape collapses to "/".
func safeReturnTo(raw string) string {
	if returnToRe.MatchString(raw) {
		return raw
	}
	return "/"
}

// clientIP derives the rate-limit key: the direct peer's IP, or — ONLY when
// the direct peer IS the configured trusted proxy — the rightmost non-empty
// X-Forwarded-For entry (the hop the trusted proxy itself appended; entries
// further left are client-forgeable).
func clientIP(r *http.Request, trustedProxy string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if trustedProxy == "" || host != trustedProxy {
		return host
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(parts[i]); p != "" {
			return p
		}
	}
	return host
}

// ipLimiterMaxBuckets caps the limiter map — the flow is unauthenticated, so
// the keyspace is attacker-controlled. When even pruning expired windows
// cannot make room, NEW ips are denied (fail-closed: bounded memory beats
// availability for an auth flow under active flooding).
const ipLimiterMaxBuckets = 8192

// ipLimiter is a process-local fixed-window per-IP rate limiter (F7). ctx has
// no unauthenticated limiter to reuse (CheckRateLimit is api-key-based);
// deliberately small and self-contained — if a parallel wave grows a shared
// limiter, the merge happens at the lead.
type ipLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	buckets map[string]*ipWindow
}

type ipWindow struct {
	start time.Time
	count int
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{
		limit:   limit,
		window:  window,
		now:     time.Now,
		buckets: make(map[string]*ipWindow),
	}
}

// allow reports whether one more request from ip fits the current window.
func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[ip]
	if b != nil && now.Sub(b.start) < l.window {
		b.count++
		return b.count <= l.limit
	}
	if len(l.buckets) >= ipLimiterMaxBuckets {
		for k, v := range l.buckets {
			if now.Sub(v.start) >= l.window {
				delete(l.buckets, k)
			}
		}
		if len(l.buckets) >= ipLimiterMaxBuckets {
			return false
		}
	}
	l.buckets[ip] = &ipWindow{start: now, count: 1}
	return true
}
