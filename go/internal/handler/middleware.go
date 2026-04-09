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

// Auth creates middleware that authenticates requests via X-Context-Key header
// or Authorization: Bearer token. Bearer tokens are treated as API keys.
func Auth(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawKey := r.Header.Get("X-Context-Key")
			if rawKey == "" {
				if bearer := r.Header.Get("Authorization"); strings.HasPrefix(bearer, "Bearer ") {
					rawKey = strings.TrimPrefix(bearer, "Bearer ")
				}
			}
			apiKey := auth.SanitizeKey(rawKey)

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

