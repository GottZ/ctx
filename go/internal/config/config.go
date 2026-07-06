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
//	tenancy  tenant-overridable | global-only (MANDATORY — MT3-W2): may a
//	         tenant override this key on top of _global, or does it live in
//	         _global only? A missing/unknown value is a boot panic, so no key
//	         escapes the classification. Classification rule: a key is
//	         tenant-overridable only when overriding it per-tenant touches
//	         NOTHING process-shared — it affects solely that tenant's own
//	         query/chat/dream/policy resolution (retrieval tuning, rate limits,
//	         scope/sensitivity policy, per-tenant chat budgets, the provider
//	         api_key secret_refs). Everything that touches a host/physical or
//	         process-wide resource is global-only: the DSN/listener (restart),
//	         the backend HOST/MODEL/PROTOCOL tuples (topology — per-tenant
//	         backends come from the F3 pool's scope dimension, not these legacy
//	         keys), the embed-cache-coupled keys (a tenant flush nukes the
//	         shared cache — R-SCALE6), gaming.active (GPU is one host), the
//	         scheduler cadences and offline jobs, and the server egress-audit
//	         retention. Fail-closed default for any NEW key: global-only.

// Config is the complete runtime configuration. One immutable value per
// generation; consumers take one snapshot per operation and pass values down
// as parameters.
type Config struct {
	Server        ServerConfig
	Chat          ChatConfig
	Fallback      FallbackConfig
	Embed         EmbedConfig
	Dream         DreamConfig
	Rerank        RerankConfig
	Graph         GraphConfig
	GraphOverview GraphOverviewConfig
	Query         QueryConfig
	Scheduler     SchedulerConfig
	Pool          PoolConfig
	Events        EventsConfig
	Project       ProjectConfig
	LLMLog        LLMLogConfig
	WebChat       WebChatConfig
	Writes        WritesConfig
	Tenant        TenantConfig
	Dispatch      DispatchConfig

	// sources records the origin per registry key ("env" | "default"; F2 adds
	// "settings"). Written once by the loader, read-only afterwards.
	sources map[string]string
}

// ServerConfig is the restart-only process surface: DB connection + listener.
type ServerConfig struct {
	DB         string `key:"server.db" env:"CONTEXT_DB" default:"context_store" mut:"restart" tenancy:"global-only"`
	DBUser     string `key:"server.db_user" env:"CONTEXT_DB_USER" default:"context_user" mut:"restart" tenancy:"global-only"`
	DBPass     string `key:"server.db_password" env:"CONTEXT_DB_PASSWORD" default:"" mut:"restart" secret:"presence" tenancy:"global-only"`
	DBHost     string `key:"server.db_host" env:"CONTEXT_DB_HOST" default:"localhost" mut:"restart" tenancy:"global-only"`
	DBPort     int    `key:"server.db_port" env:"CONTEXT_DB_PORT" default:"5432" mut:"restart" parse:"strict" tenancy:"global-only"`
	DBSSL      string `key:"server.db_sslmode" env:"CONTEXT_DB_SSLMODE" default:"disable" mut:"restart" tenancy:"global-only"`
	ListenAddr string `key:"server.listen_addr" env:"LISTEN_ADDR" default:":8080" mut:"restart" tenancy:"global-only"`
}

// ChatConfig is the primary chat/synthesis backend tuple.
type ChatConfig struct {
	Host string `key:"chat.host" env:"CTX_CHAT_HOST" default:"http://localhost:11434" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	// TENANT-DECISION(provider-api-key): the 6 provider api_key secret_refs
	// (chat/chat_fallback/embed/dream/dream_embed/rerank) are tenant-overridable
	// — a tenant brings its own provider credential (resolved per-tenant by the
	// 03-W3..W5 secret resolver, isolation gated by tenant.allow_shared_secrets
	// §4.3). Alt: global-only with credentials flowing ONLY through the F3 pool's
	// scope+AAD path; umentscheidbar because the per-tenant secret resolver
	// (W3-W5) is not built yet and may consolidate on the pool. §3.3 lists these
	// six as tenant-overridable; the HOST/MODEL topology stays global (pool owns
	// per-tenant backends). Pausable: no consumer until W3.
	APIKey   string             `key:"chat.api_key" env:"CTX_CHAT_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends" tenancy:"tenant-overridable"`
	Protocol backends.Protocol  `key:"chat.protocol" env:"CTX_CHAT_PROTOCOL" default:"ollama" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	Model    string             `key:"chat.model" env:"CTX_CHAT_MODEL" default:"qwen3.5:9b" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	NumCtx   int                `key:"chat.num_ctx" env:"CTX_CHAT_NUM_CTX" default:"0" mut:"hot" parse:"strict" superseded:"f3:context_backends" tenancy:"global-only"`
	Think    backends.ThinkMode `key:"chat.think" env:"CTX_CHAT_THINK" default:"false" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
}

