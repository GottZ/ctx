// Package config is the central runtime configuration: typed parsing from a
// keyed source (env in F1, context_settings on top in F2), an invariant layer
// (Validate), an atomic snapshot store, and a redacted boot dump.
//
// Layering rule (machine-enforced via depguard): only cmd/**,
// internal/handler, internal/events and internal/settings (the F2 reload
// owner) may import this package. Domain packages (llm, embed, dream, rrf,
// rerank, …) stay parameter-pure — they receive backends.Backend tuples and
// plain values as arguments.
//
// Immutability convention: a *Config published through Store.Replace or
// NewStore is never mutated afterwards. Updates are copy-on-write:
//
//	c := *store.Snapshot()
//	c.Rerank.BlendWeight = 0.5
//	store.Replace(&c)
//
// A shallow copy suffices as long as reference fields are replaced, not
// mutated: Scheduler.ReadScopes (fresh slice per load; read-only for
// consumers), Query.Timezone (*time.Location is immutable by contract), and
// the unexported sources map (written once by the loader). ThinkMode is a
// string, and ThinkMode.Ptr allocates fresh — no *bool is shared between
// generations.
package config

import (
	"fmt"
	"net/url"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
)

// Hours is a duration in hours parsed from the domain suffix form h|d|w|m|y
// (m = month = 30d — deliberately NOT time.ParseDuration, where m means
// minutes) or a bare number meaning hours.
type Hours float64

// Field tags (the registry contract):
//
//	key      canonical settings key — the F2 context_settings namespace
//	env      env var name, or "-" for fields without an env source
//	default  raw default value, parsed by the same typed parser as env input
//	mut      hot | restart | coupled | coupled:embed-cache — F2 rejects
//	         settings-writes on restart-only keys instead of silently
//	         accepting them; coupled:embed-cache keys must flush
//	         context_embed_cache on change (the cache keys only on
//	         (prefix+text, model), not host/protocol — Auflage X2)
//	parse    strict (malformed value = boot abort, today's getEnvInt fatal
//	         paths) | safe/default (malformed value = WARN + default, today's
//	         getEnv*Safe paths). F2 may flip safe fields to strict via this
//	         flag once the UI validates input (decision §7.3: F1 no, F2 yes).
//	secret   fp (machine-generated keys: presence + sha256 fingerprint ≥
//	         fpMinLen, boot dump only) | presence (human-chosen values: never
//	         fingerprinted — an unsalted hash prefix would be an offline
//	         dictionary oracle)
//	superseded  lifetime marker: F3 context_backends rows replace these keys
//	         (bootstrap MUST read the effective snapshot, not raw env — X1)

// Config is the complete runtime configuration. One immutable value per
// generation; consumers take one snapshot per operation and pass values down
// as parameters.
type Config struct {
	Server    ServerConfig
	Chat      ChatConfig
	Fallback  FallbackConfig
	Embed     EmbedConfig
	Dream     DreamConfig
	Rerank    RerankConfig
	Graph     GraphConfig
	Query     QueryConfig
	Scheduler SchedulerConfig
	Pool      PoolConfig
	Events    EventsConfig
	LLMLog    LLMLogConfig
	WebChat   WebChatConfig

	// sources records the origin per registry key ("env" | "default"; F2 adds
	// "settings"). Written once by the loader, read-only afterwards.
	sources map[string]string
}

// ServerConfig is the restart-only process surface: DB connection + listener.
type ServerConfig struct {
	DB         string `key:"server.db" env:"CONTEXT_DB" default:"context_store" mut:"restart"`
	DBUser     string `key:"server.db_user" env:"CONTEXT_DB_USER" default:"context_user" mut:"restart"`
	DBPass     string `key:"server.db_password" env:"CONTEXT_DB_PASSWORD" default:"" mut:"restart" secret:"presence"`
	DBHost     string `key:"server.db_host" env:"CONTEXT_DB_HOST" default:"localhost" mut:"restart"`
	DBPort     int    `key:"server.db_port" env:"CONTEXT_DB_PORT" default:"5432" mut:"restart" parse:"strict"`
	DBSSL      string `key:"server.db_sslmode" env:"CONTEXT_DB_SSLMODE" default:"disable" mut:"restart"`
	ListenAddr string `key:"server.listen_addr" env:"LISTEN_ADDR" default:":8080" mut:"restart"`
}

