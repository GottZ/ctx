package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// validRequestID matches only hex characters and hyphens.
var validRequestID = regexp.MustCompile(`^[0-9a-fA-F-]+$`)

const maxRequestIDLen = 64

// SchedulerNotifier signals interactive demand for the lifetime of one HTTP
// request. InteractiveArrived marks the request entering the house and returns
// an idempotent done that MUST run at handler end to mark it leaving
// (design/05 §4.1, A5-W0). The process-wide dispatcher implements this directly
// — its demand herald is the successor of the old scheduler query-demand
// mirror. The single-method closure form is deliberate: it keeps the
// idempotent-done guarantee the herald primitive already pins (dispatch
// TestHeraldDoneIdempotent) and needs no caller-side start/end pairing state —
// the returned done IS the "InteractiveDone".
type SchedulerNotifier interface {
	InteractiveArrived() func()
}

// WithScheduler wraps an http.HandlerFunc to signal interactive demand for the
// duration of the request. The wrapper and every mount stay structurally
// unchanged across A5-W0 — only the injected object moves from the scheduler
// (old QueryStart/QueryEnd mirror) to the dispatcher (the demand herald).
func WithScheduler(sn SchedulerNotifier, next http.HandlerFunc) http.HandlerFunc {
	if sn == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		done := sn.InteractiveArrived()
		defer done()
		next(w, r)
	}
}

type contextKey string

const (
	requestIDKey contextKey = "request_id"
	authResultKey contextKey = "auth_result"
)

// RequestIDFromContext extracts the request ID from the context.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// AuthResultFromContext extracts the AuthResult from the context.
func AuthResultFromContext(ctx context.Context) *auth.AuthResult {
	if ar, ok := ctx.Value(authResultKey).(*auth.AuthResult); ok {
		return ar
	}
	return nil
}

// RequestTenantScope resolves the authenticated tenant scope from a request
// context, for config's per-tenant snapshot resolution (MT 06-C5). It is the
// cycle-free wrapper the config package cannot write itself — config must not
// import handler/auth — so main wires it via config.SetRequestScopeHook at boot.
//
// The scope is the requesting key's HomeScope: the tenant's OWN policy scope,
// the same string that already drives writeScope (context_store.go:93) and is
// consistent with the read-scope set Achse 02 feeds ctx_rrf. Per design §11.1
// the tenant identity IS the scope namespace, NOT the tenant UUID
// (AuthResult.TenantID): the per-tenant config lives in context_settings.scope,
// so the overlay keys on the scope string, and a UUID would match no settings
// row. An absent AuthResult (anonymous/health paths) or an empty HomeScope
// returns "", which makes SnapshotForRequest fall back to the base generation
// (fail-safe, §4.2); a reserved (_-prefixed) scope is rejected inside the store.
//
// TENANT-DECISION(06-C5-scope): tenantScope == ar.HomeScope (scope namespace).
// Alt: ar.TenantID (UUID). Reversible if Achse 01 ever splits a parallel
// tenant_id away from the scope dimension — then this wrapper and
// LoadSettingOverrides would key on the UUID (§11.1).
func RequestTenantScope(ctx context.Context) string {
	if ar := AuthResultFromContext(ctx); ar != nil {
		return ar.HomeScope
	}
	return ""
}

// RequestPrincipal resolves the dispatch admission principal from the
// authenticated request context (Vorhaben E MW4, design/03 §4.1.1). It is
// the cycle-free wrapper dispatch cannot write itself — dispatch is a leaf
// package and must not import handler/auth — so main wires it via
// dispatch.SetPrincipalHook at boot, BEFORE the scheduler goroutines spawn:
// unlike the HTTP-only request-scope hooks, Acquire also runs on detached
// background contexts. An absent AuthResult (detached scheduler ctx,
// anonymous paths) yields the zero Principal, which is structurally
// background — an interactive acquire on such a ctx runs into the B8
// downgrade. Emptiness stays pinned on ApiKeyID inside dispatch: a synthetic
// AuthResult with an empty key id (chat.go RunQuery, the S9 finding)
// downgrades too instead of forming an anonymous interactive bucket.
func RequestPrincipal(ctx context.Context) dispatch.Principal {
	ar := AuthResultFromContext(ctx)
	if ar == nil {
		return dispatch.Principal{}
	}
	return dispatch.Principal{
		ApiKeyID:    ar.ApiKeyID,
		TenantID:    ar.TenantID,
		HomeScope:   ar.HomeScope,
		PrincipalID: ar.PrincipalID,
	}
}