// FallbackConfig is the emergency chat backend for query-path synthesis,
// engaged only on transport-level unavailability of the primary. Empty host =
// off. Model is inherited from the primary (today's semantics).
type FallbackConfig struct {
	Host     string            `key:"chat_fallback.host" env:"CTX_CHAT_FALLBACK_HOST" default:"" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	APIKey   string            `key:"chat_fallback.api_key" env:"CTX_CHAT_FALLBACK_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends" tenancy:"tenant-overridable"`
	Protocol backends.Protocol `key:"chat_fallback.protocol" env:"CTX_CHAT_FALLBACK_PROTOCOL" default:"openai" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	Timeout  time.Duration     `key:"chat_fallback.timeout" env:"CTX_CHAT_FALLBACK_TIMEOUT" default:"420" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
}

// EmbedConfig is the query-path embedding backend tuple. Model is
// mut:"coupled": changing it changes the vector space and requires a
// re-embed migration. Host/Protocol are coupled to the embed cache (X2).
type EmbedConfig struct {
	// embed.host/protocol are NAMED global-only (not just by fail-closed default):
	// they are mut:"coupled:embed-cache", and a tenant override would change the
	// effective embed tuple → embedcache.Flush nukes the process-wide, scope-less
	// context_embed_cache for ALL tenants (R-SCALE6, cosine 0.997). Model stays
	// global-only too (vector space — re-embed migration, not overridable).
	Host     string            `key:"embed.host" env:"CTX_EMBED_HOST" default:"http://localhost:11434" mut:"coupled:embed-cache" superseded:"f3:context_backends" tenancy:"global-only"`
	APIKey   string            `key:"embed.api_key" env:"CTX_EMBED_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends" tenancy:"tenant-overridable"`
	Protocol backends.Protocol `key:"embed.protocol" env:"CTX_EMBED_PROTOCOL" default:"ollama" mut:"coupled:embed-cache" superseded:"f3:context_backends" tenancy:"global-only"`
	Model    string            `key:"embed.model" env:"CTX_EMBED_MODEL" default:"qwen3-embedding:8b" mut:"coupled" superseded:"f3:context_backends" tenancy:"global-only"`
	NumCtx   int               `key:"embed.num_ctx" env:"CTX_EMBED_NUM_CTX" default:"0" mut:"hot" parse:"strict" superseded:"f3:context_backends" tenancy:"global-only"`
}

// DreamConfig is the dream pipeline: its own chat tuple (model/num_ctx/think
// inherit from chat when zero), an optional separate embed tuple (field-by-
// field inheritance from embed, credential boundary V12), and the back-off
// policy.
type DreamConfig struct {
	Enabled  bool               `key:"dream.enabled" env:"CTX_DREAM_ENABLED" default:"false" mut:"restart" tenancy:"global-only"`
	Host     string             `key:"dream.host" env:"CTX_DREAM_HOST" default:"http://localhost:11434" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	APIKey   string             `key:"dream.api_key" env:"CTX_DREAM_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends" tenancy:"tenant-overridable"`
	Protocol backends.Protocol  `key:"dream.protocol" env:"CTX_DREAM_PROTOCOL" default:"ollama" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	Model    string             `key:"dream.model" env:"CTX_DREAM_MODEL" default:"" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	NumCtx   int                `key:"dream.num_ctx" env:"CTX_DREAM_NUM_CTX" default:"0" mut:"hot" parse:"strict" superseded:"f3:context_backends" tenancy:"global-only"`
	Think    backends.ThinkMode `key:"dream.think" env:"CTX_DREAM_THINK" default:"" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`

	Embed DreamEmbedConfig

	// idle_wait (scheduler cadence) and parallelism (process-wide worker count,
	// restart) are background-pipeline infrastructure — global-only.
	IdleWait    time.Duration `key:"dream.idle_wait" env:"CTX_DREAM_IDLE_WAIT" default:"20" mut:"hot" tenancy:"global-only"`
	Parallelism int           `key:"dream.parallelism" env:"CTX_DREAM_PARALLELISM" default:"1" mut:"restart" tenancy:"global-only"`

	Backoff BackoffConfig
}

// DreamEmbedConfig is the optional separate dream embedding tuple. Empty
// fields inherit from EmbedConfig field by field (DreamEmbedBackend).
type DreamEmbedConfig struct {
	// dream_embed.host/protocol are NAMED global-only (embed-cache coupled, same
	// R-SCALE6 shared-cache flush as embed.host/protocol).
	Host     string            `key:"dream_embed.host" env:"CTX_DREAM_EMBED_HOST" default:"" mut:"coupled:embed-cache" superseded:"f3:context_backends" tenancy:"global-only"`
	APIKey   string            `key:"dream_embed.api_key" env:"CTX_DREAM_EMBED_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends" tenancy:"tenant-overridable"`
	Protocol backends.Protocol `key:"dream_embed.protocol" env:"CTX_DREAM_EMBED_PROTOCOL" default:"" mut:"coupled:embed-cache" superseded:"f3:context_backends" tenancy:"global-only"`
	Model    string            `key:"dream_embed.model" env:"CTX_DREAM_EMBED_MODEL" default:"" mut:"coupled" superseded:"f3:context_backends" tenancy:"global-only"`
	NumCtx   int               `key:"dream_embed.num_ctx" env:"CTX_DREAM_EMBED_NUM_CTX" default:"0" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
}

// BackoffConfig is the re-dream back-off policy (curve by eval count).
type BackoffConfig struct {
	// The re-dream back-off curve is an atomic per-tenant unit: it shapes when a
	// tenant's own blocks are re-dreamed (the scheduler reads it per block), no
	// cross-tenant effect — tenant-overridable as a group (mode §3.3-listed).
	Mode        string  `key:"dream.backoff_mode" env:"CTX_DREAM_BACKOFF_MODE" default:"exp" mut:"hot" tenancy:"tenant-overridable"`
	Factor      float64 `key:"dream.backoff_factor" env:"CTX_DREAM_BACKOFF_FACTOR" default:"1.6" mut:"hot" tenancy:"tenant-overridable"`
	Grace       int     `key:"dream.backoff_grace" env:"CTX_DREAM_BACKOFF_GRACE" default:"0" mut:"hot" tenancy:"tenant-overridable"`
	CapHours    Hours   `key:"dream.backoff_cap" env:"CTX_DREAM_BACKOFF_CAP" default:"45d" mut:"hot" tenancy:"tenant-overridable"`
	MinHours    Hours   `key:"dream.backoff_min" env:"CTX_DREAM_BACKOFF_MIN" default:"12h" mut:"hot" tenancy:"tenant-overridable"`
	InertOffset int     `key:"dream.backoff_inert_offset" env:"CTX_DREAM_BACKOFF_INERT_OFFSET" default:"7" mut:"hot" tenancy:"tenant-overridable"`
}

// RerankConfig is the post-RRF rerank stage. Host empty = LLM-as-judge on the
// chat model; set = cross-encoder sidecar.
type RerankConfig struct {
	// Per-tenant rerank tuning (enabled/max_docs/blend_weight — blend_weight
	// §3.3-listed) is query-time, isolation-safe. host/model are backend topology
	// (superseded by the F3 pool's scope dimension) → global-only.
	Enabled     bool    `key:"rerank.enabled" env:"CTX_RERANK_ENABLED" default:"false" mut:"hot" tenancy:"tenant-overridable"`
	Host        string  `key:"rerank.host" env:"CTX_RERANK_HOST" default:"" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	APIKey      string  `key:"rerank.api_key" env:"CTX_RERANK_API_KEY" default:"" mut:"hot" secret:"fp" superseded:"f3:context_backends" tenancy:"tenant-overridable"`
	Model       string  `key:"rerank.model" env:"CTX_RERANK_MODEL" default:"" mut:"hot" superseded:"f3:context_backends" tenancy:"global-only"`
	MaxDocs     int     `key:"rerank.max_docs" env:"CTX_RERANK_MAX_DOCS" default:"50" mut:"hot" tenancy:"tenant-overridable"`
	BlendWeight float64 `key:"rerank.blend_weight" env:"CTX_RERANK_BLEND_WEIGHT" default:"1.0" mut:"hot" tenancy:"tenant-overridable"`
}