// ChatConfig is the primary chat/synthesis backend tuple.
type ChatConfig struct {
	Host     string             `key:"chat.host" env:"CTX_CHAT_HOST" default:"http://localhost:11434" mut:"hot" superseded:"f3:context_backends"`
	APIKey   string             `key:"chat.api_key" env:"CTX_CHAT_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends"`
	Protocol backends.Protocol  `key:"chat.protocol" env:"CTX_CHAT_PROTOCOL" default:"ollama" mut:"hot" superseded:"f3:context_backends"`
	Model    string             `key:"chat.model" env:"CTX_CHAT_MODEL" default:"qwen3.5:9b" mut:"hot" superseded:"f3:context_backends"`
	NumCtx   int                `key:"chat.num_ctx" env:"CTX_CHAT_NUM_CTX" default:"0" mut:"hot" parse:"strict" superseded:"f3:context_backends"`
	Think    backends.ThinkMode `key:"chat.think" env:"CTX_CHAT_THINK" default:"false" mut:"hot" superseded:"f3:context_backends"`
}

// FallbackConfig is the emergency chat backend for query-path synthesis,
// engaged only on transport-level unavailability of the primary. Empty host =
// off. Model is inherited from the primary (today's semantics).
type FallbackConfig struct {
	Host     string            `key:"chat_fallback.host" env:"CTX_CHAT_FALLBACK_HOST" default:"" mut:"hot" superseded:"f3:context_backends"`
	APIKey   string            `key:"chat_fallback.api_key" env:"CTX_CHAT_FALLBACK_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends"`
	Protocol backends.Protocol `key:"chat_fallback.protocol" env:"CTX_CHAT_FALLBACK_PROTOCOL" default:"openai" mut:"hot" superseded:"f3:context_backends"`
	Timeout  time.Duration     `key:"chat_fallback.timeout" env:"CTX_CHAT_FALLBACK_TIMEOUT" default:"420" mut:"hot" superseded:"f3:context_backends"`
}

// EmbedConfig is the query-path embedding backend tuple. Model is
// mut:"coupled": changing it changes the vector space and requires a
// re-embed migration. Host/Protocol are coupled to the embed cache (X2).
type EmbedConfig struct {
	Host     string            `key:"embed.host" env:"CTX_EMBED_HOST" default:"http://localhost:11434" mut:"coupled:embed-cache" superseded:"f3:context_backends"`
	APIKey   string            `key:"embed.api_key" env:"CTX_EMBED_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends"`
	Protocol backends.Protocol `key:"embed.protocol" env:"CTX_EMBED_PROTOCOL" default:"ollama" mut:"coupled:embed-cache" superseded:"f3:context_backends"`
	Model    string            `key:"embed.model" env:"CTX_EMBED_MODEL" default:"qwen3-embedding:8b" mut:"coupled" superseded:"f3:context_backends"`
	NumCtx   int               `key:"embed.num_ctx" env:"CTX_EMBED_NUM_CTX" default:"0" mut:"hot" parse:"strict" superseded:"f3:context_backends"`
}

// DreamConfig is the dream pipeline: its own chat tuple (model/num_ctx/think
// inherit from chat when zero), an optional separate embed tuple (field-by-
// field inheritance from embed, credential boundary V12), and the back-off
// policy.
type DreamConfig struct {
	Enabled  bool               `key:"dream.enabled" env:"CTX_DREAM_ENABLED" default:"false" mut:"restart"`
	Host     string             `key:"dream.host" env:"CTX_DREAM_HOST" default:"http://localhost:11434" mut:"hot" superseded:"f3:context_backends"`
	APIKey   string             `key:"dream.api_key" env:"CTX_DREAM_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends"`
	Protocol backends.Protocol  `key:"dream.protocol" env:"CTX_DREAM_PROTOCOL" default:"ollama" mut:"hot" superseded:"f3:context_backends"`
	Model    string             `key:"dream.model" env:"CTX_DREAM_MODEL" default:"" mut:"hot" superseded:"f3:context_backends"`
	NumCtx   int                `key:"dream.num_ctx" env:"CTX_DREAM_NUM_CTX" default:"0" mut:"hot" parse:"strict" superseded:"f3:context_backends"`
	Think    backends.ThinkMode `key:"dream.think" env:"CTX_DREAM_THINK" default:"" mut:"hot" superseded:"f3:context_backends"`

	Embed DreamEmbedConfig

	IdleWait    time.Duration `key:"dream.idle_wait" env:"CTX_DREAM_IDLE_WAIT" default:"20" mut:"hot"`
	Parallelism int           `key:"dream.parallelism" env:"CTX_DREAM_PARALLELISM" default:"1" mut:"restart"`

	Backoff BackoffConfig
}

