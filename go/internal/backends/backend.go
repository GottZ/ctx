// Package backends holds the Backend tuple type and (since F3-P1) the
// declarative backend pool: context_backends rows loaded into an immutable
// atomic snapshot, role-filtered priority chains, the trust×sensitivity
// permission matrix, the failover error catalog and per-backend health.
// Host and Protocol select the wire path together (/api/chat vs
// /v1/chat/completions) — they only ever travel as one value, never as loose
// parameters (F1 inventory §2.3). The F1 tuple fields stay untouched; F3
// extends the SAME type additively (masterplan K4 — no second type).
//
// Log hygiene is by construction: every serialization path masks APIKey.
// LogValue covers slog, String covers %v/%+v/%s, GoString covers %#v (fmt uses
// GoStringer there, NOT Stringer), and the `json:"-"` tag covers
// encoding/json (which ignores both Stringer interfaces).
package backends

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Core roles of the routing table (design 03 §2.2). roles entries outside
// this set are allowed (proxy vision) but produce API warnings; model_map
// must cover every core role a backend carries.
const (
	RoleSynthesis  = "synthesis"
	RoleTranslate  = "translate"
	RoleEmbed      = "embed"
	RoleRerank     = "rerank"
	RoleDream      = "dream"
	RoleDigest     = "digest"
	RoleChat       = "chat"
	RoleDreamEmbed = "dream-embed" // bootstrap split when CTX_DREAM_EMBED_* diverges
	RoleClassify   = "classify"    // G41 sensitivity audit — hard-local, no external escape hatch
)

// CoreRoles lists the roles the validation treats as known. dream-embed is
// included: it exists so the bootstrap never silently drops the historical
// query-embed vs dream-embed split.
var CoreRoles = []string{
	RoleSynthesis, RoleTranslate, RoleEmbed, RoleRerank,
	RoleDream, RoleDigest, RoleChat, RoleDreamEmbed, RoleClassify,
}

// embedRoles write into the shared vector(1024) space — external backends
// carrying them are hard-blocked without an explicit equivalence proof
// (index corruption by foreign quantization is irreversible at scale).
var embedRoles = map[string]bool{RoleEmbed: true, RoleDreamEmbed: true}

// Provider classes refine wire behavior beyond the protocol (OpenRouter:
// forced zdr/deny, usage.cost, no-providers classification).
const (
	ProviderGeneric    = "generic"
	ProviderLlamaCpp   = "llamacpp"
	ProviderOpenRouter = "openrouter"
)

// Localities for the egress audit dimension; validated against base_url.
const (
	LocalityLocal    = "local"
	LocalityLAN      = "lan"
	LocalityExternal = "external"
)

// ModelSpec is one model_map value: which model a role resolves to on this
// backend, plus per-(backend, role) parameter overrides (top_p, top_k, min_p,
// think, …). The JSONB short form "model-id" normalizes to ModelSpec{Model:…}
// on load; the object form exists from day one so no row format migration is
// ever needed (F3 review).
type ModelSpec struct {
	Model  string         `json:"model"`
	Params map[string]any `json:"params,omitempty"`
}

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

// GlobalScope is the shared-backend sentinel on context_backends.scope (062
// DEFAULT): a '_global' backend is visible to every tenant. A '<tenant>' scope
// is tenant-private. Mirrors store.GlobalScope (the F2 settings/secrets identity)
// without the import — the value is a DDL constant anchored in migration 062.
const GlobalScope = "_global"