// GraphConfig is the dream-graph expansion stage (post-RRF 1-hop traversal).
type GraphConfig struct {
	// All graph-expansion knobs are query-time RRF augmentation tuning — a tenant
	// tuning its own expansion affects only its own queries, zero cross-tenant
	// effect → tenant-overridable as a group.
	Enabled                bool    `key:"graph.enabled" env:"CTX_GRAPH_EXPAND_ENABLED" default:"false" mut:"hot" tenancy:"tenant-overridable"`
	Directed               bool    `key:"graph.directed" env:"CTX_GRAPH_EXPAND_DIRECTED" default:"true" mut:"hot" tenancy:"tenant-overridable"`
	HopDepth               int     `key:"graph.hop_depth" env:"CTX_GRAPH_EXPAND_HOP_DEPTH" default:"1" mut:"hot" tenancy:"tenant-overridable"`
	SeedCount              int     `key:"graph.seed_count" env:"CTX_GRAPH_EXPAND_SEED_COUNT" default:"5" mut:"hot" tenancy:"tenant-overridable"`
	SeedScoreFloor         float64 `key:"graph.seed_score_floor" env:"CTX_GRAPH_EXPAND_SEED_SCORE_FLOOR" default:"0.5" mut:"hot" tenancy:"tenant-overridable"`
	PerSeedCap             int     `key:"graph.per_seed_cap" env:"CTX_GRAPH_EXPAND_PER_SEED_CAP" default:"3" mut:"hot" tenancy:"tenant-overridable"`
	MaxInjected            int     `key:"graph.max_injected" env:"CTX_GRAPH_EXPAND_MAX_INJECTED" default:"10" mut:"hot" tenancy:"tenant-overridable"`
	MinConfidence          float64 `key:"graph.min_confidence" env:"CTX_GRAPH_EXPAND_MIN_CONFIDENCE" default:"0.75" mut:"hot" tenancy:"tenant-overridable"`
	MinConfidenceRecurrent float64 `key:"graph.min_confidence_recurrent" env:"CTX_GRAPH_EXPAND_MIN_CONFIDENCE_RECURRENT" default:"0.8" mut:"hot" tenancy:"tenant-overridable"`
	BoostWeight            float64 `key:"graph.boost_weight" env:"CTX_GRAPH_EXPAND_BOOST_WEIGHT" default:"0.20" mut:"hot" tenancy:"tenant-overridable"`
	HubDamping             bool    `key:"graph.hub_damping" env:"CTX_GRAPH_EXPAND_HUB_DAMPING" default:"true" mut:"hot" tenancy:"tenant-overridable"`
	WeightTopical          float64 `key:"graph.weight_topical" env:"CTX_GRAPH_EXPAND_WEIGHT_TOPICAL" default:"0.5" mut:"hot" tenancy:"tenant-overridable"`
	WeightFactual          float64 `key:"graph.weight_factual" env:"CTX_GRAPH_EXPAND_WEIGHT_FACTUAL" default:"0.9" mut:"hot" tenancy:"tenant-overridable"`
	WeightCausal           float64 `key:"graph.weight_causal" env:"CTX_GRAPH_EXPAND_WEIGHT_CAUSAL" default:"0.9" mut:"hot" tenancy:"tenant-overridable"`
	WeightRecurrent        float64 `key:"graph.weight_recurrent" env:"CTX_GRAPH_EXPAND_WEIGHT_RECURRENT" default:"1.0" mut:"hot" tenancy:"tenant-overridable"`
	NewPlacementFrac       float64 `key:"graph.new_placement_frac" env:"CTX_GRAPH_EXPAND_NEW_PLACEMENT_FRAC" default:"0.6" mut:"hot" tenancy:"tenant-overridable"`
}