// DreamEmbedConfig is the optional separate dream embedding tuple. Empty
// fields inherit from EmbedConfig field by field (DreamEmbedBackend).
type DreamEmbedConfig struct {
	Host     string            `key:"dream_embed.host" env:"CTX_DREAM_EMBED_HOST" default:"" mut:"coupled:embed-cache" superseded:"f3:context_backends"`
	APIKey   string            `key:"dream_embed.api_key" env:"CTX_DREAM_EMBED_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends"`
	Protocol backends.Protocol `key:"dream_embed.protocol" env:"CTX_DREAM_EMBED_PROTOCOL" default:"" mut:"coupled:embed-cache" superseded:"f3:context_backends"`
	Model    string            `key:"dream_embed.model" env:"CTX_DREAM_EMBED_MODEL" default:"" mut:"coupled" superseded:"f3:context_backends"`
	NumCtx   int               `key:"dream_embed.num_ctx" env:"CTX_DREAM_EMBED_NUM_CTX" default:"0" mut:"hot" superseded:"f3:context_backends"`
}

// BackoffConfig is the re-dream back-off policy (curve by eval count).
type BackoffConfig struct {
	Mode        string  `key:"dream.backoff_mode" env:"CTX_DREAM_BACKOFF_MODE" default:"exp" mut:"hot"`
	Factor      float64 `key:"dream.backoff_factor" env:"CTX_DREAM_BACKOFF_FACTOR" default:"1.6" mut:"hot"`
	Grace       int     `key:"dream.backoff_grace" env:"CTX_DREAM_BACKOFF_GRACE" default:"0" mut:"hot"`
	CapHours    Hours   `key:"dream.backoff_cap" env:"CTX_DREAM_BACKOFF_CAP" default:"45d" mut:"hot"`
	MinHours    Hours   `key:"dream.backoff_min" env:"CTX_DREAM_BACKOFF_MIN" default:"12h" mut:"hot"`
	InertOffset int     `key:"dream.backoff_inert_offset" env:"CTX_DREAM_BACKOFF_INERT_OFFSET" default:"7" mut:"hot"`
}

// RerankConfig is the post-RRF rerank stage. Host empty = LLM-as-judge on the
// chat model; set = cross-encoder sidecar.
type RerankConfig struct {
	Enabled     bool    `key:"rerank.enabled" env:"CTX_RERANK_ENABLED" default:"false" mut:"hot"`
	Host        string  `key:"rerank.host" env:"CTX_RERANK_HOST" default:"" mut:"hot" superseded:"f3:context_backends"`
	APIKey      string  `key:"rerank.api_key" env:"CTX_RERANK_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends"`
	Model       string  `key:"rerank.model" env:"CTX_RERANK_MODEL" default:"" mut:"hot" superseded:"f3:context_backends"`
	MaxDocs     int     `key:"rerank.max_docs" env:"CTX_RERANK_MAX_DOCS" default:"50" mut:"hot"`
	BlendWeight float64 `key:"rerank.blend_weight" env:"CTX_RERANK_BLEND_WEIGHT" default:"1.0" mut:"hot"`
}