// Backend is the indivisible connection tuple of one inference role (F1
// fields) extended by the pool row dimensions (F3, additive — K4). F1
// consumers keep reading Host/Model/NumCtx/Think/Timeout from config-derived
// tuples; pool consumers (P2+) resolve per role via ModelFor/TimeoutFor.
type Backend struct {
	// Host is the connection base URL (DDL column: base_url).
	Host string
	// APIKey carries `json:"-"` because encoding/json ignores Stringer and
	// would serialize the raw key — config.Redacted is the only legitimate
	// marshal path for backend tuples. Pool rows resolve it in-memory from
	// the F2 secret named by APIKeyRef; it never exists in context_backends.
	APIKey   string `json:"-"`
	Protocol Protocol
	// Model empty on a fallback backend = inherit the primary model (today's
	// semantics). Pool consumers use ModelFor(role) instead.
	Model  string
	NumCtx int
	// Think is ignored by embed/rerank roles. Pool rows carry the toggle as
	// model_map params.think per role.
	Think ThinkMode
	// Timeout is a per-backend override; 0 = call-site default. Pool
	// consumers use TimeoutFor(role, def) instead.
	Timeout time.Duration

	// --- F3 pool row fields (zero values on F1 config-derived tuples) ---

	ID   string
	Name string
	// Scope is the tenant dimension (062, Modell C): '_global' = shared server
	// backend, '<tenant>' = tenant-private. Loaded by scanBackend; Chain()
	// filters on it via visibleTo (04-W2/T34) so a tenant-private backend is
	// never reachable from a foreign caller's chain (egress isolation, R-LEAK7).
	Scope         string
	ProviderClass string
	// APIKeyRef names the F2 secret this backend authenticates with; "" =
	// keyless. The NAME is harmless and shows up in list responses — the
	// resolved value only ever lives in APIKey (in-memory, masked).
	APIKeyRef    string
	Trust        Trust
	Locality     string
	Roles        []string
	ModelMap     map[string]ModelSpec
	Timeouts     map[string]int // role → seconds; missing = code default of the role
	Priority     int
	Enabled      bool
	ExtraHeaders map[string]string
	ExtraBody    map[string]any
	Limits       map[string]any
	Metadata     map[string]any
}

// HasRole reports whether the backend carries the given role.
func (b *Backend) HasRole(role string) bool {
	for _, r := range b.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// ModelFor resolves the model spec for a role: exact role key, then
// "default", then the F1 Model field (config-derived tuples without a map),
// then the zero spec (caller decides whether that is an error).
func (b *Backend) ModelFor(role string) ModelSpec {
	if spec, ok := b.ModelMap[role]; ok {
		return spec
	}
	if spec, ok := b.ModelMap["default"]; ok {
		return spec
	}
	return ModelSpec{Model: b.Model}
}

// TimeoutFor resolves the per-role timeout override, falling back to def.
func (b *Backend) TimeoutFor(role string, def time.Duration) time.Duration {
	if secs, ok := b.Timeouts[role]; ok && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return def
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
// never leak the APIKey, regardless of call-site discipline. Pool fields log
// identity + policy, never ExtraHeaders values (denylist-validated, but the
// log path stays leak-free even against a missed pattern).
func (b Backend) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", b.Name),
		slog.String("host", b.Host),
		slog.String("api_key", b.maskedAPIKey()),
		slog.String("protocol", string(b.Protocol)),
		slog.String("model", b.Model),
		slog.Int("num_ctx", b.NumCtx),
		slog.String("think", string(b.Think)),
		slog.Duration("timeout", b.Timeout),
		slog.String("trust", string(b.Trust)),
		slog.String("locality", b.Locality),
		slog.String("roles", strings.Join(b.Roles, ",")),
		slog.Int("priority", b.Priority),
		slog.Bool("enabled", b.Enabled),
	)
}

// String implements fmt.Stringer (covers %v, %+v, %s) with the same masking.
func (b Backend) String() string {
	return fmt.Sprintf(
		"Backend{Name:%q, Host:%q, APIKey:%q, Protocol:%q, Model:%q, NumCtx:%d, Think:%q, Timeout:%s, Trust:%q, Locality:%q, Roles:%q, Priority:%d, Enabled:%t}",
		b.Name, b.Host, b.maskedAPIKey(), string(b.Protocol), b.Model, b.NumCtx, string(b.Think), b.Timeout,
		string(b.Trust), b.Locality, strings.Join(b.Roles, ","), b.Priority, b.Enabled,
	)
}

// GoString implements fmt.GoStringer: %#v bypasses Stringer and would print
// every field raw without this.
func (b Backend) GoString() string {
	return b.String()
}