// GraphOverviewConfig is the F5-W6 landkarte rebuild job (precomputed Louvain
// cluster supergraph, design 07-graph-overview.md). Distinct from GraphConfig
// (the RRF query-time 1-hop expansion) — same graph domain, separate surface.
// The rebuild runs offline in the scheduler; the read endpoint (W2) gates on
// Enabled too. RebuildInterval default is seconds (6h), like the other
// duration keys.
type GraphOverviewConfig struct {
	// The landkarte is an offline, process-global supergraph rebuilt by the
	// scheduler over the whole corpus — not per-tenant today → global-only.
	// (The per-tenant rebuild loop is under construction on the B line; the
	// rebuild POLICY knobs below stay server-global even then.)
	Enabled         bool          `key:"graph_overview.enabled" env:"CTX_GRAPH_OVERVIEW_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`
	RebuildInterval time.Duration `key:"graph_overview.rebuild_interval" env:"CTX_GRAPH_OVERVIEW_REBUILD_INTERVAL" default:"21600" mut:"hot" tenancy:"global-only"`
	Resolution      float64       `key:"graph_overview.resolution" env:"CTX_GRAPH_OVERVIEW_RESOLUTION" default:"1.0" mut:"hot" tenancy:"global-only"`
	// MaxNodes is the load-bearing liveness guard (B-W1/B2-C1): Louvain past
	// ~200k nodes hits a convergence wall (bench 019ec56f); a larger node set
	// skips the rebuild fail-safe. 0 = uncapped.
	MaxNodes int `key:"graph_overview.max_nodes" env:"CTX_GRAPH_OVERVIEW_MAX_NODES" default:"200000" mut:"hot" parse:"strict" tenancy:"global-only"`
	// RebuildTimeout bounds one rebuild run (secondary liveness guard, B-W1).
	// Seconds, like the other duration keys. A running Modularize cannot be
	// interrupted — the timeout abandons it (documented goroutine leak) and
	// keeps the scheduler loop alive. 0 → 900s.
	RebuildTimeout time.Duration `key:"graph_overview.rebuild_timeout" env:"CTX_GRAPH_OVERVIEW_REBUILD_TIMEOUT" default:"900" mut:"hot" tenancy:"global-only"`
}

// QueryConfig is the query-path tuning surface: synthesis thresholds, prompt
// version, temporal timezone, rate limits.
type QueryConfig struct {
	// Query-path tuning is per-tenant request behavior (synthesis thresholds,
	// prompt version, temporal timezone, rate limits — score_threshold and the
	// rate limits §3.3-listed); each affects only that tenant's own queries.
	ScoreThreshold     float64        `key:"query.score_threshold" env:"CTX_SCORE_THRESHOLD" default:"0.001" mut:"hot" tenancy:"tenant-overridable"`
	ConfidentThreshold float64        `key:"query.confident_threshold" env:"CTX_CONFIDENT_THRESHOLD" default:"0.008" mut:"hot" tenancy:"tenant-overridable"`
	PromptVersion      string         `key:"query.prompt_version" env:"CTX_PROMPT_VERSION" default:"v5.2" mut:"hot" tenancy:"tenant-overridable"`
	Timezone           *time.Location `key:"query.timezone" env:"CTX_TIMEZONE" default:"" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	RateLimitWrite     int            `key:"query.rate_limit_write" env:"CTX_RATE_LIMIT_WRITE" default:"100" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	RateLimitRead      int            `key:"query.rate_limit_read" env:"CTX_RATE_LIMIT_READ" default:"0" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
}

// SchedulerConfig is the background-pipeline scope surface.
// TODO(multi-tenant): home_scope/read_scopes are server-global today; in the
// multi-tenant line they become per-key/per-tenant resolution (F2 settings
// scope column + key home_scope), and this group degrades to defaults.
type SchedulerConfig struct {
	// TENANT-DECISION(scheduler-scope): read_scopes/home_scope are tenant-
	// overridable (§3.3-listed — they ARE the per-tenant scope-resolution keys).
	// CONSUMER OBLIGATION (04-W6/T38, NOT W2): the per-tenant background pipeline
	// MUST intersect a tenant's configured read_scopes with that tenant's actual
	// entitlements (ctx_auth own ∪ grants, _-filtered) before reading — a raw
	// config value is NOT grant-gated, so an unintersected consumer would be a
	// cross-tenant background leak. Alt: global-only (background stays server-
	// scoped); umentscheidbar because the per-tenant background consumer (T38) is
	// unbuilt. Pausable: no tenant rows + no per-tenant background path until W3/W4.
	ReadScopes []string `key:"scheduler.read_scopes" env:"CTX_READ_SCOPES" default:"private,shared,work" mut:"hot" tenancy:"tenant-overridable"`
	// HomeScope has no env knob in F1 (hardcoded "private" until now); the
	// key-keyed loader makes it settings-fillable from F2 without a special
	// path (env:"-" skips the env source, not the lookup).
	HomeScope string `key:"scheduler.home_scope" env:"-" default:"private" mut:"hot" tenancy:"tenant-overridable"`
	// LLMLogRetentionDays is the age after which the background janitor NULLs
	// the prompt/response BODIES in context_llm_log. The telemetry row
	// (pipeline/model/tokens/cost/block_ids/backend/trust) survives — the
	// egress audit stays lossless, only the plaintext shadow corpus is dropped
	// (Body-NULLing, NOT a chunk drop; masterplan E4). 0 disables retention:
	// bodies are kept forever (operator opt-in). The 90-day default ships safe.
	// global-only (design 03 §8-D5): the body-NULLing janitor runs process-global
	// over the one hypertable; per-tenant retention needs a scope-aware janitor
	// (telemetry achse, own wave). The operator owns the egress-audit policy.
	LLMLogRetentionDays int `key:"scheduler.llmlog_retention_days" env:"CTX_LLMLOG_RETENTION_DAYS" default:"90" mut:"hot" parse:"strict" tenancy:"global-only"`
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
	TickInterval time.Duration `key:"events.tick_interval" env:"CTX_EVENTS_TICK_INTERVAL" default:"5" mut:"hot" tenancy:"global-only"`
	// QueueStatsInterval decouples dream.QueueDepth — an O(n) full-scan CTE
	// over context_blocks with no covering index — from the base tick. The
	// queue forecast's smallest window is 1h, so 30s is fresh enough and the
	// scan never rides the 5s cadence. 1M+ follow-up (partial index /
	// maintained counters) is named in design 04 §3.6 / R12.
	QueueStatsInterval time.Duration `key:"events.queue_stats_interval" env:"CTX_EVENTS_QUEUE_STATS_INTERVAL" default:"30" mut:"hot" tenancy:"global-only"`
	// PingInterval is the SSE keepalive cadence (a ": ping" comment line). It
	// MUST stay below the fronting proxy's read timeout (nginx
	// proxy_read_timeout, currently 60s) — an idle stream between diffs is
	// dropped otherwise. Policy-as-data: a proxy retune is a settings flip, not
	// a rebuild; 25s leaves ~2 pings inside a 60s window. F4-W7 (GET
	// /api/events) only; the query heartbeat shares the same 25s rationale.
	PingInterval time.Duration `key:"events.ping_interval" env:"CTX_EVENTS_PING_INTERVAL" default:"25" mut:"hot" tenancy:"global-only"`
	// MaxConnections caps concurrent SSE streams; the admin-only endpoint
	// answers 429 above it and the client degrades to polling. The default is
	// sized for the 256M container — autonomous agents / friend-tenant panels
	// (O5) raise it via a settings flip, not a redeploy. parse:"strict": a
	// malformed cap is an operator typo worth a loud boot abort (same call as
	// llmlog.max_limit), not a silent fall-back that hides the intended ceiling.
	// max_connections is a per-tenant SSE stream cap (§3.3-listed) — tenant-
	// overridable, while the cadences above stay process-global (one collector,
	// one proxy-bound ping window).
	MaxConnections int `key:"events.max_connections" env:"CTX_EVENTS_MAX_CONNECTIONS" default:"8" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
}