// GraphConfig is the dream-graph expansion stage (post-RRF 1-hop traversal).
type GraphConfig struct {
	Enabled                bool    `key:"graph.enabled" env:"CTX_GRAPH_EXPAND_ENABLED" default:"false" mut:"hot"`
	Directed               bool    `key:"graph.directed" env:"CTX_GRAPH_EXPAND_DIRECTED" default:"true" mut:"hot"`
	HopDepth               int     `key:"graph.hop_depth" env:"CTX_GRAPH_EXPAND_HOP_DEPTH" default:"1" mut:"hot"`
	SeedCount              int     `key:"graph.seed_count" env:"CTX_GRAPH_EXPAND_SEED_COUNT" default:"5" mut:"hot"`
	SeedScoreFloor         float64 `key:"graph.seed_score_floor" env:"CTX_GRAPH_EXPAND_SEED_SCORE_FLOOR" default:"0.5" mut:"hot"`
	PerSeedCap             int     `key:"graph.per_seed_cap" env:"CTX_GRAPH_EXPAND_PER_SEED_CAP" default:"3" mut:"hot"`
	MaxInjected            int     `key:"graph.max_injected" env:"CTX_GRAPH_EXPAND_MAX_INJECTED" default:"10" mut:"hot"`
	MinConfidence          float64 `key:"graph.min_confidence" env:"CTX_GRAPH_EXPAND_MIN_CONFIDENCE" default:"0.75" mut:"hot"`
	MinConfidenceRecurrent float64 `key:"graph.min_confidence_recurrent" env:"CTX_GRAPH_EXPAND_MIN_CONFIDENCE_RECURRENT" default:"0.8" mut:"hot"`
	BoostWeight            float64 `key:"graph.boost_weight" env:"CTX_GRAPH_EXPAND_BOOST_WEIGHT" default:"0.20" mut:"hot"`
	HubDamping             bool    `key:"graph.hub_damping" env:"CTX_GRAPH_EXPAND_HUB_DAMPING" default:"true" mut:"hot"`
	WeightTopical          float64 `key:"graph.weight_topical" env:"CTX_GRAPH_EXPAND_WEIGHT_TOPICAL" default:"0.5" mut:"hot"`
	WeightFactual          float64 `key:"graph.weight_factual" env:"CTX_GRAPH_EXPAND_WEIGHT_FACTUAL" default:"0.9" mut:"hot"`
	WeightCausal           float64 `key:"graph.weight_causal" env:"CTX_GRAPH_EXPAND_WEIGHT_CAUSAL" default:"0.9" mut:"hot"`
	WeightRecurrent        float64 `key:"graph.weight_recurrent" env:"CTX_GRAPH_EXPAND_WEIGHT_RECURRENT" default:"1.0" mut:"hot"`
	NewPlacementFrac       float64 `key:"graph.new_placement_frac" env:"CTX_GRAPH_EXPAND_NEW_PLACEMENT_FRAC" default:"0.6" mut:"hot"`
}

// QueryConfig is the query-path tuning surface: synthesis thresholds, prompt
// version, temporal timezone, rate limits.
type QueryConfig struct {
	ScoreThreshold     float64        `key:"query.score_threshold" env:"CTX_SCORE_THRESHOLD" default:"0.001" mut:"hot"`
	ConfidentThreshold float64        `key:"query.confident_threshold" env:"CTX_CONFIDENT_THRESHOLD" default:"0.008" mut:"hot"`
	PromptVersion      string         `key:"query.prompt_version" env:"CTX_PROMPT_VERSION" default:"v5.2" mut:"hot"`
	Timezone           *time.Location `key:"query.timezone" env:"CTX_TIMEZONE" default:"" mut:"hot" parse:"strict"`
	RateLimitWrite     int            `key:"query.rate_limit_write" env:"CTX_RATE_LIMIT_WRITE" default:"100" mut:"hot" parse:"strict"`
	RateLimitRead      int            `key:"query.rate_limit_read" env:"CTX_RATE_LIMIT_READ" default:"0" mut:"hot" parse:"strict"`
}

// SchedulerConfig is the background-pipeline scope surface.
// TODO(multi-tenant): home_scope/read_scopes are server-global today; in the
// multi-tenant line they become per-key/per-tenant resolution (F2 settings
// scope column + key home_scope), and this group degrades to defaults.
type SchedulerConfig struct {
	ReadScopes []string `key:"scheduler.read_scopes" env:"CTX_READ_SCOPES" default:"private,shared,work" mut:"hot"`
	// HomeScope has no env knob in F1 (hardcoded "private" until now); the
	// key-keyed loader makes it settings-fillable from F2 without a special
	// path (env:"-" skips the env source, not the lookup).
	HomeScope string `key:"scheduler.home_scope" env:"-" default:"private" mut:"hot"`
	// LLMLogRetentionDays is the age after which the background janitor NULLs
	// the prompt/response BODIES in context_llm_log. The telemetry row
	// (pipeline/model/tokens/cost/block_ids/backend/trust) survives — the
	// egress audit stays lossless, only the plaintext shadow corpus is dropped
	// (Body-NULLing, NOT a chunk drop; masterplan E4). 0 disables retention:
	// bodies are kept forever (operator opt-in). The 90-day default ships safe.
	LLMLogRetentionDays int `key:"scheduler.llmlog_retention_days" env:"CTX_LLMLOG_RETENTION_DAYS" default:"90" mut:"hot" parse:"strict"`
}