// isValidRequestID checks that a client-supplied request ID contains only
// hex characters (0-9, a-f, A-F) and hyphens, and is at most 64 characters.
func isValidRequestID(id string) bool {
	return len(id) > 0 && len(id) <= maxRequestIDLen && validRequestID.MatchString(id)
}

// RequestID assigns a unique ID to each request and adds it to the context and response header.
// Client-supplied IDs are accepted only if they contain hex characters and hyphens
// and are at most 64 characters long; otherwise a server-generated ID is used.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !isValidRequestID(id) {
			b := make([]byte, 8)
			if _, err := rand.Read(b); err != nil {
				slog.Error("failed to generate request ID", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			id = hex.EncodeToString(b)
		}

		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying ResponseWriter so http.NewResponseController can
// reach Flush/Hijack through this status-capturing wrapper. Without it the
// controller stops here and the query heartbeat's flushes are buffered to the
// end of the response (no streaming -> reverse-proxy read-timeout 504).
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Logger logs each HTTP request with method, path, status, and duration.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(rw, r)

		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFromContext(r.Context()),
		)
	})
}

// Recovery recovers from panics in HTTP handlers and returns a 500 error.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered",
					"error", rec,
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// credentialFromRequest extracts the RAW credential from the X-Context-Key
// header or an Authorization: Bearer token — deliberately WITHOUT SanitizeKey:
// the ctxt_/ctxr_ prefix of an opaque token (S3) must survive extraction so
// resolveCredential can branch on it, and it must do so header-agnostically
// (a ctxt_ token via X-Context-Key would otherwise be hex-stripped into the
// raw-key path — design 03 §4, RVW-Vollst-F6). Shared by the Auth middleware
// (connect-time) and both SSE in-stream re-auth paths (events.go,
// project_events.go) so all three read the credential identically.
func credentialFromRequest(r *http.Request) string {
	rawKey := r.Header.Get("X-Context-Key")
	if rawKey == "" {
		if bearer := r.Header.Get("Authorization"); strings.HasPrefix(bearer, "Bearer ") {
			rawKey = strings.TrimPrefix(bearer, "Bearer ")
		}
	}
	return strings.TrimSpace(rawKey)
}

// sessionCookieName is the httpOnly web-session cookie (design 05 §4.2 path
// 3, wave R2). Its value is the context_web_sessions row id; issuance (login)
// lands in R3 — until then only seeded rows can resolve.
const sessionCookieName = "ctx_session"

// requestCredential extracts the request's credential with the §4.2
// precedence: a header credential (X-Context-Key / Bearer) ALWAYS wins; only
// a request with no header credential at all falls through to the session
// cookie (isSession=true). The discrimination is an explicit flag, never a
// value shape — a session id is a dashed UUID that SanitizeKey would strip
// into a 32-hex, key-shaped string, so shape-sniffing could misroute it.
// Shared by the Auth middleware and both SSE re-auth seams: the streams
// capture (raw, isSession) at connect time and the tick re-resolves the
// same pair, so a cookie-carried stream dies with its session exactly like
// a token-carried stream dies with its token.
func requestCredential(r *http.Request) (raw string, isSession bool) {
	if raw = credentialFromRequest(r); raw != "" {
		return raw, false
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}

// resolveRequestCredential resolves either credential shape onto the ONE
// AuthResult gate: a session id runs overlay→token→ctx_auth_by_id (05 §4.2
// path 3), everything else takes the S3 resolveCredential branches. A
// session miss of ANY kind never falls back to the key path (fail-closed,
// same rule as the ctxt_ branch). csrfSecret is non-empty ONLY on a valid
// session resolve — the Auth middleware checks it against X-CSRF-Token on
// state-changing cookie requests (R3, 05 §4.4); header-credential paths
// carry no ambient authority and need no CSRF.
func resolveRequestCredential(ctx context.Context, pool *pgxpool.Pool, raw string, isSession bool) (*auth.AuthResult, string, error) {
	if !isSession {
		ar, err := resolveCredential(ctx, pool, raw)
		return ar, "", err
	}
	apiKeyID, csrfSecret, ok, err := store.ResolveWebSession(ctx, pool, raw)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return &auth.AuthResult{IsValid: false}, "", nil
	}
	ar, err := auth.AuthenticateByID(ctx, pool, apiKeyID)
	return ar, csrfSecret, err
}