// ProjectConfig is the workflow project surface (design/03 §4.4). The forge SYNC
// trigger and the SSE domain-event hub (W9) carry runtime knobs; the webhook
// rate/retention keys land with the W13 inbound surface.
type ProjectConfig struct {
	Sync    ProjectSyncConfig
	Events  ProjectEventsConfig
	Webhook ProjectWebhookConfig
}

// ProjectWebhookConfig governs the unauthenticated GitHub inbound surface (POST
// /webhooks/github/{project_id}, workflow W13, design/03 §3.4/§4.4/§5.3). W11
// deliberately did NOT create these keys (no webhook surface yet); they land with
// the inbound wave.
//
// RateLimit is counted PER PROJECT over a fixed 60-s window
// (context_webhook_events, idx_webhook_project_recent) and applies ONLY to
// signature-valid deliveries (§5.3 order Body-Cap → Lookup → HMAC → Rate-Limit →
// INSERT) — an unsigned flood never reaches the counter, so it cannot push GitHub
// into disabling the hook. An int ceiling like events.max_connections ⇒
// parse:"strict" (a typo'd cap silently defaulting would hide the intended
// Denial-of-Sync protection). tenant-overridable (a per-tenant repo-traffic
// profile), 120/min the GitHub-comfortable default.
//
// Retention is the age past which the scheduler Janitor arm evicts PROCESSED
// deliveries index-gestützt (idx_webhook_done); the queue is a through-buffer,
// not an archive (the block-write audit lives in context_write_log). Duration
// suffix h/d/w/m/y (Hours parser), 0 = keep forever (operator opt-out, same
// convention as scheduler.llmlog_retention_days / webchat.session_retention).
// global-only: the janitor is one process-wide sweep over the one queue table,
// like the llmlog body-NULLing janitor (design/03 §3.4/§8).
type ProjectWebhookConfig struct {
	RateLimit int   `key:"webhook.rate_limit" env:"CTX_WEBHOOK_RATE_LIMIT" default:"120" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	Retention Hours `key:"webhook.retention" env:"CTX_WEBHOOK_RETENTION" default:"14d" mut:"hot" tenancy:"global-only"`
}

// ProjectEventsConfig governs the GET /api/project/events SSE domain-event hub
// (workflow W9, design/03 §4.5/§6.2). MaxConnections is counted PER TENANT in
// projectHub.subscribe() (§4.4/§6.2) — deliberately NOT the server-global
// events.max_connections (/api/events, telemetry): a global 8-cap would push
// whole tenants into the expensive per-request auth poll path at target scale
// (§6.4). FlushInterval is the coalescing cadence: NOTIFY writes accumulate per
// project for one interval, then flush as ONE frame set — a 10k-import burst
// collapses to O(subs) frames per tick, not O(writes) (§6.2). CoalesceThreshold
// is the per-project-per-tick block count above which the frame degrades to a
// content-free {kind:'issues-bulk', count} refetch signal instead of an id list.
// PingInterval is the keepalive; it reuses the events.ping_interval rationale
// (below the fronting-proxy read timeout).
type ProjectEventsConfig struct {
	MaxConnections    int           `key:"project.events.max_connections" env:"CTX_PROJECT_EVENTS_MAX_CONNECTIONS" default:"16" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	FlushInterval     time.Duration `key:"project.events.flush_interval" env:"CTX_PROJECT_EVENTS_FLUSH_INTERVAL" default:"1" mut:"hot" tenancy:"global-only"`
	PingInterval      time.Duration `key:"project.events.ping_interval" env:"CTX_PROJECT_EVENTS_PING_INTERVAL" default:"25" mut:"hot" tenancy:"global-only"`
	CoalesceThreshold int           `key:"project.events.coalesce_threshold" env:"CTX_PROJECT_EVENTS_COALESCE_THRESHOLD" default:"20" mut:"hot" parse:"strict" tenancy:"global-only"`
}

