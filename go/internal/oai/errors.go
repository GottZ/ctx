// Package oai is the OpenAI-compatible wire surface of ctxd: the /v1 group
// (and /v2/rerank) that external programs reach with a model key
// (design/02-wire.md). Wave W02-02 lays its foundation — the error envelope,
// the enabled gate, the in-flight budget and the recovery middleware; the
// route group, the model-key door and the handlers arrive in later waves.
//
// Layering (.golangci.yml depguard oai-isolation and config-layering): the
// package imports no corpus package (rrf, chat, dream, events, store) and no
// internal/config. Settings arrive through an injected getter
// (func() Settings, built in cmd/ctxd), so every request reads one snapshot
// of the hot gateway.* keys. The package makes no wire call of its own
// (design/02 §4.1, L4).
package oai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
)

// Error types — the `type` field of the envelope (design/02 §4.10.2). The
// OpenAI SDKs map exceptions by HTTP status, not by type; the type is what a
// client logs and what a peer ctx reads alongside the code.
const (
	TypeInvalidRequest     = "invalid_request_error"
	TypeRateLimit          = "rate_limit_error"
	TypeServiceUnavailable = "service_unavailable_error"
	TypeServer             = "server_error"
	TypeInsufficientQuota  = "insufficient_quota"
)

// Error codes — the `code` field. The set is the vocabulary a client (and a
// peer ctx, through backends.Classify, W02-11) can meet on this surface.
const (
	CodeNotFound                 = "not_found"
	CodeMethodNotAllowed         = "method_not_allowed"
	CodeInvalidAPIKey            = "invalid_api_key"
	CodePermissionDenied         = "permission_denied"
	CodeRateLimitExceeded        = "rate_limit_exceeded"
	CodeServerBusy               = "server_busy"
	CodeConcurrencyLimitExceeded = "concurrency_limit_exceeded"
	CodeInsufficientQuota        = "insufficient_quota"
	CodeInternalError            = "internal_error"
	CodeRequestTooLarge          = "request_too_large"
	CodeServerOverloaded         = "server_overloaded"
	CodeServiceUnavailable       = "service_unavailable"
	CodeInvalidJSON              = "invalid_json"
	CodeMissingRequiredParameter = "missing_required_parameter"
	CodeInvalidType              = "invalid_type"
	CodeUnsupportedParameter     = "unsupported_parameter"
	CodeUnsupportedValue         = "unsupported_value"
	CodeModelNotFound            = "model_not_found"
	CodeModelNotSupported        = "model_not_supported"
	CodeModelUnavailable         = "model_unavailable"
	CodeNoEligibleBackend        = "no_eligible_backend"
	CodeUpstreamUnavailable      = "upstream_unavailable"
	CodeUpstreamRejected         = "upstream_rejected"
	CodeContextLengthExceeded    = "context_length_exceeded"
	CodeUpstreamError            = "upstream_error"
	CodeUpstreamTimeout          = "upstream_timeout"
	CodeFederationHeaderInvalid  = "federation_header_invalid"
	CodeFederationLoop           = "federation_loop"
	CodeFederationDepth          = "federation_depth"
	CodeFederationHopRequired    = "federation_hop_required"
	CodePeerInstanceMismatch     = "peer_instance_mismatch"
)

// CodeSpec binds a code to the one status and type it is sent with.
type CodeSpec struct {
	Code   string
	Status int
	Type   string
}