// EventsConfig is the status-collector / SSE timing surface (design 04 §3.6).
// Policy as data, same line gaming.active rides: a fronting-proxy change
// becomes a settings flip, not a rebuild. F4-W6 (the status collector) reads
// both keys today; F4-W7 (SSE) layers ping/cap keys on top of the same group.
type EventsConfig struct {
	// TickInterval is the collector base cadence: the cheap sources (health,
	// pool.Status, dream mode, gaming, llm-24h aggregate) refresh at most this
	// often, and GET /api/status serves the cache instead of rebuilding per
	// request — N pollers cost one refresh, not N.
	TickInterval time.Duration `key:"events.tick_interval" env:"CTX_EVENTS_TICK_INTERVAL" default:"5" mut:"hot"`
	// QueueStatsInterval decouples dream.QueueDepth — an O(n) full-scan CTE
	// over context_blocks with no covering index — from the base tick. The
	// queue forecast's smallest window is 1h, so 30s is fresh enough and the
	// scan never rides the 5s cadence. 1M+ follow-up (partial index /
	// maintained counters) is named in design 04 §3.6 / R12.
	QueueStatsInterval time.Duration `key:"events.queue_stats_interval" env:"CTX_EVENTS_QUEUE_STATS_INTERVAL" default:"30" mut:"hot"`
	// PingInterval is the SSE keepalive cadence (a ": ping" comment line). It
	// MUST stay below the fronting proxy's read timeout (nginx
	// proxy_read_timeout, currently 60s) — an idle stream between diffs is
	// dropped otherwise. Policy-as-data: a proxy retune is a settings flip, not
	// a rebuild; 25s leaves ~2 pings inside a 60s window. F4-W7 (GET
	// /api/events) only; the query heartbeat shares the same 25s rationale.
	PingInterval time.Duration `key:"events.ping_interval" env:"CTX_EVENTS_PING_INTERVAL" default:"25" mut:"hot"`
	// MaxConnections caps concurrent SSE streams; the admin-only endpoint
	// answers 429 above it and the client degrades to polling. The default is
	// sized for the 256M container — autonomous agents / friend-tenant panels
	// (O5) raise it via a settings flip, not a redeploy. parse:"strict": a
	// malformed cap is an operator typo worth a loud boot abort (same call as
	// llmlog.max_limit), not a silent fall-back that hides the intended ceiling.
	MaxConnections int `key:"events.max_connections" env:"CTX_EVENTS_MAX_CONNECTIONS" default:"8" mut:"hot" parse:"strict"`
}

// LLMLogConfig is the telemetry read surface. The retention knob (body-NULLing
// after N days) lives under scheduler.* because it is a janitor job; this is
// the per-request read cap for GET /api/llmlog.
type LLMLogConfig struct {
	// MaxLimit caps GET /api/llmlog?limit=. The endpoint is admin-only and
	// rides the created_at DESC hypertable path; 200 keeps one page bounded
	// without a full chunk walk.
	MaxLimit int `key:"llmlog.max_limit" env:"CTX_LLMLOG_MAX_LIMIT" default:"200" mut:"hot" parse:"strict"`
}