// ProjectSyncConfig governs the manual forge-sync trigger (POST /api/project/{id}
// /sync, workflow W11). RateLimit is counted PER PROJECT over context_project_
// sync_runs (§4.4 — NOT per api_key_id like the I6 write throttle: N agent keys of
// one repo share the budget, so they cannot storm one GitHub token). MaxConcurrent
// is a PROCESS-global semaphore over the per-project run-state map — it can carry
// no tenant override (a process-wide slot count is not a per-tenant quantity), so
// it is global-only + restart (the semaphore is sized once at boot).
type ProjectSyncConfig struct {
	RateLimit     int `key:"project.sync.rate_limit" env:"CTX_PROJECT_SYNC_RATE_LIMIT" default:"6" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	MaxConcurrent int `key:"project.sync.max_concurrent" env:"CTX_PROJECT_SYNC_MAX_CONCURRENT" default:"3" mut:"restart" parse:"strict" tenancy:"global-only"`
}

// LLMLogConfig is the telemetry read surface. The retention knob (body-NULLing
// after N days) lives under scheduler.* because it is a janitor job; this is
// the per-request read cap for GET /api/llmlog.
type LLMLogConfig struct {
	// MaxLimit caps GET /api/llmlog?limit=. The endpoint is admin-only and
	// rides the created_at DESC hypertable path; 200 keeps one page bounded
	// without a full chunk walk.
	// Server read-endpoint cap (bounds one /api/llmlog page over the hypertable)
	// — global-only by fail-closed default; not a per-tenant tuning surface.
	MaxLimit int `key:"llmlog.max_limit" env:"CTX_LLMLOG_MAX_LIMIT" default:"200" mut:"hot" parse:"strict" tenancy:"global-only"`
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
	Enabled bool `key:"webchat.enabled" env:"CTX_WEBCHAT_ENABLED" default:"true" mut:"hot" tenancy:"tenant-overridable"`
	// MaxIterations caps the tool loop per turn; one closing call WITHOUT tools
	// follows the cap (E4 — never tool_choice:none).
	MaxIterations int `key:"webchat.max_iterations" env:"CTX_WEBCHAT_MAX_ITERATIONS" default:"6" mut:"hot" tenancy:"tenant-overridable"`
	// MaxTokens clamps max_tokens per model call (a request may ask less, never
	// more); the per-backend limits.chat_max_tokens clamp applies on top (CPU 512).
	MaxTokens int `key:"webchat.max_tokens" env:"CTX_WEBCHAT_MAX_TOKENS" default:"2048" mut:"hot" tenancy:"tenant-overridable"`
	// CompletionBudget caps Σ completion_tokens across all iterations of one turn.
	CompletionBudget int `key:"webchat.completion_budget" env:"CTX_WEBCHAT_COMPLETION_BUDGET" default:"8192" mut:"hot" tenancy:"tenant-overridable"`
	// ToolResultMaxChars truncates one tool result (ctx_get pages the rest via
	// the offset marker so a >window block stays fully readable).
	ToolResultMaxChars int `key:"webchat.tool_result_max_chars" env:"CTX_WEBCHAT_TOOL_RESULT_MAX_CHARS" default:"8000" mut:"hot" tenancy:"tenant-overridable"`
	// HistoryBudgetChars bounds the session history fed into the prompt (~15k
	// tokens); older tool results condense, then oldest messages drop (§3.6).
	HistoryBudgetChars int `key:"webchat.history_budget_chars" env:"CTX_WEBCHAT_HISTORY_BUDGET_CHARS" default:"60000" mut:"hot" tenancy:"tenant-overridable"`
	// LLMTimeout bounds one model call (bare seconds; CPU-fallback worst case is
	// why it is large — the per-backend MaxTokens clamp keeps that bounded).
	LLMTimeout time.Duration `key:"webchat.llm_timeout" env:"CTX_WEBCHAT_LLM_TIMEOUT" default:"900" mut:"hot" tenancy:"tenant-overridable"`
	// ConcurrentTurns caps simultaneously running turns per home_scope (the
	// in-memory semaphore §3.3); above it the handler answers 429 before stream
	// start. Bounds the turn FREQUENCY per tenant, not just one turn's budget (R1).
	ConcurrentTurns int `key:"webchat.concurrent_turns" env:"CTX_WEBCHAT_CONCURRENT_TURNS" default:"1" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	// SessionRetention enables the retention janitor: sessions whose updated_at
	// is older than this are deleted (messages cascade). 0 = off (kept forever,
	// the shipped default). Duration suffix h/d/w/m/y (Hours parser, since v2.6.0).
	SessionRetention Hours `key:"webchat.session_retention" env:"CTX_WEBCHAT_SESSION_RETENTION" default:"0" mut:"hot" tenancy:"tenant-overridable"`
}