// codes is the vocabulary behind Codes (design/02 §4.10.2; the dispatcher's
// saturation code server_busy, masterplan K6; the limiter codes of design/06
// §4.4; the federation codes of design/04 §4.6). Each code has exactly one
// status and type; what varies per situation is x-should-retry and the
// Retry-After hint, which the writer call carries. Data, not behaviour:
// W02-11 checks the classifier for totality against this table.
//
// Not listed: tokens_exceed_limit — design/06 decided E-06-9 for (a)
// (Finding 31: reservations are capped, never refused), so nothing sends it.
// The federation codes have no type in design/04; they take the convention
// of the table above: 4xx ⇒ invalid_request_error, the non-503 5xx (508) ⇒
// server_error.
var codes = []CodeSpec{
	{CodeNotFound, http.StatusNotFound, TypeInvalidRequest},
	{CodeMethodNotAllowed, http.StatusMethodNotAllowed, TypeInvalidRequest},
	{CodeInvalidAPIKey, http.StatusUnauthorized, TypeInvalidRequest},
	{CodePermissionDenied, http.StatusForbidden, TypeInvalidRequest},
	{CodeRateLimitExceeded, http.StatusTooManyRequests, TypeRateLimit},
	{CodeServerBusy, http.StatusTooManyRequests, TypeRateLimit},
	{CodeConcurrencyLimitExceeded, http.StatusTooManyRequests, TypeRateLimit},
	{CodeInsufficientQuota, http.StatusTooManyRequests, TypeInsufficientQuota},
	{CodeInternalError, http.StatusServiceUnavailable, TypeServiceUnavailable},
	{CodeRequestTooLarge, http.StatusRequestEntityTooLarge, TypeInvalidRequest},
	{CodeServerOverloaded, http.StatusServiceUnavailable, TypeServiceUnavailable},
	{CodeServiceUnavailable, http.StatusServiceUnavailable, TypeServiceUnavailable},
	{CodeInvalidJSON, http.StatusBadRequest, TypeInvalidRequest},
	{CodeMissingRequiredParameter, http.StatusBadRequest, TypeInvalidRequest},
	{CodeInvalidType, http.StatusBadRequest, TypeInvalidRequest},
	{CodeUnsupportedParameter, http.StatusBadRequest, TypeInvalidRequest},
	{CodeUnsupportedValue, http.StatusBadRequest, TypeInvalidRequest},
	{CodeModelNotFound, http.StatusNotFound, TypeInvalidRequest},
	{CodeModelNotSupported, http.StatusBadRequest, TypeInvalidRequest},
	{CodeModelUnavailable, http.StatusServiceUnavailable, TypeServiceUnavailable},
	{CodeNoEligibleBackend, http.StatusServiceUnavailable, TypeServiceUnavailable},
	{CodeUpstreamUnavailable, http.StatusServiceUnavailable, TypeServiceUnavailable},
	{CodeUpstreamRejected, http.StatusBadRequest, TypeInvalidRequest},
	{CodeContextLengthExceeded, http.StatusBadRequest, TypeInvalidRequest},
	{CodeUpstreamError, http.StatusInternalServerError, TypeServer},
	{CodeUpstreamTimeout, http.StatusGatewayTimeout, TypeServer},
	{CodeFederationHeaderInvalid, http.StatusBadRequest, TypeInvalidRequest},
	{CodeFederationLoop, http.StatusLoopDetected, TypeServer},
	{CodeFederationDepth, http.StatusLoopDetected, TypeServer},
	{CodeFederationHopRequired, http.StatusBadRequest, TypeInvalidRequest},
	{CodePeerInstanceMismatch, http.StatusForbidden, TypeInvalidRequest},
}

// Codes returns the vocabulary as a copy (Fremd-Review 2, FR2-7): the table
// W02-11 checks its classifier against for totality cannot be changed at
// run time by any importer.
func Codes() []CodeSpec { return slices.Clone(codes) }

// overloadedRetryAfter is the Retry-After of a budget refusal (design/02
// §4.2.1, §4.10.2: "Retry-After: 1"). Hardcoded on purpose and not a setting:
// the budget frees as requests finish, so the hint is "the next second", not
// a policy; the dispatcher's quantized, jittered hints (masterplan K6) are a
// different signal and live in the dispatcher.
const overloadedRetryAfter = "1"

// Error is one envelope answer. Code and Param render as JSON null when
// empty. ShouldRetry is always sent as an explicit x-should-retry header:
// both SDKs honour it over their status-based default, so the server — not
// the client's guess — decides whether a retry can help.
type Error struct {
	Status      int
	Type        string
	Code        string
	Param       string
	Message     string
	ShouldRetry bool
}