// WebChatConfig is the F6 web-chat harness surface (design 06 §3.2). All knobs
// are hot — a turn reads a fresh config snapshot at start, so a settings flip
// hits the next turn, not the running one. The integer BUDGETS stay non-strict
// (a bad value falls back to the default, and the engine's withDefaults() is a
// second net) — only ConcurrentTurns is parse:"strict": it is a per-tenant
// fairness ceiling like events.max_connections, and a typo'd cap that silently
// fell back to the default would hide the intended limit on the single
// llama.cpp slot (R1).
type WebChatConfig struct {
	// Enabled gates POST /api/chat/stream + the session routes; off ⇒ 404 (the
	// SPA route reads the /health feature bit and hides itself).
	Enabled bool `key:"webchat.enabled" env:"CTX_WEBCHAT_ENABLED" default:"true" mut:"hot"`
	// MaxIterations caps the tool loop per turn; one closing call WITHOUT tools
	// follows the cap (E4 — never tool_choice:none).
	MaxIterations int `key:"webchat.max_iterations" env:"CTX_WEBCHAT_MAX_ITERATIONS" default:"6" mut:"hot"`
	// MaxTokens clamps max_tokens per model call (a request may ask less, never
	// more); the per-backend limits.chat_max_tokens clamp applies on top (CPU 512).
	MaxTokens int `key:"webchat.max_tokens" env:"CTX_WEBCHAT_MAX_TOKENS" default:"2048" mut:"hot"`
	// CompletionBudget caps Σ completion_tokens across all iterations of one turn.
	CompletionBudget int `key:"webchat.completion_budget" env:"CTX_WEBCHAT_COMPLETION_BUDGET" default:"8192" mut:"hot"`
	// ToolResultMaxChars truncates one tool result (ctx_get pages the rest via
	// the offset marker so a >window block stays fully readable).
	ToolResultMaxChars int `key:"webchat.tool_result_max_chars" env:"CTX_WEBCHAT_TOOL_RESULT_MAX_CHARS" default:"8000" mut:"hot"`
	// HistoryBudgetChars bounds the session history fed into the prompt (~15k
	// tokens); older tool results condense, then oldest messages drop (§3.6).
	HistoryBudgetChars int `key:"webchat.history_budget_chars" env:"CTX_WEBCHAT_HISTORY_BUDGET_CHARS" default:"60000" mut:"hot"`
	// LLMTimeout bounds one model call (bare seconds; CPU-fallback worst case is
	// why it is large — the per-backend MaxTokens clamp keeps that bounded).
	LLMTimeout time.Duration `key:"webchat.llm_timeout" env:"CTX_WEBCHAT_LLM_TIMEOUT" default:"900" mut:"hot"`
	// ConcurrentTurns caps simultaneously running turns per home_scope (the
	// in-memory semaphore §3.3); above it the handler answers 429 before stream
	// start. Bounds the turn FREQUENCY per tenant, not just one turn's budget (R1).
	ConcurrentTurns int `key:"webchat.concurrent_turns" env:"CTX_WEBCHAT_CONCURRENT_TURNS" default:"1" mut:"hot" parse:"strict"`
	// SessionRetention enables the retention janitor: sessions whose updated_at
	// is older than this are deleted (messages cascade). 0 = off (kept forever,
	// the shipped default). Duration suffix h/d/w/m/y (Hours parser, since v2.6.0).
	SessionRetention Hours `key:"webchat.session_retention" env:"CTX_WEBCHAT_SESSION_RETENTION" default:"0" mut:"hot"`
}

// ScopeFloor maps a scope to its minimum effective sensitivity (F3 §2.3d).
// The floor only RAISES — effective = max(block.sensitivity, floor[scope]) —
// so friend-tenant and future workflow/issue scopes are blanket-protectable
// without block mutation. Loader builds a fresh map per generation; consumers
// are read-only (immutability convention above).
type ScopeFloor map[string]backends.Sensitivity

// Apply returns the floor-adjusted sensitivity for a block of the given
// scope. No floor entry = unchanged; an entry can only RAISE (monotone —
// MaxSensitivity never lowers).
func (f ScopeFloor) Apply(s backends.Sensitivity, scope string) backends.Sensitivity {
	if min, ok := f[scope]; ok {
		return backends.MaxSensitivity(s, min)
	}
	return s
}