// csrfTokenKey carries the session's csrf_secret through the request context
// (set by Auth on the cookie path) so whoami can hand it to the SPA — the
// synchronizer-token delivery channel (05 §4.4).
type csrfTokenContextKey struct{}

// CSRFTokenFromContext returns the session csrf token, or "" on header-
// credential requests.
func CSRFTokenFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(csrfTokenContextKey{}).(string); ok {
		return v
	}
	return ""
}

// stateChanging reports whether the method mutates (the CSRF-relevant set).
func stateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// resolveCredential is the ONE credential→AuthResult gate for all HTTP auth
// paths (design 03 §4, wave S3). Branching happens on the extracted value,
// BEFORE SanitizeKey, independent of the source header:
//
//   - ctxt_ (opaque access token, 099): token-store lookup → ctx_auth_by_id.
//     A miss NEVER falls back to the raw-key path (explicit fail-closed) —
//     unknown, expired and revoked tokens are all the same invalid result.
//   - ctxr_ (refresh token): explicitly rejected — a refresh token is not an
//     access credential; the SQL token_type='access' filter is only the
//     second belt.
//   - anything else: the raw-api-key path, byte-identical to the pre-S3
//     behaviour (SanitizeKey → ctx_auth). Raw keys are pure hex, so a
//     prefixed token can never be misclassified here and vice versa.
//
// Both token and key materialise through the SAME ctx_auth_by_id scope-build
// (095): a revoked key or deactivated principal kills its tokens instantly,
// with no separate token-revocation bookkeeping.
func resolveCredential(ctx context.Context, pool *pgxpool.Pool, raw string) (*auth.AuthResult, error) {
	switch {
	case strings.HasPrefix(raw, store.AccessTokenPrefix):
		// RFC 8707 audience membership (S5/W03-6): the token's audiences
		// must contain OUR canonical MCP resource. Enforceable only with a
		// configured canonical issuer (S2) — unset skips the gate (see
		// LookupAccessToken doc); raw api keys bypass it by design (the
		// documented E2 spec deviation, design 03 §4).
		requiredAudience := ""
		if issuer := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvCanonicalIssuer)), "/"); issuer != "" {
			requiredAudience = issuer + "/mcp"
		}
		apiKeyID, ok, err := store.LookupAccessToken(ctx, pool, raw, requiredAudience)
		if err != nil {
			return nil, err
		}
		if !ok {
			return &auth.AuthResult{IsValid: false}, nil
		}
		return auth.AuthenticateByID(ctx, pool, apiKeyID)
	case strings.HasPrefix(raw, store.RefreshTokenPrefix):
		return &auth.AuthResult{IsValid: false}, nil
	default:
		return auth.Authenticate(ctx, pool, auth.SanitizeKey(raw))
	}
}

