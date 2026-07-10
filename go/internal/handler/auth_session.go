// Web-session lifecycle (OAuth R3, design 05 §4.3/§4.4):
//
//	POST /auth/login   → key → login token pair + overlay row + cookies
//	POST /auth/refresh → rotate the refresh cookie, repoint the overlay
//	POST /auth/logout  → revoke the token family, delete the overlay
//
// Mounted in the public route region next to the SSO routes (these ARE the
// auth flow). The session cookie (ctx_session) carries the overlay row id
// resolved by the Auth middleware since R2; the refresh cookie (ctx_refresh)
// carries the ctxr_ plaintext, path-scoped to /auth so it never rides data
// requests. CSRF: the login/refresh/logout routes themselves need no
// synchronizer token — login PRESENTS a credential (nothing ambient),
// refresh needs the SameSite=Strict refresh cookie a cross-site request
// cannot send, logout is idempotent and SameSite=Lax-shielded; the
// synchronizer gate protects the ambient /api/* mutations (middleware.go).
package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// refreshCookieName carries the ctxr_ refresh token plaintext, path-scoped
// to /auth (it exists ONLY for POST /auth/refresh — data requests never see
// it) and SameSite=Strict (a cross-site request cannot trigger a rotation).
const refreshCookieName = "ctx_refresh"

// sessionRateLimit / sessionRateWindow bound the unauthenticated login
// endpoint per IP — the same fixed-window shape as the SSO routes (F7);
// login is the key-guessing surface, so it shares their conservative rate.
var (
	sessionRateLimit  = 30
	sessionRateWindow = ssoRateWindow
)

// SessionHandler serves the web-session lifecycle routes.
type SessionHandler struct {
	pool         *pgxpool.Pool
	issuer       string // CTX_CANONICAL_ISSUER (empty → r.Host fallback for audiences)
	trustedProxy string
	limiter      *ipLimiter
}

// NewSessionHandler wires the session lifecycle handler.
func NewSessionHandler(pool *pgxpool.Pool) *SessionHandler {
	return &SessionHandler{
		pool:         pool,
		issuer:       strings.TrimRight(strings.TrimSpace(os.Getenv(EnvCanonicalIssuer)), "/"),
		trustedProxy: strings.TrimSpace(os.Getenv(EnvTrustedProxy)),
		limiter:      newIPLimiter(sessionRateLimit, sessionRateWindow),
	}
}

// secureCookies decides the Secure flag from configuration, not from header
// sniffing: a canonical issuer on https means production-behind-TLS; an
// unset/http issuer is the dev shape. Deterministic beats a spoofable
// X-Forwarded-Proto.
func (h *SessionHandler) secureCookies() bool {
	return strings.HasPrefix(h.issuer, "https://")
}

// audienceBase returns the issuer for the login token's audiences —
// canonical when configured, r.Host otherwise (the same fallback the OAuth
// endpoints use pre-S2-config).
func (h *SessionHandler) audienceBase(r *http.Request) string {
	if h.issuer != "" {
		return h.issuer
	}
	return "https://" + r.Host
}

// setSessionCookies writes both lifecycle cookies. MaxAge tracks the
// refresh TTL — the session cookie outlives the 1h access token because
// /auth/refresh rotates underneath it; the 90d family cap (S4) bounds the
// total regardless of rolling refreshes.
func (h *SessionHandler) setSessionCookies(w http.ResponseWriter, sessionID, refreshToken string) {
	maxAge := int(refreshTokenTTL().Seconds())
	// #nosec G124 -- HttpOnly+SameSite are set; Secure is DELIBERATELY
	// config-derived (secureCookies: https canonical issuer → true), not a
	// hard literal — a hard true would break plain-http dev/loopback logins
	// while production behind TLS always derives true (design 05 §4.3,
	// cookies.go-Destillat "Secure-in-Prod").
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: sessionID,
		Path: "/", HttpOnly: true, Secure: h.secureCookies(),
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	})
	// #nosec G124 -- same rationale; additionally Path=/auth + SameSite=Strict.
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookieName, Value: refreshToken,
		Path: "/auth", HttpOnly: true, Secure: h.secureCookies(),
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

// clearSessionCookies expires both cookies (logout / dead family).
func (h *SessionHandler) clearSessionCookies(w http.ResponseWriter) {
	for _, c := range []struct{ name, path string }{
		{sessionCookieName, "/"},
		{refreshCookieName, "/auth"},
	} {
		// #nosec G124 -- expiry cookie (MaxAge -1, empty value); Secure is
		// config-derived, see setSessionCookies.
		http.SetCookie(w, &http.Cookie{
			Name: c.name, Value: "", Path: c.path, HttpOnly: true,
			Secure: h.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
	}
}

// sessionError answers one uniform JSON error — no oracle between unknown
// key, dead family, missing cookie (same discipline as the SSO rejects).
func sessionError(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]any{"success": false, "error": "authentication failed"})
}

