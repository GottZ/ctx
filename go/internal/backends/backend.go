// Package backends holds the Backend tuple type: the indivisible connection
// descriptor of one inference role (chat, chat-fallback, embed, dream,
// dream-embed). Host and Protocol select the wire path together (/api/chat vs
// /v1/chat/completions) — they only ever travel as one value, never as loose
// parameters (F1 inventory §2.3). F3 grows this package into the backend
// pool/resolver (named backends, health, trust levels); the type is the
// landing zone.
//
// Log hygiene is by construction: every serialization path masks APIKey.
// LogValue covers slog, String covers %v/%+v/%s, GoString covers %#v (fmt uses
// GoStringer there, NOT Stringer), and the `json:"-"` tag covers
// encoding/json (which ignores both Stringer interfaces).
package backends

import (
	"fmt"
	"log/slog"
	"time"
)

// Protocol selects the wire dialect of a backend.
type Protocol string

// Supported wire protocols. Empty is only valid where a field documents
// inheritance (dream-embed); config validation V4 rejects everything else.
const (
	ProtocolOllama Protocol = "ollama"
	ProtocolOpenAI Protocol = "openai"
)

// ThinkMode is the reasoning toggle as configured: "true" | "false" | ""
// (= omit from the request).
type ThinkMode string

// Ptr converts the mode to the wire form. The *bool is freshly allocated on
// every call so no pointer is ever shared between config generations.
func (t ThinkMode) Ptr() *bool {
	switch t {
	case "true":
		v := true
		return &v
	case "false":
		v := false
		return &v
	default:
		return nil
	}
}

// Backend is the indivisible connection tuple of one inference role.
type Backend struct {
	Host string
	// APIKey carries `json:"-"` because encoding/json ignores Stringer and
	// would serialize the raw key — config.Redacted is the only legitimate
	// marshal path for backend tuples.
	APIKey   string `json:"-"`
	Protocol Protocol
	// Model empty on a fallback backend = inherit the primary model (today's
	// semantics). The field exists now so F3 model remapping is pure fill-in.
	Model  string
	NumCtx int
	// Think is ignored by embed/rerank roles.
	Think ThinkMode
	// Timeout is a per-backend override; 0 = call-site default. F1: only the
	// chat fallback sets it (CPU synthesis needs minutes, not seconds).
	Timeout time.Duration
}

// maskedAPIKey is presence-only: backends never expose key material or
// fingerprints — fingerprinting is the boot-dump's privilege (config.Redacted).
func (b Backend) maskedAPIKey() string {
	if b.APIKey == "" {
		return ""
	}
	return "set"
}

// LogValue implements slog.LogValuer: structured logging of a Backend can
// never leak the APIKey, regardless of call-site discipline.
func (b Backend) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("host", b.Host),
		slog.String("api_key", b.maskedAPIKey()),
		slog.String("protocol", string(b.Protocol)),
		slog.String("model", b.Model),
		slog.Int("num_ctx", b.NumCtx),
		slog.String("think", string(b.Think)),
		slog.Duration("timeout", b.Timeout),
	)
}

// String implements fmt.Stringer (covers %v, %+v, %s) with the same masking.
func (b Backend) String() string {
	return fmt.Sprintf(
		"Backend{Host:%q, APIKey:%q, Protocol:%q, Model:%q, NumCtx:%d, Think:%q, Timeout:%s}",
		b.Host, b.maskedAPIKey(), string(b.Protocol), b.Model, b.NumCtx, string(b.Think), b.Timeout,
	)
}

// GoString implements fmt.GoStringer: %#v bypasses Stringer and would print
// every field raw without this.
func (b Backend) GoString() string {
	return b.String()
}