// envelope is the wire form: all four keys, always (OpenAI spec).
type envelope struct {
	Error envelopeError `json:"error"`
}

type envelopeError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Write sends e. Headers set by the caller before the call (Retry-After,
// Allow, WWW-Authenticate) are kept; Content-Type and x-should-retry are set
// here.
func Write(w http.ResponseWriter, e Error) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.ShouldRetry {
		h.Set("x-should-retry", "true")
	} else {
		h.Set("x-should-retry", "false")
	}
	w.WriteHeader(e.Status)
	body := envelope{Error: envelopeError{Message: e.Message, Type: e.Type, Param: nullable(e.Param), Code: nullable(e.Code)}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("gateway: failed to encode error envelope", "error", err, "status", e.Status, "code", e.Code)
	}
}

// NotFound is the gateway's one 404: an unknown path, and every request
// while gateway.enabled is false. Fixed bytes — no method, path or model
// name in the message — so the two cases are indistinguishable and no
// answer reflects request input. Handler signature, so a router can mount
// it as its NotFound handler.
func NotFound(w http.ResponseWriter, _ *http.Request) {
	Write(w, Error{Status: http.StatusNotFound, Type: TypeInvalidRequest, Code: CodeNotFound,
		Message: "The requested resource does not exist."})
}

// MethodNotAllowed is the gateway's 405. RFC 9110 requires an Allow header;
// this writer keeps one set before the call but cannot compute it. chi does
// NOT set Allow for a custom MethodNotAllowed handler (chi v5.5.0 mux.go:
// only its default handler does), so the wave that mounts the group computes
// Allow per path before calling this (W01-6).
func MethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	Write(w, Error{Status: http.StatusMethodNotAllowed, Type: TypeInvalidRequest, Code: CodeMethodNotAllowed,
		Message: "The request method is not allowed for this endpoint."})
}

// WriteInvalidAPIKey is the 401 of the model-key door (W01-6): missing,
// malformed, unknown or revoked key, and every knowledge credential. One
// text for all of them — the answer does not say which.
func WriteInvalidAPIKey(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="ctx"`)
	Write(w, Error{Status: http.StatusUnauthorized, Type: TypeInvalidRequest, Code: CodeInvalidAPIKey,
		Message: "Incorrect or missing API key."})
}

// WriteRequestTooLarge is the 413 of the strict body cap (design/02 §4.2
// stage 3, mounted after the door, so only an authenticated caller learns
// the cap). Signature of handler.MaxBodySizeStrictWith's onTooLarge.
func WriteRequestTooLarge(w http.ResponseWriter, maxBytes int64) {
	Write(w, Error{Status: http.StatusRequestEntityTooLarge, Type: TypeInvalidRequest, Code: CodeRequestTooLarge,
		Message: fmt.Sprintf("The request body exceeds the limit of %d bytes.", maxBytes)})
}

// WriteOverloaded is the 503 of the in-flight budget (design/02 §4.2.1):
// refused at once, never queued, retry after a second.
func WriteOverloaded(w http.ResponseWriter) {
	w.Header().Set("Retry-After", overloadedRetryAfter)
	Write(w, Error{Status: http.StatusServiceUnavailable, Type: TypeServiceUnavailable, Code: CodeServerOverloaded,
		Message: "The server is temporarily overloaded; retry shortly.", ShouldRetry: true})
}

// WriteInternal is the A-internal fault (masterplan K5, K29; R-02-14): a
// panic, an auth-database error, a wiring defect. 503, not 500 — a 500 stops
// a peer ctx's whole chain (backends/classify.go: STOP), a 503 lets it fall
// to its next row — and x-should-retry: false, because the same request
// against the same instance is not expected to fare better. The only 500 on
// this surface is a relayed upstream 500 (code upstream_error, W02-07).
func WriteInternal(w http.ResponseWriter) {
	Write(w, Error{Status: http.StatusServiceUnavailable, Type: TypeServiceUnavailable, Code: CodeInternalError,
		Message: "The server could not complete the request."})
}