// HandleLogin implements POST /auth/login (05 §4.3 login source (a): key
// entry): validate the key, mint a login token pair through the SAME 03
// mint path as /token (issued_via='login', audiences carry BOTH the MCP and
// web resource — E2: a login token is structurally MCP-capable), lay the
// overlay row and set the cookies. The response carries the csrf_token so
// the SPA can start mutating without a whoami round trip.
func (h *SessionHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(clientIP(r, h.trustedProxy)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "rate limited"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.APIKey) == "" {
		sessionError(w, http.StatusBadRequest)
		return
	}

	result, err := auth.Authenticate(r.Context(), h.pool, auth.SanitizeKey(req.APIKey))
	if err != nil || !result.IsValid {
		sessionError(w, http.StatusUnauthorized)
		return
	}

	base := h.audienceBase(r)
	pair, err := store.MintTokenPair(r.Context(), h.pool, store.OAuthToken{
		APIKeyID:    result.ApiKeyID,
		PrincipalID: result.PrincipalID,
		ClientID:    "", // web login has no OAuth client; rotation binds NULL↔""
		Audiences:   []string{base + "/mcp", base + "/web"},
		IssuedVia:   "login",
	}, accessTokenTTL(), refreshTokenTTL())
	if err != nil {
		slog.Error("session: mint login pair", "error", err)
		sessionError(w, http.StatusInternalServerError)
		return
	}

	csrfBytes := make([]byte, 32)
	if _, err := rand.Read(csrfBytes); err != nil {
		slog.Error("session: csrf rand", "error", err)
		sessionError(w, http.StatusInternalServerError)
		return
	}
	csrfSecret := hex.EncodeToString(csrfBytes)

	sessionID, err := store.CreateWebSession(r.Context(), h.pool,
		pair.AccessToken, result.PrincipalID, csrfSecret, r.UserAgent(), clientIP(r, h.trustedProxy))
	if err != nil {
		slog.Error("session: create overlay", "error", err)
		sessionError(w, http.StatusInternalServerError)
		return
	}

	h.setSessionCookies(w, sessionID, pair.RefreshToken)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "csrf_token": csrfSecret})
}

// HandleRefresh implements POST /auth/refresh: rotate the refresh cookie
// through 03's RotateRefreshToken (client binding "" ↔ the login rows'
// NULL client_id; reuse detection and the family kill are 03's code — the
// LOOP gate "Refresh-Replay → Familien-Revoke" is exactly that path) and
// repoint the overlay onto the fresh access row. The repoint is family-
// bound in the predicate: a refresh of family A can never capture a
// session of family B. Any failure clears the cookies — the SPA re-logins.
func (h *SessionHandler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	refreshC, errR := r.Cookie(refreshCookieName)
	sessionC, errS := r.Cookie(sessionCookieName)
	if errR != nil || errS != nil || refreshC.Value == "" || sessionC.Value == "" {
		h.clearSessionCookies(w)
		sessionError(w, http.StatusUnauthorized)
		return
	}

	pair, outcome, err := store.RotateRefreshToken(r.Context(), h.pool, refreshC.Value, "",
		accessTokenTTL(), refreshTokenTTL(), refreshFamilyCap())
	if err != nil {
		slog.Error("session: rotate refresh", "error", err)
		h.clearSessionCookies(w)
		sessionError(w, http.StatusUnauthorized)
		return
	}
	if outcome != store.Rotated {
		if outcome == store.RotateReuse {
			slog.Warn("session: refresh reuse detected — family revoked")
		}
		h.clearSessionCookies(w)
		sessionError(w, http.StatusUnauthorized)
		return
	}

	ok, err := store.RepointWebSession(r.Context(), h.pool, sessionC.Value, pair.AccessToken)
	if err != nil || !ok {
		// Family mismatch or vanished overlay — fail closed, force re-login.
		if err != nil {
			slog.Error("session: repoint overlay", "error", err)
		}
		h.clearSessionCookies(w)
		sessionError(w, http.StatusUnauthorized)
		return
	}

	h.setSessionCookies(w, sessionC.Value, pair.RefreshToken)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// HandleLogout implements POST /auth/logout: revoke the whole token family
// and delete the overlay (one tx), then clear the cookies. Idempotent — a
// dead or malformed session logs out just as successfully.
func (h *SessionHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if err := store.DestroyWebSession(r.Context(), h.pool, c.Value); err != nil {
			slog.Error("session: destroy", "error", err)
			// Still clear the cookies — the client side of logout must win.
		}
	}
	h.clearSessionCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
