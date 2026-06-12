package backends

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
)

// ErrClass is the failover decision for one failed backend attempt. Each
// class is individually decided in the F3 catalog (design 03 §2.4): whether
// the chain continues to the next backend and which cooldown the failed one
// earns. The chain executor (P2) consumes Next(); the pool consumes
// Cooldown() via ReportFailure.
type ErrClass int

// Error classes, catalog order.
const (
	ClassOK          ErrClass = iota
	ClassTransport            // dial/refused/DNS/reset/EOF → next, 30s
	ClassServerFault          // HTTP 500 → STOP (doctrine anchor: the server ran the request)
	ClassGateway              // 502/503/504 → next, 30s
	ClassRateLimit            // 429 → next, Retry-After|60s, cap 5m
	ClassBilling              // 402 → next, 1h, error log
	ClassAuth                 // 401/403 → next, 15m, error log + pool reload (secret rotation self-heal)
	ClassBadRequest           // 400/404/413 → next, NO cooldown (request-specific)
	ClassNoProviders          // OpenRouter config-permanent → next, 1h, error log
	ClassTimeout              // attempt deadline, parent alive → STOP (unless pool.failover_on_timeout)
	ClassCanceled             // parent ctx canceled → STOP immediately
)

// String renders the class for slog/status output.
func (c ErrClass) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassTransport:
		return "transport"
	case ClassServerFault:
		return "server_fault"
	case ClassGateway:
		return "gateway"
	case ClassRateLimit:
		return "rate_limit"
	case ClassBilling:
		return "billing"
	case ClassAuth:
		return "auth"
	case ClassBadRequest:
		return "bad_request"
	case ClassNoProviders:
		return "no_providers"
	case ClassTimeout:
		return "timeout"
	case ClassCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Next reports whether the chain should try the following backend after this
// class. Timeout follows the "slow-but-alive is not down" doctrine — the
// settings flip pool.failover_on_timeout (P2) overrides at the call site.
func (c ErrClass) Next() bool {
	switch c {
	case ClassTransport, ClassGateway, ClassRateLimit, ClassBilling, ClassAuth, ClassBadRequest, ClassNoProviders:
		return true
	default: // ServerFault, Timeout, Canceled, OK
		return false
	}
}

// Cooldown returns the base cooldown this class earns the backend.
// retryAfter (from a 429) wins over the class default, capped at 5 minutes.
func (c ErrClass) Cooldown(retryAfter time.Duration) time.Duration {
	switch c {
	case ClassTransport, ClassGateway:
		return 30 * time.Second
	case ClassRateLimit:
		d := 60 * time.Second
		if retryAfter > 0 {
			d = retryAfter
		}
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		return d
	case ClassBilling, ClassNoProviders:
		return time.Hour
	case ClassAuth:
		return 15 * time.Minute
	default: // BadRequest (request-specific — don't punish the backend), STOP classes
		return 0
	}
}

// Classify maps one attempt error onto the failover catalog. providerClass
// refines provider-specific permanent failures (OpenRouter "no providers").
// Context errors must be checked FIRST: a canceled parent wraps everything.
func Classify(err error, providerClass string) ErrClass {
	if err == nil {
		return ClassOK
	}
	if errors.Is(err, context.Canceled) {
		return ClassCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTimeout
	}
	var se *httpx.StatusError
	if errors.As(err, &se) {
		if providerClass == ProviderOpenRouter && isNoProvidersBody(se.Body) {
			return ClassNoProviders
		}
		switch se.Code {
		case 500:
			return ClassServerFault
		case 502, 503, 504:
			return ClassGateway
		case 429:
			return ClassRateLimit
		case 402:
			return ClassBilling
		case 401, 403:
			return ClassAuth
		case 400, 404, 413:
			return ClassBadRequest
		default:
			if se.Code >= 500 {
				return ClassGateway // unknown 5xx: infrastructure said no
			}
			return ClassBadRequest // unknown 4xx: request-specific, no cooldown
		}
	}
	if httpx.IsBackendUnavailable(err) {
		return ClassTransport
	}
	// Unclassified (marshal/decode/transport oddities): stop the chain — a
	// malformed response is not evidence the next backend would do better,
	// and eskalating unknown failures is how silent loops start.
	return ClassServerFault
}

// isNoProvidersBody matches OpenRouter's configuration-permanent failure
// ("No allowed providers are available for the selected model", e.g.
// zdr:true without a ZDR endpoint, inventory A6). Retrying is pointless —
// the condition only clears through a config change.
func isNoProvidersBody(body string) bool {
	b := strings.ToLower(body)
	return strings.Contains(b, "no allowed providers") ||
		strings.Contains(b, "no providers are available")
}