// PoolConfig is the F3 trust-gating policy surface (settings-only, no env
// source — these keys are born in F2, not migrated from env vars).
// The default keys are guard:"sensitivity-downgrade": LOWERING them needs a
// confirm flag in the settings write (one wrong F4 dropdown / CLI typo on
// 'public' would silently mark ALL new unclassified blocks external-eligible
// until the first failover — F3 §3.5).
type PoolConfig struct {
	DefaultQuerySensitivity backends.Sensitivity `key:"pool.default_query_sensitivity" env:"-" default:"personal" mut:"hot" guard:"sensitivity-downgrade"`
	DefaultBlockSensitivity backends.Sensitivity `key:"pool.default_block_sensitivity" env:"-" default:"credentials" mut:"hot" guard:"sensitivity-downgrade"`
	// TODO(multi-tenant): the floor is server-global policy; the multi-tenant
	// line scopes it per tenant (settings scope column) so each tenant floors
	// its own scopes.
	ScopeSensitivityFloor ScopeFloor `key:"pool.scope_sensitivity_floor" env:"-" default:"{}" mut:"hot"`

	// GamingActive flips the named GPU-host backends out of EVERY chain so the
	// GPU is free to game. It lives in the settings layer (persistent across
	// restarts) — NOT an atomic: the dream-mode break path (restart ⇒ the GPU
	// lock is gone) is the explicit anti-pattern here (design 03 §2.6).
	// Toggled via `ctx gaming on|off` / the gaming-mode manage action.
	GamingActive bool `key:"gaming.active" env:"-" default:"false" mut:"hot"`
	// GamingDisabledBackends names which backends gaming.active excludes —
	// policy as data, so a second GPU host later is a list edit, not code.
	// Default = the herbert GPU backends; the CPU/external rows stay in as
	// failover. Comma-split (scopes parser); the gaming-mode action validates
	// the names against the live pool (a typo ⇒ unknown_backends, risk 6.6).
	GamingDisabledBackends []string `key:"gaming.disabled_backends" env:"-" default:"herbert-chat,herbert-rerank" mut:"hot"`
}

// GamingState returns the chain-time gaming exclusion from THIS settings
// snapshot (design 03 §2.6). Callers pass it to Pool.Chain — the pool holds
// no policy (deliberate decoupling, backends/pool.go): the toggle takes
// effect on the next chain that reads a fresh config snapshot, no restart.
func (c *Config) GamingState() backends.GamingState {
	return backends.GamingState{
		Active:           c.Pool.GamingActive,
		DisabledBackends: c.Pool.GamingDisabledBackends,
	}
}

// Source reports the origin of a registry key in this snapshot:
// "env" | "default" (F2 adds "settings"). Unknown keys return "".
func (c *Config) Source(key string) string {
	return c.sources[key]
}

// ChatBackend returns the primary chat tuple, 1:1 from Chat.*.
func (c *Config) ChatBackend() backends.Backend {
	return backends.Backend{
		Host:     c.Chat.Host,
		APIKey:   c.Chat.APIKey,
		Protocol: c.Chat.Protocol,
		Model:    c.Chat.Model,
		NumCtx:   c.Chat.NumCtx,
		Think:    c.Chat.Think,
	}
}

// ChatFallbackBackend returns the emergency synthesis backend, or nil when no
// fallback host is configured. Model stays empty = inherit the primary model
// at the call site (today's chatWithFallback semantics).
func (c *Config) ChatFallbackBackend() *backends.Backend {
	if c.Fallback.Host == "" {
		return nil
	}
	return &backends.Backend{
		Host:     c.Fallback.Host,
		APIKey:   c.Fallback.APIKey,
		Protocol: c.Fallback.Protocol,
		Timeout:  c.Fallback.Timeout,
	}
}

// DreamBackend returns the dream chat tuple with the inheritance chain
// resolved: Model/Think inherit from chat when empty, NumCtx inherits from
// chat when 0 (Delta 1 — unified with the daily-synthesis derivation; V1
// guards the divergent-NumCtx dual-runner case).
func (c *Config) DreamBackend() backends.Backend {
	b := backends.Backend{
		Host:     c.Dream.Host,
		APIKey:   c.Dream.APIKey,
		Protocol: c.Dream.Protocol,
		Model:    c.Dream.Model,
		NumCtx:   c.Dream.NumCtx,
		Think:    c.Dream.Think,
	}
	if b.Model == "" {
		b.Model = c.Chat.Model
	}
	if b.NumCtx == 0 {
		b.NumCtx = c.Chat.NumCtx
	}
	if b.Think == "" {
		b.Think = c.Chat.Think
	}
	return b
}

// DreamEmbedBackend returns the dream embedding tuple with field-by-field
// fallback onto Embed.* (today's scheduler semantics). The cross-host
// credential case — APIKey inheriting although Host does not — is rejected by
// V12 at validation time instead of silently changing the inheritance here.
func (c *Config) DreamEmbedBackend() backends.Backend {
	b := backends.Backend{
		Host:     c.Dream.Embed.Host,
		APIKey:   c.Dream.Embed.APIKey,
		Protocol: c.Dream.Embed.Protocol,
		Model:    c.Dream.Embed.Model,
		NumCtx:   c.Dream.Embed.NumCtx,
	}
	if b.Host == "" {
		b.Host = c.Embed.Host
	}
	if b.APIKey == "" {
		b.APIKey = c.Embed.APIKey
	}
	if b.Protocol == "" {
		b.Protocol = c.Embed.Protocol
	}
	if b.Model == "" {
		b.Model = c.Embed.Model
	}
	if b.NumCtx == 0 {
		b.NumCtx = c.Embed.NumCtx
	}
	return b
}

