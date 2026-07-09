// DCR guardrails (design 02 §5.2, wave 02-W4b): per-IP rate limit + hard
// table cap on /register. INV-B already makes registration authority-free
// (a client row grants zero data access) — this layer is PURE resource
// protection against table DoS in the open mode. The cap is the hard
// backstop that holds regardless of how the IP counting is evaded.
//
// Inactivity GC is deliberately NOT built: context_oauth_clients carries no
// usage signal (no last_used; context_oauth_codes rows are deleted on
// redeem and swept on expiry, so their absence proves nothing) — a GC keyed
// on created_at alone would delete ACTIVE clients. Revisit when 03's token
// store gives clients an honest last-used trace.
package handler

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// EnvDCRMaxClients caps the context_oauth_clients row count (hard
	// backstop, default 10000). At/over the cap /register answers 403.
	EnvDCRMaxClients = "CTX_OAUTH_MAX_CLIENTS"
	// EnvDCRRegisterRate is the per-IP /register budget per minute
	// (fixed window, default 10). Over budget answers 429.
	EnvDCRRegisterRate = "CTX_OAUTH_REGISTER_RATE"
	// EnvTrustedProxy names the ONE proxy hop whose X-Forwarded-For
	// appendix is trusted for the per-IP counting (see dcrClientIP).
	EnvTrustedProxy = "CTX_TRUSTED_PROXY"

	dcrDefaultMaxClients   = 10000
	dcrDefaultRegisterRate = 10
	dcrRateWindow          = time.Minute
)

// dcrEnvInt reads a positive integer env override, falling back to def on
// unset/garbage/non-positive (fail-closed: a typo never disables the guard).
func dcrEnvInt(name string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// dcrClientIP resolves the source IP the rate limit counts. Default:
// RemoteAddr's host — behind a reverse proxy that is the proxy IP (global
// throttle effect, still fail-closed). ONLY when CTX_TRUSTED_PROXY is set
// AND RemoteAddr matches that hop, the LAST X-Forwarded-For entry is used:
// that entry is the one the trusted hop itself appended (the client as the
// proxy saw it). Everything left of it is client-supplied — naive XFF trust
// is bypassable by rotating fabricated entries, which is exactly why no
// other position in the list is ever consulted and the table cap stays the
// backstop independent of IP counting (design 02 §5.2).
func dcrClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	trusted := strings.TrimSpace(os.Getenv(EnvTrustedProxy))
	if trusted == "" || host != trusted {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	parts := strings.Split(xff, ",")
	if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
		return ip
	}
	return host
}

// dcrRateLimiter is a process-local fixed-window per-IP counter. In-memory
// is sufficient here: multi-instance deployments divide the effective rate
// by the instance count at worst, and the DB cap is the shared backstop.
type dcrRateLimiter struct {
	mu   sync.Mutex
	hits map[string]*dcrWindow
}

type dcrWindow struct {
	start time.Time
	count int
}

// dcrLimiter is the process-wide limiter behind /register.
var dcrLimiter = &dcrRateLimiter{hits: map[string]*dcrWindow{}}

// allow counts one request for ip and reports whether it is within limit
// for the current window.
func (l *dcrRateLimiter) allow(ip string, limit int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Opportunistic memory guard: before growing past ~4k tracked IPs,
	// drop expired windows (spam IPs age out; no background goroutine).
	if len(l.hits) >= 4096 {
		for k, w := range l.hits {
			if now.Sub(w.start) >= dcrRateWindow {
				delete(l.hits, k)
			}
		}
	}

	w, ok := l.hits[ip]
	if !ok || now.Sub(w.start) >= dcrRateWindow {
		l.hits[ip] = &dcrWindow{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= limit
}

// dcrRateLimitOK is the first W4b guard: the in-memory per-IP budget.
// It answers 429 itself and returns false when Register must stop. Runs
// BEFORE body parsing — rejected spam never exercises the JSON path.
func (h *OAuthHandler) dcrRateLimitOK(w http.ResponseWriter, r *http.Request) bool {
	ip := dcrClientIP(r)
	if dcrLimiter.allow(ip, dcrEnvInt(EnvDCRRegisterRate, dcrDefaultRegisterRate), time.Now()) {
		return true
	}
	// The slog line is the forensic trail for open-mode registrations
	// (created_by is NULL there — the IP log is what remains).
	slog.Warn("oauth: dcr register rate limited", "ip", ip)
	writeJSON(w, http.StatusTooManyRequests, map[string]string{
		"error":             "too_many_requests",
		"error_description": "registration rate limit exceeded, retry later",
	})
	return false
}

// dcrCapOK is the second W4b guard: the hard row-count backstop. It runs
// AFTER metadata validation (an invalid request is settled without a DB
// round trip; the rate limiter above throttles spam long before COUNT
// becomes a load factor) and answers 403/500 itself.
func (h *OAuthHandler) dcrCapOK(w http.ResponseWriter, r *http.Request) bool {
	var count int
	if err := h.pool.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM context_oauth_clients`).Scan(&count); err != nil {
		slog.Error("oauth: dcr client cap count", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false // fail-closed: unknown count never admits a write
	}
	if count >= dcrEnvInt(EnvDCRMaxClients, dcrDefaultMaxClients) {
		slog.Warn("oauth: dcr register cap reached", "count", count, "ip", dcrClientIP(r))
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":             "too_many_clients",
			"error_description": "client registration capacity reached",
		})
		return false
	}
	return true
}
