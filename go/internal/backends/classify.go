package backends

import (
	"context"
	"encoding/json"
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

// Error classes, catalog order. The values are iota: a new class is
// appended at the end, which changes none of the existing ones.
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

	// ctx peer classes (W02-11, masterplan K15), appended: iota continues.
	ClassBusy           // ctx peer refused admission (server_busy, concurrency_limit_exceeded) → STOP, NO cooldown
	ClassQuotaExhausted // ctx peer: insufficient_quota → STOP, Retry-After|1h, cap 24h
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
	case ClassBusy:
		return "busy"
	case ClassQuotaExhausted:
		return "quota_exhausted"
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
	case ClassBusy, ClassQuotaExhausted:
		// Fail-closed until their walk rules land: after busy the chain may
		// go on to further ctx rows only (design/04 §4.4 AfterBusy, F-W10),
		// after quota to non-billable rows only (design/06 K-06-26, W06-14).
		// Until then neither goes on at all — the next row being a cloud row
		// is exactly the spill both classes exist to prevent. This holds
		// per request: the quota cooldown still sorts the row behind the
		// others for the following requests (Pool.Chain), which the
		// successor rule of W06-14 has to cover as well.
		return false
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
	case ClassQuotaExhausted:
		// The peer's budget is spent until its window resets, and the
		// Retry-After names that reset (design/06 §4.4): the full hint counts
		// — the 5-minute rate-limit cap would re-probe a peer exhausted for
		// the day every 5 minutes (design/06 K-06-26).
		d := quotaCooldownDefault
		if retryAfter > 0 {
			d = retryAfter
		}
		return min(d, quotaCooldownCap)
	default: // BadRequest (request-specific — don't punish the backend), Busy (a capacity signal, not health), STOP classes
		return 0
	}
}

const (
	// quotaCooldownDefault is the quota cooldown without a Retry-After (a
	// header-stripping proxy in between, an error event inside a stream).
	// One hour is the granularity a ctx peer computes quota relief in (the
	// tenant gate's hour buckets, design/06 §4.4; design/02 §4.10.2): at
	// most one probe per window, and a reset is noticed within the hour.
	quotaCooldownDefault = time.Hour
	// quotaCooldownCap is the 24-hour cap of design/06 K-06-26: a monthly
	// budget is re-probed once a day instead of being slept through on one
	// header.
	quotaCooldownCap = 24 * time.Hour
)