// WritesConfig governs the F6-C6 write-confirmation staging store
// (context_pending_writes, migration 089): LLM-path writes (MCP/Chat) are
// staged and only a hash-selected confirm executes them; REST/CLI stay direct.
// TWO DECOUPLED knobs (masterplan D-E3 — the coupled draft was a double-break,
// D2-C1: expiry=eviction meant ttl=0 rejected every confirm AND grew unbounded).
type WritesConfig struct {
	// ConfirmTTL is the expiry clock: a staged write must be confirmed within
	// this window (bare seconds; default 600 = 10min — long enough for human
	// confirm latency). 0 = staged writes never expire (0-is-off convention),
	// which is NOT feature-death: expiry and eviction are separate knobs.
	// global-only: the D-W3 ticker is one process-wide sweep.
	ConfirmTTL time.Duration `key:"writes.confirm_ttl" env:"CTX_WRITES_CONFIRM_TTL" default:"600" mut:"hot" tenancy:"global-only"`
	// ConfirmRetention is the eviction window: the D-W3 ticker chunk-drops
	// hypertable chunks whose rows (created_at) are older than this — consumed
	// history and never-confirmed stages alike (D2-M3). 0 = keep forever
	// (operator opt-out, same convention as llmlog/webchat/webhook retention).
	// Duration suffix h/d/w/m/y (Hours parser); keep well above confirm_ttl.
	ConfirmRetention Hours `key:"writes.confirm_retention" env:"CTX_WRITES_CONFIRM_RETENTION" default:"24h" mut:"hot" tenancy:"global-only"`
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
	// Per-tenant trust policy (§3.3-listed): a tenant sets its own default
	// sensitivities and per-scope floor — affects only its own blocks' egress
	// eligibility. The floor only RAISES (ScopeFloor.Apply is monotone) and the
	// defaults stay guard:"sensitivity-downgrade" (lowering needs a confirm flag),
	// so per-tenant override stays fail-closed within the tenant.
	DefaultQuerySensitivity backends.Sensitivity `key:"pool.default_query_sensitivity" env:"-" default:"personal" mut:"hot" guard:"sensitivity-downgrade" tenancy:"tenant-overridable"`
	DefaultBlockSensitivity backends.Sensitivity `key:"pool.default_block_sensitivity" env:"-" default:"credentials" mut:"hot" guard:"sensitivity-downgrade" tenancy:"tenant-overridable"`
	ScopeSensitivityFloor   ScopeFloor           `key:"pool.scope_sensitivity_floor" env:"-" default:"{}" mut:"hot" tenancy:"tenant-overridable"`

	// GamingActive flips the named GPU-host backends out of EVERY chain so the
	// GPU is free to game. It lives in the settings layer (persistent across
	// restarts) — NOT an atomic: the dream-mode break path (restart ⇒ the GPU
	// lock is gone) is the explicit anti-pattern here (design 03 §2.6).
	// Toggled via `ctx gaming on|off` / the gaming-mode manage action.
	// gaming.active/disabled_backends are NAMED global-only: the GPU is physically
	// one host, not a tenant concept (design 03 §3.3) — a tenant must never flip a
	// server-wide GPU switch.
	GamingActive bool `key:"gaming.active" env:"-" default:"false" mut:"hot" tenancy:"global-only"`
	// GamingDisabledBackends names which backends gaming.active excludes —
	// policy as data, so a second GPU host later is a list edit, not code.
	// Default = the herbert GPU backends; the CPU/external rows stay in as
	// failover. Comma-split (scopes parser); the gaming-mode action validates
	// the names against the live pool (a typo ⇒ unknown_backends, risk 6.6).
	GamingDisabledBackends []string `key:"gaming.disabled_backends" env:"-" default:"herbert-chat,herbert-rerank" mut:"hot" tenancy:"global-only"`
}

// TenantConfig holds per-tenant POLICY switches the OPERATOR sets, never the
// tenant itself — hence global-only (a tenant-scope row is dropped by the
// toOverrides gate, so a tenant cannot self-grant). The value lives in the
// tenant's OWN context_settings scope and is read DIRECTLY at that scope
// (store.TenantAllowsSharedSecrets), NOT through this snapshot field — the
// snapshot value is server-global by the global-only classification and is not
// the per-tenant truth. The field exists so the key is a known registry entry
// (write-gate + GET visibility), not as a consumed snapshot value.
type TenantConfig struct {
	// AllowSharedSecrets opts a tenant INTO the shared _global secret fallback
	// (design 03 §4.3/D2): false (default = STRICT isolation) resolves a tenant
	// secret_ref ONLY at the tenant scope; true lets tenantSecretResolver fall
	// back to a shared _global provider key. global-only so a tenant-admin cannot
	// self-grant it; the operator seeds the per-tenant row out of band, and the
	// resolver / checkSecretRef read it at the tenant scope.
	// TENANT-DECISION(allow-shared-secrets): default false (strikte Isolation) —
	// Alt default true / getrennte Flags, umentscheidbar weil additiver Settings-Gate.
	AllowSharedSecrets bool `key:"tenant.allow_shared_secrets" env:"-" default:"false" mut:"hot" tenancy:"global-only"`

	// AllowCrossTenantBlockGrant opts a tenant INTO making block-level read grants
	// to a FOREIGN tenant (design/07 §5.2): false (default = strict isolation)
	// allows only INTRA-tenant block grants (department→department, the Enterprise
	// use-case); true lets an owner-tenant admin share a SINGLE block across the
	// tenant boundary. global-only so a tenant-admin cannot self-grant it; the
	// value is read DIRECTLY at the tenant scope
	// (store.TenantAllowsCrossTenantBlockGrant), NOT through this snapshot field —
	// the field exists only so the key is a known registry entry (write-gate + GET
	// visibility), exactly like AllowSharedSecrets.
	// TENANT-DECISION(allow-cross-tenant-block-grant): default false (opt-in), analog
	// E5 — Alt default true, umentscheidbar weil additiver Settings-Gate.
	AllowCrossTenantBlockGrant bool `key:"tenant.allow_cross_tenant_block_grant" env:"-" default:"false" mut:"hot" tenancy:"global-only"`
}