// Auth creates middleware that authenticates requests via X-Context-Key header
// or Authorization: Bearer token — resolveCredential branches on opaque ctxt_
// tokens vs raw API keys (S3) — or, header-less, via the ctx_session cookie
// (R2: overlay→token→ctx_auth_by_id, same downstream AuthResult shape).
func Auth(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, isSession := requestCredential(r)
			result, csrfSecret, err := resolveRequestCredential(r.Context(), pool, raw, isSession)
			if err != nil {
				slog.Error("auth failed",
					"error", err,
					"request_id", RequestIDFromContext(r.Context()),
				)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "authentication error"})
				return
			}

			// CSRF synchronizer gate (R3, 05 §4.4) — cookie path ONLY: the
			// cookie is ambient, so every state-changing request must echo
			// the per-session secret in X-CSRF-Token (delivered via whoami).
			// Header-credential requests carry no ambient authority and pass
			// untouched (the MCP-Bearer regression gate). Comparison is
			// constant-time — the secret is high-entropy, but the check is
			// on the hot path and the guard costs nothing.
			if isSession && result.IsValid && stateChanging(r.Method) {
				if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrfSecret)) != 1 {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or invalid CSRF token"})
					return
				}
			}

			if !result.IsValid {
				// RFC 9728 §5.1 (S5/W03-5): the 401 points the client at
				// the protected-resource metadata AND names the scope to
				// request — MCP clients bootstrap their auth flow from
				// exactly this header.
				issuer := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvCanonicalIssuer)), "/")
				if issuer == "" {
					issuer = "https://" + r.Host
				}
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer resource_metadata=%q, scope="mcp"`,
						issuer+"/.well-known/oauth-protected-resource/mcp"))
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}

			ctx := context.WithValue(r.Context(), authResultKey, result)
			if csrfSecret != "" {
				ctx = context.WithValue(ctx, csrfTokenContextKey{}, csrfSecret)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireMember gates a route on ANY valid authenticated key (member tier and
// up). Mount AFTER Auth — Auth already rejects invalid/missing keys with 401,
// so in the production chain this is the explicit, DB-free, PROBEABLE in-mount
// gate that makes a member surface fail-closed the same structural way
// RequireAdmin does (design/03 §5.1: the gate lives in the SAME function as the
// routes, so the negative probe exercises exactly the chain production mounts).
// A missing or invalid AuthResult yields 401 — no member handler downstream
// ever runs without a caller identity to scope against (the K-T1 pairing: the
// gate admits, the handler scopes reads to the caller's visible scopes).
// Negative probe: TestTypesMemberGate_NoAuth401 was red against MountTypes with
// this Use() line removed (the nil AuthResult reached the handler and panicked
// into typeVisibleScopes — the fail-open trap made visible).
func RequireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ar := AuthResultFromContext(r.Context())
		if ar == nil || !ar.IsValid {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"success": false, "error": "unauthorized",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin gates a route on the admin tier (052). Mount AFTER Auth —
// it reads the AuthResult from the request context. Non-admin (or missing
// auth context) yields 403; the response shape matches the API envelope.
// Negative probe: TestRequireAdmin_NonAdmin403 / TestManageAdminGate_* were
// red against the ungated chain before this landed (G03, 2026-06-10).
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ar := AuthResultFromContext(r.Context())
		if ar == nil || !ar.IsValid || !ar.IsAdmin {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"success": false, "error": "admin key required",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdminOrTenantAdmin gates a route on EITHER the server-admin tier (M052)
// OR a tenant-admin (owner/admin role) of the caller's OWN tenant (MT T37b,
// 04-W5). Mount AFTER Auth. It only ADMITS the caller — it does NOT filter the
// payload; the route's handler MUST itself scope the response to the caller's
// tenant (the /api/llmlog handler applies the per-tenant api_key_id filter).
// Pairing the looser gate with the in-handler filter is the K-T1 invariant: a
// gate-without-filter would leak every tenant's telemetry. A member or an
// unauthenticated caller gets 403 (same body as RequireAdmin — no tier oracle).
func RequireAdminOrTenantAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ar := AuthResultFromContext(r.Context())
		admitted := ar != nil && (ar.IsServerAdmin() || ar.IsTenantAdminOf(ar.TenantID))
		if !admitted {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"success": false, "error": "admin key required",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders adds security-related response headers to every request.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// MaxBodySize limits the request body to maxBytes. Returns 413 if exceeded.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBodySizeStrict is MaxBodySize for handlers that own their error surface —
// third-party ones above all. It rejects a declared over-cap body with the
// house envelope BEFORE the handler runs, and still wraps the body afterwards.
//
// Rationale (Gap-C6-b): MaxBodySize only wraps, so the verdict surfaces as a
// MaxBytesError inside whoever reads the body. Our own handlers translate that
// into 413 + envelope (decodeIssueBody); the MCP SDK does not — it answers
// plain-text 400 "failed to read body" (go-sdk streamable.go:433-435). A caller
// then cannot tell an over-cap body from a malformed one.
//
// Mount AFTER Auth: the pre-check answers before any handler work, so mounting
// it first would let an anonymous caller confirm route and cap.
//
// Residual: a request without a declared Content-Length (chunked) still falls
// through to the wrapping guard — memory stays bounded, only the response shape
// is the handler's. Declared-length requests are the ones we can answer cleanly,
// and every MCP client sends one.
func MaxBodySizeStrict(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
					"success": false,
					"error":   fmt.Sprintf("request body exceeds %s cap", byteCap(maxBytes)),
				})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// byteCap renders a body cap the way the existing 413 messages spell it
// ("1 MB cap") without lying about caps that are not whole MB.
func byteCap(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