// Classify maps one attempt error onto the failover catalog. providerClass
// refines provider-specific permanent failures (OpenRouter "no providers")
// and selects the ctx column of the gateway-code table (a peer ctx names its
// reason in error.code, in the body of an error answer and inside a stream).
// Context errors must be checked FIRST: a canceled parent wraps everything.
// providerErrorTyper is implemented by llm.StreamError: the provider's error
// type of an error event delivered inside an HTTP-200 stream.
type providerErrorTyper interface {
	ProviderErrorType() string
}

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
		// A ctx peer's error code wins over the status, with the same
		// precedence as the no-providers body: 429 is a capacity refusal
		// (server_busy) or a spent budget (insufficient_quota), 504 an
		// upstream timeout on the peer (upstream_timeout). Every other
		// provider class keeps the status mapping below.
		if class, ok := ctxAnswerClass(providerClass, se.Body); ok {
			return class
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
	// A mid-stream error event (HTTP 200, error object inside the SSE
	// stream) carries the provider's error TYPE instead of a status code.
	// The anthropic wire names the same types its HTTP errors do, so they map
	// onto the classes the equivalent status would get — otherwise a 529
	// overloaded_error would fail over as HTTP but stop the chain as an
	// event. The codes of a ctx gateway map the same way through
	// gatewayCodeClasses. Unknown types (OpenRouter's numeric codes) fall
	// through to the stop-the-chain default below.
	var pe providerErrorTyper
	if errors.As(err, &pe) {
		if class, ok := GatewayCodeClass(providerClass, pe.ProviderErrorType()); ok {
			return class
		}
		switch pe.ProviderErrorType() {
		case "overloaded_error":
			return ClassGateway
		case "api_error":
			return ClassServerFault
		case "rate_limit_error":
			return ClassRateLimit
		case "billing_error":
			return ClassBilling
		case "authentication_error", "permission_error":
			return ClassAuth
		case "invalid_request_error", "not_found_error", "request_too_large":
			return ClassBadRequest
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

// gatewayCodeClasses maps the error codes of a ctx gateway (the internal/oai
// vocabulary, design/02 §4.10.2) onto the catalog, on every provider class.
// Each code takes the class its own status gets on the status path of
// Classify, so a code inside a stream fails over exactly like the HTTP error
// it stands for (design/02 §4.10.3, corrected by masterplan K6: server_busy
// and concurrency_limit_exceeded are 429 codes). Data, not a switch cascade:
// internal/oai TestClassifierTotality checks it against the vocabulary, and
// a code without an entry turns that test red. The federation codes (508)
// are an unknown 5xx on the status path today; their own class (ClassLoop)
// arrives with F-W10.
var gatewayCodeClasses = map[string]ErrClass{
	// 429
	"rate_limit_exceeded":        ClassRateLimit,
	"server_busy":                ClassRateLimit,
	"concurrency_limit_exceeded": ClassRateLimit,
	"insufficient_quota":         ClassRateLimit,
	// 503, 504, 508
	"upstream_unavailable": ClassGateway,
	"server_overloaded":    ClassGateway,
	"internal_error":       ClassGateway,
	"model_unavailable":    ClassGateway,
	"no_eligible_backend":  ClassGateway,
	"service_unavailable":  ClassGateway,
	"upstream_timeout":     ClassGateway,
	"federation_loop":      ClassGateway,
	"federation_depth":     ClassGateway,
	// the model-key door's 503s (W01-6, in the vocabulary since W02-05)
	"proxy_misconfigured": ClassGateway,
	"auth_overloaded":     ClassGateway,
	"auth_unavailable":    ClassGateway,
	// 500 — the one STOP code: the upstream ran the request
	"upstream_error": ClassServerFault,
	// 401, 403
	"invalid_api_key":        ClassAuth,
	"permission_denied":      ClassAuth,
	"peer_instance_mismatch": ClassAuth,
	// 400, 404, 405, 413 — request-specific
	"upstream_rejected":          ClassBadRequest,
	"context_length_exceeded":    ClassBadRequest,
	"unsupported_parameter":      ClassBadRequest,
	"unsupported_value":          ClassBadRequest,
	"model_not_found":            ClassBadRequest,
	"not_found":                  ClassBadRequest,
	"method_not_allowed":         ClassBadRequest,
	"request_too_large":          ClassBadRequest,
	"invalid_json":               ClassBadRequest,
	"missing_required_parameter": ClassBadRequest,
	"invalid_type":               ClassBadRequest,
	"model_not_supported":        ClassBadRequest,
	"federation_header_invalid":  ClassBadRequest,
	"federation_hop_required":    ClassBadRequest,
}

// ctxCodeClasses are the entries of a ctx row (masterplan K15), consulted
// before gatewayCodeClasses. A's admission refusal — L2 server_busy, L1
// concurrency_limit_exceeded — is a capacity signal, not health (design/03
// E-03-12); A's upstream attempt timeout keeps the slow-but-alive STOP of
// this catalog over the hop (design/04 §4.4); A's quota answer cools the row
// until the window resets (design/06 K-06-26).
var ctxCodeClasses = map[string]ErrClass{
	"server_busy":                ClassBusy,
	"concurrency_limit_exceeded": ClassBusy,
	"upstream_timeout":           ClassTimeout,
	"insufficient_quota":         ClassQuotaExhausted,
}

// GatewayCodeClass maps one gateway error code onto the catalog for a
// provider class: the ctx column for ProviderCtx, the generic one for every
// other class. ok is false for a code outside the table (OpenRouter's
// numeric codes, the anthropic wire's error types, "").
func GatewayCodeClass(providerClass, code string) (ErrClass, bool) {
	if providerClass == ProviderCtx {
		if class, ok := ctxCodeClasses[code]; ok {
			return class, true
		}
	}
	class, ok := gatewayCodeClasses[code]
	return class, ok
}

// ctxAnswerClass classifies the error answer of a ctx row by the code in its
// body; ok is false for every other provider class and for a body without a
// code the table knows — the status decides then.
func ctxAnswerClass(providerClass, body string) (ErrClass, bool) {
	if providerClass != ProviderCtx {
		return ClassOK, false
	}
	return GatewayCodeClass(ProviderCtx, bodyErrorCode(body))
}

// bodyErrorCode reads error.code from an error body in the OpenAI envelope
// ({"error":{"code":"…"}}, what a ctx gateway answers with). Only the code
// is read — the body itself stays log-only (httpx.StatusError.Body). Any
// other body (no JSON, truncated, a string error, a null or numeric code)
// yields "".
func bodyErrorCode(body string) string {
	var env struct {
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return ""
	}
	var code string
	if json.Unmarshal(env.Error.Code, &code) != nil {
		return ""
	}
	return code
}