// DispatchConfig is the internal/dispatch admission-layer surface (Vorhaben
// E, MW1 — design/01 §3.1 + design/03 §3.1). All keys are hot and
// global-only: the dispatcher arbitrates the process-shared physical
// backend targets (classification rule above). The capacity caps are
// parse:"strict" like their siblings (webchat.concurrent_turns,
// events.max_connections) — a typo'd cap silently falling back to the
// default would hide the intended ceiling on the single llama.cpp slot.
// The per-TARGET policy (slots, preempt_background, herald_scope) lives in
// context_backends.limits, not here (mechanism=code, policy=data). The
// settings reload owner maps this struct onto dispatch.Settings.
type DispatchConfig struct {
	// Enabled is the emergency stop, NOT a feature flag (E-U3): false
	// degrades every Acquire to a pass-through = exactly today's behavior.
	// The code waves are behavior-neutral through policy emptiness anyway;
	// a default-off switch would be a second activation truth (B7 analog).
	// Deliberately non-strict: the exact-match bool parser cannot malform,
	// so a strict tag would be an unreachable classification (see
	// TestRegistryStrictSet's contract).
	Enabled bool `key:"dispatch.enabled" env:"CTX_DISPATCH_ENABLED" default:"true" mut:"hot" tenancy:"global-only"`
	// BackgroundQueueMax caps waiting background acquires per target; above
	// it Acquire fails fast with ErrQueueFull and the arm retries on its own
	// cadence (terminal defer — the W49c back-off keeps its authority).
	BackgroundQueueMax int `key:"dispatch.background_queue_max" env:"CTX_DISPATCH_BACKGROUND_QUEUE_MAX" default:"32" mut:"hot" parse:"strict" tenancy:"global-only"`
	// InteractiveQueuePerPrincipal is the B9/E-U6 tag-1 brake: waiting
	// interactive acquires per ApiKeyID per target; 0 = off.
	InteractiveQueuePerPrincipal int `key:"dispatch.interactive_queue_per_principal" env:"CTX_DISPATCH_INTERACTIVE_QUEUE_PER_PRINCIPAL" default:"8" mut:"hot" parse:"strict" tenancy:"global-only"`
	// InteractiveQueuePerTenant closes the key-mint factorization (C8): N
	// self-minted keys of ONE tenant cannot multiply the principal cap; 0 = off.
	InteractiveQueuePerTenant int `key:"dispatch.interactive_queue_per_tenant" env:"CTX_DISPATCH_INTERACTIVE_QUEUE_PER_TENANT" default:"16" mut:"hot" parse:"strict" tenancy:"global-only"`
	// InteractiveQueueMax is the aggregate load-shed per target: an early
	// 429 beats an unservable wait slot (design/03 §4.5.4); 0 = off.
	InteractiveQueueMax int `key:"dispatch.interactive_queue_max" env:"CTX_DISPATCH_INTERACTIVE_QUEUE_MAX" default:"64" mut:"hot" parse:"strict" tenancy:"global-only"`
	// LeaseReapGrace (bare seconds) is the slack AFTER the reap reference
	// before the reaper force-releases a never-released lease (B1).
	LeaseReapGrace time.Duration `key:"dispatch.lease_reap_grace" env:"CTX_DISPATCH_LEASE_REAP_GRACE" default:"30" mut:"hot" parse:"strict" tenancy:"global-only"`
	// LeaseMaxAge (bare seconds) is the reap fallback for leases without a
	// deadline hint AND without a ctx deadline (the embed wire path since
	// wave 49). Default == the longest legitimate hold (webchat stream
	// timeout below).
	LeaseMaxAge time.Duration `key:"dispatch.lease_max_age" env:"CTX_DISPATCH_LEASE_MAX_AGE" default:"900" mut:"hot" parse:"strict" tenancy:"global-only"`
	// PreemptReleaseTimeout (bare seconds) is the preempt watchdog (MW18,
	// E-P2): a background victim canceled in favor of interactive demand
	// whose release stays out past this fence is force-released — the third
	// legitimate release path (slog-ERROR + divergence counter). It guards
	// the CLIENT half of the abort latency (cancel → wire return, purely
	// Go-side, healthy in milliseconds); 2 s is a ≥3-orders-of-magnitude
	// safety factor, calibrated against preempt_release_ms_max after MW20.
	PreemptReleaseTimeout time.Duration `key:"dispatch.preempt_release_timeout" env:"CTX_DISPATCH_PREEMPT_RELEASE_TIMEOUT" default:"2" mut:"hot" parse:"strict" tenancy:"global-only"`
	// BackgroundAgingAfter (bare seconds) arms the FA anti-starvation aging
	// escape (MW25, design/04 §4.6; key renamed from background_max_wait per
	// K6 — the semantics are an aging ESCAPE, never a wait-abort budget): a
	// background acquire waiting longer may break the herald term once. It
	// NEVER overtakes a waiting interactive acquire, and on targets with an
	// interactive role it requires preempt_background=true (coupling
	// invariant, F-B7 — the only tag-1-built exception to the herald term,
	// K7). 0 (default) = off: herald term unweakened, behavior byte-identical.
	// Activation is an operations decision under the aged-preempt waste
	// metric (E-F5), not a deploy.
	BackgroundAgingAfter time.Duration `key:"dispatch.background_aging_after" env:"CTX_DISPATCH_BACKGROUND_AGING_AFTER" default:"0" mut:"hot" parse:"strict" tenancy:"global-only"`
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