// EmbedBackend returns the query-path embedding tuple, 1:1 from Embed.*.
func (c *Config) EmbedBackend() backends.Backend {
	return backends.Backend{
		Host:     c.Embed.Host,
		APIKey:   c.Embed.APIKey,
		Protocol: c.Embed.Protocol,
		Model:    c.Embed.Model,
		NumCtx:   c.Embed.NumCtx,
	}
}

// RerankRRF converts the rerank group to the rrf-stage parameter struct.
func (c *Config) RerankRRF() rrf.RerankConfig {
	return rrf.RerankConfig{
		Enabled:     c.Rerank.Enabled,
		Host:        c.Rerank.Host,
		APIKey:      c.Rerank.APIKey,
		Model:       c.Rerank.Model,
		MaxDocs:     c.Rerank.MaxDocs,
		BlendWeight: c.Rerank.BlendWeight,
	}
}

// GraphRRF converts the graph group to the rrf-stage parameter struct.
func (c *Config) GraphRRF() rrf.GraphConfig {
	return rrf.GraphConfig{
		Enabled:                c.Graph.Enabled,
		Directed:               c.Graph.Directed,
		HopDepth:               c.Graph.HopDepth,
		SeedCount:              c.Graph.SeedCount,
		SeedScoreFloor:         c.Graph.SeedScoreFloor,
		PerSeedCap:             c.Graph.PerSeedCap,
		MaxInjected:            c.Graph.MaxInjected,
		MinConfidence:          c.Graph.MinConfidence,
		MinConfidenceRecurrent: c.Graph.MinConfidenceRecurrent,
		BoostWeight:            c.Graph.BoostWeight,
		HubDamping:             c.Graph.HubDamping,
		WeightTopical:          c.Graph.WeightTopical,
		WeightFactual:          c.Graph.WeightFactual,
		WeightCausal:           c.Graph.WeightCausal,
		WeightRecurrent:        c.Graph.WeightRecurrent,
		NewPlacementFrac:       c.Graph.NewPlacementFrac,
	}
}

// DreamBackoff converts the dream back-off group to the dream-stage parameter
// struct (same converter pattern as RerankRRF/GraphRRF). Both consumers — the
// scheduler per cycle and the ManageHandler dream-stats per request — derive
// from their snapshot through this one method, so the policy the cycles run
// and the policy /api/manage renders are always the same generation (F1-W6:
// the 6 dream package vars + SetBackoffConfig died in favor of this).
func (c *Config) DreamBackoff() dream.BackoffConfig {
	return dream.BackoffConfig{
		Mode:        c.Dream.Backoff.Mode,
		Factor:      c.Dream.Backoff.Factor,
		Grace:       c.Dream.Backoff.Grace,
		MinHours:    float64(c.Dream.Backoff.MinHours),
		CapHours:    float64(c.Dream.Backoff.CapHours),
		InertOffset: c.Dream.Backoff.InertOffset,
	}
}

// SynthesisSettings converts the query group's scoring surface to the llm
// synthesis parameter struct (F1-W2 introduced the parameter; F1-W4 moves the
// derivation onto the snapshot — one source instead of the cmd/ctxd bridge
// copy).
func (c *Config) SynthesisSettings() llm.SynthesisSettings {
	return llm.SynthesisSettings{
		ScoreThreshold:     c.Query.ScoreThreshold,
		ConfidentThreshold: c.Query.ConfidentThreshold,
		PromptVersion:      c.Query.PromptVersion,
	}
}

// DSN returns the PostgreSQL connection string. User and password are
// URL-encoded to handle special characters.
func (c *Config) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(c.Server.DBUser),
		url.QueryEscape(c.Server.DBPass),
		c.Server.DBHost,
		c.Server.DBPort,
		c.Server.DB,
		c.Server.DBSSL,
	)
}
