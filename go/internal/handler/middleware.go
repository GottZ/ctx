package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// validRequestID matches only hex characters and hyphens.
var validRequestID = regexp.MustCompile(`^[0-9a-fA-F-]+$`)

const maxRequestIDLen = 64

// SchedulerNotifier is the interface the scheduler must implement for demand signaling.
type SchedulerNotifier interface {
	QueryStart()
	QueryEnd()
}

// WithScheduler wraps an http.HandlerFunc to signal query start/end to the scheduler.
func WithScheduler(sn SchedulerNotifier, next http.HandlerFunc) http.HandlerFunc {
	if sn == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		sn.QueryStart()
		defer sn.QueryEnd()
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

// apiKeyFromRequest extracts and sanitizes the API key from the X-Context-Key
// header or an Authorization: Bearer token. Shared by the Auth middleware
// (connect-time) and the SSE in-stream re-auth path (events.go, G34) so both
// read the key identically.
func apiKeyFromRequest(r *http.Request) string {
	rawKey := r.Header.Get("X-Context-Key")
	if rawKey == "" {
		if bearer := r.Header.Get("Authorization"); strings.HasPrefix(bearer, "Bearer ") {
			rawKey = strings.TrimPrefix(bearer, "Bearer ")
		}
	}
	return auth.SanitizeKey(rawKey)
}

// Auth creates middleware that authenticates requests via X-Context-Key header
// or Authorization: Bearer token. Bearer tokens are treated as API keys.
func Auth(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := apiKeyFromRequest(r)

			result, err := auth.Authenticate(r.Context(), pool, apiKey)
			if err != nil {
				slog.Error("auth failed",
					"error", err,
					"request_id", RequestIDFromContext(r.Context()),
				)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "authentication error"})
				return
			}

			if !result.IsValid {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}

			ctx := context.WithValue(r.Context(), authResultKey, result)
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

