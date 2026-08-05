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
	Server         ServerConfig
	Chat           ChatConfig
	Fallback       FallbackConfig
	Embed          EmbedConfig
	Dream          DreamConfig
	Rerank         RerankConfig
	Graph          GraphConfig
	GraphOverview  GraphOverviewConfig
	GraphCache     GraphCacheConfig
	Cluster        ClusterConfig
	ClusterOps     ClusterOpsConfig
	Query          QueryConfig
	Scheduler      SchedulerConfig
	Pool           PoolConfig
	Events         EventsConfig
	Project        ProjectConfig
	LLMLog         LLMLogConfig
	WebChat        WebChatConfig
	Writes         WritesConfig
	Tenant         TenantConfig
	Dispatch       DispatchConfig
	Contract       ContractConfig
	Status         StatusConfig
	EmbedBackfill  EmbedBackfillConfig
	EmbedMigration EmbedMigrationConfig
	RecallCheck    RecallCheckConfig
	Selector       SelectorConfig

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
	// Language selects the daily-synthesis report language. EMPTY (the
	// default) = legacy behavior: German report, title "Tagesbericht <date>",
	// tag "tagesbericht" — byte-identical for existing deployments, whose
	// report series is keyed by that very title ((category, title, scope) is
	// the upsert identity). Set it explicitly to localize; a primary subtag
	// of "de" ("de", "de-DE", …) stays on the legacy German surface, every
	// other tag switches title/tag/system-prompt to English + the named
	// language. V14 validates the shape (see validateDream).
	Language string `key:"dream.language" env:"CTX_DREAM_LANGUAGE" default:"" mut:"hot" tenancy:"global-only"`
	// LinkFloorConfidence is the raw confidence assigned to relationship
	// links the LLM names WITHOUT a strength signal (string-map drift form,
	// absent confidence fields — PR #12). The default 0.9 keeps such links
	// above the RRF graph-expansion retrieval gate (graph.min_confidence,
	// default 0.75), so a type-only answer still yields retrieval-live
	// edges. Set 0.7 for the conservative PR-#12 semantics — links persist
	// and show in the ego graph but stay out of RRF expansion until a later
	// dream cycle re-classifies them — or any other [0,1] float. Values
	// below a type's minRawConfidence write gate are lifted to that gate
	// per type (a floored link that cannot clear the write gate would be a
	// silent no-op). Out-of-range values are fatal at boot (V15).
	LinkFloorConfidence float64 `key:"dream.link_floor_confidence" env:"CTX_DREAM_LINK_FLOOR_CONFIDENCE" default:"0.9" mut:"hot" tenancy:"global-only"`

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
	// TombstoneRetention is the window in which a RETIRED topic keeps its
	// substance available for a re-attach (E2-01 / Amendment A01-2, wave W3):
	// a birth candidate first probes its members against the core_blocks of
	// tombstones of the SAME scope that died no longer ago than this, and
	// re-attaches instead of minting a fresh identity. That is what lets a
	// batch import which tears a partition apart across several rebuilds find
	// its old topics again instead of renaming the whole map.
	//
	// Seconds like the other duration keys; 45 d is the proposal of the
	// decision record (dream.backoff_cap order of magnitude) and covers
	// several rebuild cycles at the 6 h cadence. 0 disables the re-attach
	// path entirely — the run then behaves like pure one-generation matching,
	// which mints a NEW topic and never revives a foreign identity, so the
	// off state is the fail-closed one.
	//
	// NOT parse:"strict": a retention horizon is a cadence, not a security
	// ceiling — same classification as rebuild_timeout one line up. The purge
	// that consumes the same key is wave W8; W3 only reads it.
	TombstoneRetention time.Duration `key:"graph_overview.tombstone_retention" env:"CTX_GRAPH_OVERVIEW_TOMBSTONE_RETENTION" default:"3888000" mut:"hot" tenancy:"global-only"`
}

// ClusterConfig is the Achse-03 cluster-consumption RANKING surface
// (plan-cluster-topicmap design/03 §4.9): the query-time knobs of the
// categorical retrieval stage that boosts results sharing a Louvain cluster
// with the strong hits of a query. It is the sibling of GraphConfig (the
// kantenbasierte dream-graph expansion) — same domain, kategorische instead of
// kantenbasierte Evidenz — and split from ClusterOpsConfig along the same line
// GraphConfig / GraphOverviewConfig already draw: ranking policy here,
// wire/operations there.
//
// Wave C0 declares the group COMPLETELY and without a consumer (K6: the only
// cluster.*-declaring wave); C3/C8/C9 wire their fields later. Every default is
// therefore the SHIPPED-dark state — flipping Enabled is a data flip, not a
// re-tuning exercise.
type ClusterConfig struct {
	// Every knob is query-time RRF augmentation tuning: a tenant tuning its own
	// cluster boost affects only its own queries, zero cross-tenant effect →
	// tenant-overridable as a group (verbatim the GraphConfig rationale above).
	Enabled     bool    `key:"cluster.enabled" env:"CTX_CLUSTER_ENABLED" default:"false" mut:"hot" tenancy:"tenant-overridable"`
	SeedCount   int     `key:"cluster.seed_count" env:"CTX_CLUSTER_SEED_COUNT" default:"10" mut:"hot" tenancy:"tenant-overridable"`
	TopClusters int     `key:"cluster.top_clusters" env:"CTX_CLUSTER_TOP_CLUSTERS" default:"2" mut:"hot" tenancy:"tenant-overridable"`
	MinShare    float64 `key:"cluster.min_share" env:"CTX_CLUSTER_MIN_SHARE" default:"0.25" mut:"hot" tenancy:"tenant-overridable"`
	BoostWeight float64 `key:"cluster.boost_weight" env:"CTX_CLUSTER_BOOST_WEIGHT" default:"0.12" mut:"hot" tenancy:"tenant-overridable"`
	// SizeDamping normalises a cluster's share by its corpus share. Default TRUE
	// (UD-04-03): at the Ziel-Scale the measured Heavy-Tail (live Median 6 vs
	// max 133 = 11 % of all nodes) degenerates the un-damped boost — a mega
	// cluster wins nearly every query by construction. Shipping it armed keeps
	// the semantics identical from day one instead of changing behaviour on the
	// way to scale; the A/B knob stays.
	SizeDamping bool `key:"cluster.size_damping" env:"CTX_CLUSTER_SIZE_DAMPING" default:"true" mut:"hot" tenancy:"tenant-overridable"`
	// The centroid arm (C8) is the query-INDEPENDENT prior: "where does this
	// question live" even when RRF returns nothing usable. Read only once the
	// centroid table (migration 127) exists and is filled.
	CentroidEnabled bool    `key:"cluster.centroid_enabled" env:"CTX_CLUSTER_CENTROID_ENABLED" default:"false" mut:"hot" tenancy:"tenant-overridable"`
	CentroidWeight  float64 `key:"cluster.centroid_weight" env:"CTX_CLUSTER_CENTROID_WEIGHT" default:"0.5" mut:"hot" tenancy:"tenant-overridable"`
	CentroidTopK    int     `key:"cluster.centroid_top_k" env:"CTX_CLUSTER_CENTROID_TOP_K" default:"3" mut:"hot" tenancy:"tenant-overridable"`
	// InjectMax caps how many unseen blocks of the winning cluster C9 may add to
	// a result set. Default 0 = the stage never injects — arming it is a
	// deliberate step after the eval measurement, never a side effect of a
	// deploy. Declared here (not in C9) because K6 keeps the namespace in ONE
	// wave; a knob whose default is a documented no-op is not an operator trap.
	InjectMax int `key:"cluster.inject_max" env:"CTX_CLUSTER_INJECT_MAX" default:"0" mut:"hot" tenancy:"tenant-overridable"`
}

// ClusterOpsConfig is the Achse-03 WIRE and OPERATIONS surface (design/03 §4.9)
// — the counterpart to ClusterConfig, cut exactly like GraphOverviewConfig sits
// next to GraphConfig.
//
// EVERY key is global-only, and the reason differs per group but never
// disappears: ego_annotate / ego_annotate_max_nodes / facet_enabled /
// route_enabled are WIRE CONTRACTS (a response shape that differs per tenant is
// a shape no client can rely on), max_staleness guards the integrity of a
// SHARED artefact (the landkarte), and the centroid_* knobs steer ONE
// process-wide background build.
type ClusterOpsConfig struct {
	// MaxStaleness is the age past which the cluster stage switches itself off
	// (C4): a boost computed from a frozen map is a confident wrong answer.
	// Bare seconds like the other duration keys (24h).
	MaxStaleness time.Duration `key:"cluster.max_staleness" env:"CTX_CLUSTER_MAX_STALENESS" default:"86400" mut:"hot" tenancy:"global-only"`
	EgoAnnotate  bool          `key:"cluster.ego_annotate" env:"CTX_CLUSTER_EGO_ANNOTATE" default:"false" mut:"hot" tenancy:"global-only"`
	// EgoAnnotateMaxNodes is the annotation's OWN ceiling, deliberately below
	// the ego route ceiling (egoMaxLimit 1500): above it C2 answers with empty
	// arrays plus a trip instead of lowering the whole route's ceiling or
	// blowing the latency gate. /api/graph/all defaults limit to the ceiling, so
	// this is the normal case there, not the corner case.
	EgoAnnotateMaxNodes int  `key:"cluster.ego_annotate_max_nodes" env:"CTX_CLUSTER_EGO_ANNOTATE_MAX_NODES" default:"500" mut:"hot" tenancy:"global-only"`
	FacetEnabled        bool `key:"cluster.facet_enabled" env:"CTX_CLUSTER_FACET_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`
	RouteEnabled        bool `key:"cluster.route_enabled" env:"CTX_CLUSTER_ROUTE_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`
	// The centroid build (C8) runs in its OWN transaction after the persist
	// commit — never inside it: the rebuild is all-or-nothing under
	// graph_overview.rebuild_timeout, so a centroid step inside would roll back
	// a complete, good rebuild whenever the sum overruns.
	CentroidBuild   bool          `key:"cluster.centroid_build" env:"CTX_CLUSTER_CENTROID_BUILD" default:"false" mut:"hot" tenancy:"global-only"`
	CentroidTimeout time.Duration `key:"cluster.centroid_timeout" env:"CTX_CLUSTER_CENTROID_TIMEOUT" default:"300" mut:"hot" tenancy:"global-only"`
	CentroidBatch   int           `key:"cluster.centroid_batch" env:"CTX_CLUSTER_CENTROID_BATCH" default:"500" mut:"hot" tenancy:"global-only"`
	// CentroidWorkMem is the per-statement work_mem of the aggregation step (a
	// PG memory literal, e.g. "256MB"). C8 owns its validation and its
	// application — it is a SET LOCAL value, so the consumer must whitelist the
	// literal form rather than interpolate it raw.
	CentroidWorkMem string `key:"cluster.centroid_work_mem" env:"CTX_CLUSTER_CENTROID_WORK_MEM" default:"256MB" mut:"hot" tenancy:"global-only"`
	// CentroidANNThreshold is a declared RESOURCE limit, not a semantic one
	// (UD-02-03): below it the centroid read is an exact ~170 MB scan at the
	// 10M ceiling — no recall question, no index churn. Above it C8 builds the
	// HNSW index once per cycle in a build-and-swap, never maintaining it row by
	// row through a 6h full-replace. Calibrated against the C8 measurement.
	CentroidANNThreshold int `key:"cluster.centroid_ann_threshold" env:"CTX_CLUSTER_CENTROID_ANN_THRESHOLD" default:"50000" mut:"hot" tenancy:"global-only"`
	CentroidEFSearch     int `key:"cluster.centroid_ef_search" env:"CTX_CLUSTER_CENTROID_EF_SEARCH" default:"100" mut:"hot" tenancy:"global-only"`
}

// GraphCacheConfig is the Achse-05 CSR graph-cache track (design/05 §4.7). It
// governs the in-process adjacency cache over BOTH link tables (dream +
// structural) and its rebuild job (W05.2). Doctrine "Mechanismus = Code / Policy
// = Daten": the CSR build, the state automaton and the dirty clock are code in
// internal/graphcache; every numeric knob lives here.
//
// EVERY key is global-only — including the future serve flags — and the §4.7
// rationale is deliberately explicit (not just the fail-closed default): the
// cache is ONE process-global heap structure (a single CSR snapshot over the
// whole corpus) served by ONE rebuild goroutine. A per-tenant override would
// touch NOTHING tenant-private (config.go:62-67 tenancy rule) — enabled and the
// cadence own a shared process resource, and a tenant-tunable rebuild cadence or
// staleness ceiling could only mis-tune or disarm the one shared cache the whole
// process serves from (the OOM-Guard / Prozess-Ressource argument, §4.7). Unlike
// the security-ceiling caps, this group carries NO parse:"strict" (RecallCheck
// precedent): an operational tuning knob falling back to its protective default
// on a malformed value is the right degradation, never a boot-abort.
type GraphCacheConfig struct {
	// Enabled is the master gate (default FALSE, §4.7): the rebuild job stays
	// inert until enabled, and every consumer path (W05.5+) reads a nil Current()
	// → SQL fallback. A hot enable triggers a fresh boot-build on the next loop.
	Enabled bool `key:"graph_cache.enabled" env:"CTX_GRAPH_CACHE_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`
	// RebuildInterval is the HARD interval (§4.3): an unconditional rebuild at
	// least this often, measured from the last build START — it covers missed
	// NOTIFYs (listener reconnect) and the signal-free past. Seconds (6h default),
	// like the other duration keys.
	RebuildInterval time.Duration `key:"graph_cache.rebuild_interval" env:"CTX_GRAPH_CACHE_REBUILD_INTERVAL" default:"21600" mut:"hot" tenancy:"global-only"`
	// DebounceWindow is the quiet period a dirty signal must age before a
	// signal-driven rebuild fires (§4.3, quiet = now − lastDirtyAt). Seconds.
	DebounceWindow time.Duration `key:"graph_cache.debounce_window" env:"CTX_GRAPH_CACHE_DEBOUNCE_WINDOW" default:"60" mut:"hot" tenancy:"global-only"`
	// MinRebuildInterval is the upper bound on rebuild FREQUENCY (§4.3): a
	// signal-driven rebuild is suppressed until this long since the last build
	// start, so periodic link-writes cannot trigger a rebuild storm. Seconds.
	MinRebuildInterval time.Duration `key:"graph_cache.min_rebuild_interval" env:"CTX_GRAPH_CACHE_MIN_REBUILD_INTERVAL" default:"300" mut:"hot" tenancy:"global-only"`
	// MaxPendingAge is the starvation bound (§4.3): a rebuild fires no later than
	// this after the OLDEST unconsumed signal (firstPendingAt), even under writes
	// denser than DebounceWindow. Seconds.
	MaxPendingAge time.Duration `key:"graph_cache.max_pending_age" env:"CTX_GRAPH_CACHE_MAX_PENDING_AGE" default:"600" mut:"hot" tenancy:"global-only"`
	// MaxStaleness is the Dirty-Age degradation threshold (§4.3/§4.6): once a
	// signal is pending AND its Dirty-Age (now − firstPendingAt) exceeds this, the
	// automaton goes Degraded and consumers fall back to SQL. It bounds the
	// maximum cache lie. Seconds (15 min default). It is Dirty-Age, NOT build age
	// — an idle DB never degrades (§4.3 idle regime).
	MaxStaleness time.Duration `key:"graph_cache.max_staleness" env:"CTX_GRAPH_CACHE_MAX_STALENESS" default:"900" mut:"hot" tenancy:"global-only"`
	// FailedThreshold is the number of consecutive build failures after which the
	// state turns Failed (status red + an error log per attempt, §4.6).
	FailedThreshold int `key:"graph_cache.failed_threshold" env:"CTX_GRAPH_CACHE_FAILED_THRESHOLD" default:"3" mut:"hot" tenancy:"global-only"`
	// DegreeWalkBudget caps the hint-filtered degree walk (§4.1, E-05-3a); the
	// degree becomes a lower bound past it. Consumed by the W05.6 degree path;
	// laid down here so the whole graph_cache.* group is one surface.
	DegreeWalkBudget int `key:"graph_cache.degree_walk_budget" env:"CTX_GRAPH_CACHE_DEGREE_WALK_BUDGET" default:"4000" mut:"hot" tenancy:"global-only"`
	// ServeEgo / ServeExpand are the W05.5 / W05.7 consumer flags (default false):
	// laid down here so the surface is complete, but no consumer reads them yet.
	ServeEgo    bool `key:"graph_cache.serve_ego" env:"CTX_GRAPH_CACHE_SERVE_EGO" default:"false" mut:"hot" tenancy:"global-only"`
	ServeExpand bool `key:"graph_cache.serve_expand" env:"CTX_GRAPH_CACHE_SERVE_EXPAND" default:"false" mut:"hot" tenancy:"global-only"`
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
	// DBStatsInterval decouples the /api/status "db" section (Evokoa-Clean-
	// Room design/03 §4.7 — migrations/contract/extensions/relations/HNSW/
	// embed-backlog) from the base tick, the QueueStatsInterval pattern
	// applied to a second source: catalog/pg_stat reads are cheap but
	// pointless at the 5s cadence. 60s mirrors contract.recheck_interval's
	// own default — both cadences answer "how fresh does drift/ops
	// visibility over the SAME schema need to be". global-only: the db
	// section is server-wide schema/relation telemetry with no per-tenant
	// dimension (classification mirrors QueueStatsInterval, not
	// MaxConnections).
	DBStatsInterval time.Duration `key:"events.db_stats_interval" env:"CTX_EVENTS_DB_STATS_INTERVAL" default:"60" mut:"hot" tenancy:"global-only"`
	// LLMCallCoalesceThreshold is the per-tick llmlog row count above which the
	// stream's `llmcalls` frame degrades to a content-free {kind:'llmcalls-bulk',
	// count, cursor} refetch signal instead of carrying the rows — the exact
	// contract project.events.coalesce_threshold runs on the domain-event hub
	// (issues-bulk), so both SSE hubs share ONE coalescing doctrine. Below it the
	// rows ride the frame; the tick ALWAYS costs one frame per connection, never
	// one per row (the per-row fan-out overflowed a 16-deep mailbox above ~14
	// rows and dropped every open panel). global-only + strict for the same
	// reasons the sibling carries: a process-global fan-out knob, and a typo'd
	// threshold silently falling back to the default would hide the intended
	// degradation point. The cadences above stay non-strict (a stream cadence is
	// not a ceiling).
	LLMCallCoalesceThreshold int `key:"events.llmcall_coalesce_threshold" env:"CTX_EVENTS_LLMCALL_COALESCE_THRESHOLD" default:"20" mut:"hot" parse:"strict" tenancy:"global-only"`
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

// PoolConfig holds the per-tenant POOL policy: the F3 trust-gating surface it
// was born as, plus the blob write budget B2 added to it (settings-only, no env
// source — these keys are born in F2, not migrated from env vars).
// The sensitivity keys are guard:"sensitivity-downgrade": LOWERING them needs a
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
	// LLMAuditMinSensitivity is the floor of the G41 audit verdict (design 04
	// §4.5-c): the LLM classification may go HIGHER than this value, never
	// lower. Composed with backends.MaxSensitivity, which is monotone — the
	// same shape ScopeFloor.Apply already uses one line up.
	//
	// DEFENSE IN DEPTH: at the default this key closes NO reachable path.
	// auditOneBlock can only produce credentials, personal or internal
	// (internal/events/audit.go — public stays a manual decision by
	// construction), so 'internal' is exactly today's code floor, written down
	// as policy instead of living implicitly in a control flow. It starts
	// biting the day the verdict set is ever extended — or the day a tenant
	// raises it to 'personal' to keep its corpus off no-credentials backends.
	//
	// tenant-overridable like its pool siblings, and for the same reason: a
	// tenant may only make ITSELF stricter. That promise is only true if the
	// read point is the per-tenant snapshot the audit batch already takes
	// (internal/events/audit.go auditTenantScope) — reading the process-wide
	// base generation would silently serve the _global value.
	LLMAuditMinSensitivity backends.Sensitivity `key:"pool.llm_audit_min_sensitivity" env:"-" default:"internal" mut:"hot" guard:"sensitivity-downgrade" tenancy:"tenant-overridable"`
	// BlobRateLimitWrite caps /api/blob/store per api key and 60-second window
	// (B2/E1-A). It counts its OWN action (store.ActionBlobWrite), NOT the
	// block-write action query.rate_limit_write gates: a 50 MB binary upload and
	// a text block are different costs on different paths, and coupling them
	// meant a key at its block limit could not store a blob while blob writes
	// paid nothing back — a budget that only ever bit from the outside.
	//
	// This key does NOT follow the 0-is-off convention of its rate-limit
	// siblings: 0/unset falls back to the VALUE of query.rate_limit_write, so an
	// operator who only ever tuned the block limit keeps a bounded blob surface
	// instead of silently unlimited uploads. Off needs BOTH at 0 — deliberate,
	// because this is the surface where "unlimited" costs disk, not rows.
	// Settings-only like its PoolConfig siblings; tenant-overridable like the
	// block limit it falls back to.
	BlobRateLimitWrite int `key:"pool.blob_rate_limit_write" env:"-" default:"10" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	// ExternalNumCtxFallback is the operator-declared context window, in
	// TOKENS, for chain members whose row declares none (context_backends
	// .num_ctx IS NULL — H12 / decision E10). It is the ONLY way a prompt
	// carrying foreign text may be built for such a chain; without it the
	// budget pass refuses (promptguard.ErrUndeclaredWindow).
	//
	// Default 0 = UNSET = no fallback = refuse. Conservative on purpose: the
	// alternative — a compiled-in rate value — is not a conservative guess but
	// a wrong one. For the live openrouter row the routed model's per-provider
	// windows span 32 768 to 262 144, and the model-level context_length is the
	// MAXIMUM over providers, not the minimum; sizing a prompt against it
	// overflows on every provider below the top. An operator who knows which
	// floor their routing actually guarantees can declare it here (or, better,
	// put num_ctx on the row itself, which is per-backend rather than global).
	//
	// tenant-overridable like its pool siblings and for the same reason: the
	// value only ever bounds the tenant's OWN prompts, and a tenant on a
	// stricter provider tier must be able to declare its own floor.
	ExternalNumCtxFallback int `key:"pool.external_num_ctx_fallback" env:"-" default:"0" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	// OpenRouterWindowTTL is the cache lifetime, in SECONDS, of the
	// per-provider endpoint discovery behind AUTO context windows (E10-W2):
	// an openrouter-class row with num_ctx NULL asks
	// GET {base_url}/v1/models/{model}/endpoints which providers serve the
	// routed model and how large their windows are, plans against the best of
	// them, and constrains each request to the providers that can hold it.
	//
	// Default 3600 s: the provider mix of a model changes on the order of
	// days, and the cost of being an hour behind is bounded on both sides —
	// a provider that vanished is caught by ordinary failover, a provider that
	// appeared merely goes unused until the refresh. Refreshes are
	// stale-while-error (a transient API failure keeps the last known mix
	// serving, up to a hard 24 h age limit inside the cache).
	//
	// 0 = OFF, following the 0-is-off convention of its rate-limit siblings:
	// discovery stops, and a NULL window falls back to
	// pool.external_num_ctx_fallback — or refuses, which is the H12 floor this
	// key can raise but never lower.
	//
	// tenant-overridable like its pool siblings: the value only affects how
	// often the tenant's OWN requests re-read a public metadata route.
	OpenRouterWindowTTL int `key:"pool.openrouter_window_ttl" env:"-" default:"3600" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	// The legacy gaming.active / gaming.disabled_backends settings keys were
	// retired in Web-UX U01-W5 (AM-7 cutover): chain-time exclusion is now the
	// eject disable-profile (092), read live from the pool snapshot. Any leftover
	// gaming.* rows in context_settings are inert — admitOverride drops them as
	// unknown keys (build.go), so no delete-migration is needed.
}

// BlobWriteLimit resolves the effective /api/blob/store budget of this
// snapshot: the dedicated pool.blob_rate_limit_write when it is set, otherwise
// the VALUE of query.rate_limit_write as a fallback ceiling (B2/E1-A, see the
// field doc). 0 means no limit — reachable only when BOTH keys are 0.
//
// Resolution lives here, not in the handler, so the fallback cannot drift
// between the gate that refuses a request and any later reader of the budget.
func (c *Config) BlobWriteLimit() int {
	if c.Pool.BlobRateLimitWrite > 0 {
		return c.Pool.BlobRateLimitWrite
	}
	return c.Query.RateLimitWrite
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
// default would hide the intended ceiling on the shared llama.cpp backend.
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

// ContractConfig is the schema-contract check surface (Evokoa-Clean-Room
// design/03 §4.4/§4.5, wave W03-3). Both keys are global-only — the check
// runs once per process over the one shared schema, there is no per-tenant
// dimension.
//
// Mode carries a DOCUMENTED, DELIBERATE exception to this registry's normal
// DB>env>default precedence. It is registered here — with ordinary tags —
// only so contract.mode is a known registry entry (visible/writable through
// the generic GET/PUT /api/settings surface, counted by
// TestRegistryCoversEveryField, classified by TestRegistryTenancySet)
// exactly like every other key. Its OWN merged value is deliberately NEVER
// read by any enforcement decision: internal/schemacontract.ResolveMode
// (called from cmd/ctxd's schemaContractBoot and
// schemacontract.RunCheckSingleFlight) resolves the EFFECTIVE mode
// independently, straight from os.Getenv(schemacontract.EnvContractMode)
// and a direct context_settings read, with ENV-DOMINANT precedence (env >
// DB > default; an "off" row written to the DB is not honored) — the
// opposite order of this field's own registry-merged value. Consuming
// cfg.Contract.Mode anywhere for an enforcement decision would silently
// reinstate the exact DB>env precedence bug §4.4 documents: a DB writer —
// the very actor migration_integrity distrusts — could then override an
// operator's env break-glass (CTX_CONTRACT_MODE=off). See
// internal/schemacontract's package doc and mode.go for the real
// resolution.
//
// RecheckInterval carries NO such exception: cfgStore is genuinely how the
// periodic re-check ticker (cmd/ctxd's startContractRecheckTicker) learns a
// hot cadence change, normal DB>env>default precedence applies.
type ContractConfig struct {
	// Mode is registry-complete but behaviorally inert — see the type doc
	// above. Deliberately untyped/unvalidated (plain string, no Validate
	// entry): schemacontract.ResolveMode already treats any value outside
	// off|warn|enforce as "unrecognized" and falls through safely, so a
	// second validation layer here would only duplicate that logic against
	// a value nothing ever acts on.
	Mode string `key:"contract.mode" env:"CTX_CONTRACT_MODE" default:"warn" mut:"hot" tenancy:"global-only"`
	// RecheckInterval (bare seconds) is the periodic re-check cadence
	// (design/03 §4.5): 0 = off (the ticker keeps polling on its own fixed
	// cadence so a later hot flip to a positive value is picked up without
	// a restart, but runs no check while off). Default 60s: Introspect+Diff
	// are catalog-only and row-count-independent (design/03 §6) — the
	// cadence buys "drift laut binnen ≤60s am laufenden Prozess" at
	// millisecond cost per tick. Non-strict like events.tick_interval and
	// its cadence siblings: a malformed value falls back to the default
	// instead of aborting boot — a re-check cadence is not a security
	// ceiling (unlike the dispatch.*/events.max_connections int caps).
	RecheckInterval time.Duration `key:"contract.recheck_interval" env:"CTX_CONTRACT_RECHECK_INTERVAL" default:"60" mut:"hot" tenancy:"global-only"`
}

// StatusConfig is the /api/status observability surface OUTSIDE the
// events.db_stats_interval cadence (design/03 §4.7, W03-8): the four-channel
// (semantic/fts_de/fts_en/trigram) latency probe against ctx_rrf's own CTEs.
type StatusConfig struct {
	// ChannelProbeInterval gates the ChannelProbe (design/03 §4.7/E-03-5).
	// Default 0 = OFF, deliberately WITHOUT the <=0-falls-back-to-N convention
	// events.db_stats_interval/contract.recheck_interval use: 0 here means the
	// probe NEVER runs and dbStatus.ChannelProbe stays permanently null (Gate
	// 1 default-off golden) — not "runs on some default cadence". The probe
	// shares recall_check's Probe-Input-Quelle (context_embed_cache) and the
	// same "wieviel Eigenlast ist akzeptabel" question; E-03-5's recommendation
	// is to flip this on together with the Achse-01 recall_check rollout, not
	// on deploy. Non-strict, same cadence-field convention as
	// RecheckInterval/DBStatsInterval — a malformed value falls back to the
	// default (off) rather than aborting boot.
	ChannelProbeInterval time.Duration `key:"status.channel_probe_interval" env:"CTX_STATUS_CHANNEL_PROBE_INTERVAL" default:"0" mut:"hot" tenancy:"global-only"`
}

// EmbedBackfillConfig is the Achse-04 W04-2 safety-valve surface for the two
// regular (non-migration) embed-backfill paths: the scheduler arm
// (backfillOneEmbedding, Pfad B) and the query-path pre-search loop
// (backfillPending, Pfad A). Both share the same context_embed_failures memo
// mechanic (migration_id NULL) that closes the Vorfall-2026-07-10 head-of-
// line class (design/04 §4.4). global-only: this governs a single shared
// physical resource (the embed backend pool + one process-wide failures
// table) the same way scheduler.llmlog_retention_days and
// events.db_stats_interval do — not a per-tenant query-time knob.
type EmbedBackfillConfig struct {
	// SyncCap bounds Pfad A's synchronous per-request work (design/04 §4.3
	// "Pfad-A-Kappung"): the query-path backfill loop today has no cap and
	// embeds EVERY pending block inline before the triggering search runs —
	// after a large rest-transient the FIRST interactive query pays the
	// entire nachzug. 0 = uncapped (explicit opt-out, GraphOverview.MaxNodes
	// convention), never the accidental zero-value: a raw &Config{} in a
	// unit test that never sets this field disables the cap rather than
	// silently blocking every backfillPending call.
	SyncCap int `key:"embed_backfill.sync_cap" env:"CTX_EMBED_BACKFILL_SYNC_CAP" default:"4" mut:"hot" tenancy:"global-only"`
	// MaxTokens is the pre-wire Oversize-Gate estimate threshold
	// (design/04 §4.4): len(embedText)/4 above this skips the block WITHOUT
	// a wire call (last_class='oversize', next_attempt_at='infinity' —
	// permanently parked, never a blind 24h-in-slow-motion retry). Default
	// 24000 sits below the live kv-unified pool's 32k (docker-
	// compose.override.yml) minus interactive headroom minus margin — see
	// docs/operations.md "Embedding backend tuning". 0 = pre-check disabled
	// (same explicit-opt-out convention as SyncCap); HTTP 400
	// exceed_context_size responses are still classified as oversize on the
	// wire-error path regardless of this gate (the estimate is the vorab-
	// filter, the classification is the net behind it).
	MaxTokens int `key:"embed_backfill.max_tokens" env:"CTX_EMBED_BACKFILL_MAX_TOKENS" default:"24000" mut:"hot" tenancy:"global-only"`
	// BackoffBase/BackoffCap shape the exponential retry backoff
	// (base * 2^(attempts-1), capped) a non-oversize embed failure (backend
	// down, transient wire error) gets memoized with — the SAME curve
	// class as dream.backoff_* but scoped to embed failures, computed
	// server-side in the upsert (store.RecordEmbedFailure) to avoid a
	// read-then-write race across concurrent pickers. Bare seconds, like
	// the other duration keys (contract.recheck_interval convention).
	// Default 1min base / 24h cap (design/04 §4.4).
	BackoffBase time.Duration `key:"embed_backfill.backoff_base" env:"CTX_EMBED_BACKFILL_BACKOFF_BASE" default:"60" mut:"hot" tenancy:"global-only"`
	BackoffCap  time.Duration `key:"embed_backfill.backoff_cap" env:"CTX_EMBED_BACKFILL_BACKOFF_CAP" default:"86400" mut:"hot" tenancy:"global-only"`
}

// EmbedMigrationConfig is the Achse-04 W04-4 knob surface for the re-embed
// MIGRATION worker (migrateOneEmbedding, design/04 §4.3/§4.4) — the
// dual-column sibling of EmbedBackfillConfig above, kept as its own key
// group because the two arms are tuned independently (a migration may want
// a larger batch while the regular backfill stays untouched, and vice
// versa). global-only for the same reason: one shared embed pool, one
// system-wide migration (idx_embed_migration_single_active).
type EmbedMigrationConfig struct {
	// BatchPerCycle bounds how many blocks one scheduler tick migrates
	// (design §4.3 Takt: BACKGROUND class + per-attempt admission make the
	// DURATION harmless — this cap bounds how long a single tick can hold
	// the scheduler loop and how much counter delta a crash can lose).
	// <=0 disables the arm's per-cycle work entirely (explicit opt-out,
	// SyncCap convention — a raw &Config{} in a unit test does nothing
	// rather than silently running an 8er batch against a half-built
	// fixture).
	BatchPerCycle int `key:"embed_migration.batch_per_cycle" env:"CTX_EMBED_MIGRATION_BATCH_PER_CYCLE" default:"8" mut:"hot" tenancy:"global-only"`
	// MaxTokens is the migration worker's pre-wire Oversize-Gate threshold
	// (design §4.4) — same len/4 estimate, same infinity-park semantics as
	// embed_backfill.max_tokens, same 24000 default (32k kv-unified pool
	// minus interactive headroom minus margin; len/4 underestimates
	// hex-dense content ~2×, the wire-error classification is the net
	// behind this gate). 0 = pre-check disabled.
	MaxTokens int `key:"embed_migration.max_tokens" env:"CTX_EMBED_MIGRATION_MAX_TOKENS" default:"24000" mut:"hot" tenancy:"global-only"`
	// BackoffBase/BackoffCap shape the exponential retry curve for
	// migration-scoped failure memos (context_embed_failures rows with
	// migration_id set) — server-side exponent, bare seconds, exactly the
	// embed_backfill.backoff_* mechanics. Default 1min base / 24h cap
	// (design §4.4).
	BackoffBase time.Duration `key:"embed_migration.backoff_base" env:"CTX_EMBED_MIGRATION_BACKOFF_BASE" default:"60" mut:"hot" tenancy:"global-only"`
	BackoffCap  time.Duration `key:"embed_migration.backoff_cap" env:"CTX_EMBED_MIGRATION_BACKOFF_CAP" default:"86400" mut:"hot" tenancy:"global-only"`
	// VerifySampleN is the W04-5 verify gate's sampling knob (design/04
	// §4.7): capacity of the guard-stage cosine-distribution reservoir
	// (Stufe 5) and cap for the named lists inside verify_report (skip
	// list). The Stufe-2 integrity checks (dims/norm/model) deliberately run
	// FULL-coverage inside the folded single-pass scan — the pass already
	// pays the TOAST detoast of every _next vector for the quality kNN, so
	// a norm check per row is one fused loop at zero marginal I/O; sampling
	// there would only re-open the exact defect class the gate exists to
	// catch. <=0 skips the guard-stage sample (section reports "skipped").
	VerifySampleN int `key:"embed_migration.verify_sample_n" env:"CTX_EMBED_MIGRATION_VERIFY_SAMPLE_N" default:"1000" mut:"hot" tenancy:"global-only"`
	// VerifyOverlapK is k for the degraded Stufe-4 quality metric
	// (Overlap@k old vs. new space, informative — the Achse-01 recall_check
	// replaces this once it exists, see events.runVerifyQualityStage).
	VerifyOverlapK int `key:"embed_migration.verify_overlap_k" env:"CTX_EMBED_MIGRATION_VERIFY_OVERLAP_K" default:"10" mut:"hot" tenancy:"global-only"`
	// VerifyOverlapSamples is the number of sample queries the Overlap@k
	// stage draws (deterministic md5-ordered draw over migrated blocks —
	// blocks-as-queries, both space vectors exact and wire-free). <=0
	// disables Stufe 4 (section reports "skipped").
	VerifyOverlapSamples int `key:"embed_migration.verify_overlap_samples" env:"CTX_EMBED_MIGRATION_VERIFY_OVERLAP_SAMPLES" default:"16" mut:"hot" tenancy:"global-only"`
}

// RecallCheckConfig is the Achse-01 recall_check surface (design/01 §3.2):
// the periodic ANN-vs-exact recall measurement on the live corpus. Doctrine
// "Mechanismus = Code / Policy = Daten": the probe mechanics (two-leg
// measurement, plan assertion, recall arithmetic, stratification) are code in
// internal/recall; every tunable (k, N, bounds, budgets, cadence, off-peak
// anchor, ef, epsilon) lives here. The whole group is laid down in W01-2; the
// core (internal/recall.RunOnce) consumes k_list/queries_per_stratum/
// strata_bounds/epsilon/ef_search plus the two leg-budget values, the
// scheduler keys (enabled/interval/offpeak_hour/park_max_ms/retention_days
// and the budget ROTATION semantics) are wired in W01-3. global-only: the
// arm measures a single shared physical resource (the one HNSW index + one
// shared buffer pool); recall metrics are server-admin-only (§5.3).
// Deliberately NO parse:"strict" anywhere in this group (the
// TestRegistryStrictSet pin stays untouched): recall_check is a trend
// monitor, not a security ceiling — a malformed value falling back to its
// protective default (E-01-7 spirit: never boot-abort power for the
// measuring arm) is the right degradation, unlike the dispatch/quota caps.
type RecallCheckConfig struct {
	// Enabled is the master gate of the arm (E-01-1: default on — "erst
	// messen"; a default-off measuring system recreates the GAP-D hole).
	Enabled bool `key:"recall_check.enabled" env:"CTX_RECALL_CHECK_ENABLED" default:"true" mut:"hot" tenancy:"global-only"`
	// Interval is the run cadence of the cheap strata (small/medium),
	// hot-reloaded per iteration (Overview pattern). Seconds.
	Interval time.Duration `key:"recall_check.interval" env:"CTX_RECALL_CHECK_INTERVAL" default:"86400" mut:"hot" tenancy:"global-only"`
	// OffpeakHour anchors the expensive strata (large/all) to a local wall-
	// clock hour (runDailySynthesis pattern); 4 is deliberately offset from
	// the daily synthesis at 03:00.
	OffpeakHour int `key:"recall_check.offpeak_hour" env:"CTX_RECALL_CHECK_OFFPEAK_HOUR" default:"4" mut:"hot" tenancy:"global-only"`
	// KList is the comma-separated measurement k set: 10 = comparability with
	// the R@10 history, 75 = the productive semantic-CTE window (073 LIMIT 75).
	KList string `key:"recall_check.k_list" env:"CTX_RECALL_CHECK_K_LIST" default:"10,75" mut:"hot" tenancy:"global-only"`
	// QueriesPerStratum is the target sample size per stratum (budget may cut).
	QueriesPerStratum int `key:"recall_check.queries_per_stratum" env:"CTX_RECALL_CHECK_QUERIES" default:"20" mut:"hot" tenancy:"global-only"`
	// StrataBounds are the class boundaries "b1,b2": small n<=b1 < medium
	// n<=b2 < large (embedded active blocks per scope). The default is PINNED
	// to the E-02-1 selector thresholds exact_max=4096 / grey_max=65536
	// (masterplan K3): the strata boundaries ARE the dispatch thresholds, so
	// the measurement calibrates the selector's buckets directly.
	StrataBounds string `key:"recall_check.strata_bounds" env:"CTX_RECALL_CHECK_STRATA_BOUNDS" default:"4096,65536" mut:"hot" tenancy:"global-only"`
	// ExactBudgetMS caps the sum of all exact legs of one run INCLUDING the
	// stratification count and the log-sampling scan (§6.2 budget clock).
	ExactBudgetMS int `key:"recall_check.exact_budget_ms" env:"CTX_RECALL_CHECK_EXACT_BUDGET_MS" default:"300000" mut:"hot" tenancy:"global-only"`
	// ExactTouchBudgetBytes is the second budget dimension: capped heap+TOAST
	// read volume of the exact legs per run. 0 = auto (25% of shared_buffers,
	// resolved at runtime — W01-3 wiring).
	ExactTouchBudgetBytes int `key:"recall_check.exact_touch_budget_bytes" env:"CTX_RECALL_CHECK_EXACT_TOUCH_BUDGET" default:"0" mut:"hot" tenancy:"global-only"`
	// LegTimeoutMS is the hard single-leg maximum (statement_timeout cap on
	// top of the remaining run budget) — one 10M leg must never eat the rest.
	LegTimeoutMS int `key:"recall_check.leg_timeout_ms" env:"CTX_RECALL_CHECK_LEG_TIMEOUT_MS" default:"60000" mut:"hot" tenancy:"global-only"`
	// ParkMaxMS bounds the mid-run demand park loop (§4.3, W01-3); beyond it
	// the run aborts with invalid_reason='demand_deferred'.
	ParkMaxMS int `key:"recall_check.park_max_ms" env:"CTX_RECALL_CHECK_PARK_MAX_MS" default:"600000" mut:"hot" tenancy:"global-only"`
	// EfSearch is the ANN-leg hnsw.ef_search; 0 = pgvector default (40).
	// Non-zero enables live tuning probes without a deploy.
	EfSearch int `key:"recall_check.ef_search" env:"CTX_RECALL_CHECK_EF_SEARCH" default:"0" mut:"hot" tenancy:"global-only"`
	// Epsilon is the tie tolerance of the distance-based recall definition
	// (§4.2.3: hit = dist(a) <= d_ref + epsilon). Default 0; stamped into
	// meta.epsilon of every row. A config key, not a code constant — the
	// doctrine "kein Schwellwert hart kodiert" applies to the tie window too.
	Epsilon float64 `key:"recall_check.epsilon" env:"CTX_RECALL_CHECK_EPSILON" default:"0" mut:"hot" tenancy:"global-only"`
	// RetentionDays is the janitor delete horizon for context_recall_runs
	// (§3.3, W01-3 janitor line).
	RetentionDays int `key:"recall_check.retention_days" env:"CTX_RECALL_CHECK_RETENTION_DAYS" default:"365" mut:"hot" tenancy:"global-only"`
}

// SelectorConfig is the semantic strategy dispatch (Achse 02, design/02
// §3.4). Doctrine "Mechanismus = Code / Policy = Daten": the two SQL arms,
// the bounded probe, the clamps and the dispatch algorithm are code in
// internal/rrf; WHICH thresholds apply and whether the selector runs at all
// is data here.
//
// EVERY key is global-only (the fail-closed default of the registry
// doctrine, registry.go:50-57): ExactMax/GreyScanTuples dimension buffer
// touch and materialisation of the SHARED database — a tenant override would
// very much touch something process-shared (config.go:62-77: then
// global-only). The GraphConfig rationale (config.go:226-229, "zero
// cross-tenant effect") does NOT carry here: graph knobs are capped at ~10
// injected blocks, these knobs at tens of thousands of index/heap accesses
// per query. Enabled is global-only so the measurement-reservation gate
// (E-02-2) is technically enforced. Tenant opening = follow-up wave W02-8
// (two-stage ceilings).
//
// StatsTTL follows the house convention for duration defaults: UNITLESS
// SECONDS (parseDurationSeconds, load.go:141-148) — hence default:"60", not
// "60s". The pg_stats snapshot behind it is a process resource anyway (one
// cache for all tenants; it holds only scope names × frequencies, which the
// same catalog shows every DB user — no tenant datum, §5.5).
type SelectorConfig struct {
	Enabled        bool          `key:"retrieval.selector.enabled" env:"CTX_RETRIEVAL_SELECTOR_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`
	ExactMax       int           `key:"retrieval.selector.exact_max" env:"CTX_RETRIEVAL_SELECTOR_EXACT_MAX" default:"4096" mut:"hot" tenancy:"global-only"`
	GreyMax        int           `key:"retrieval.selector.grey_max" env:"CTX_RETRIEVAL_SELECTOR_GREY_MAX" default:"65536" mut:"hot" tenancy:"global-only"`
	GreyScanTuples int           `key:"retrieval.selector.grey_scan_tuples" env:"CTX_RETRIEVAL_SELECTOR_GREY_SCAN_TUPLES" default:"60000" mut:"hot" tenancy:"global-only"`
	StatsTTL       time.Duration `key:"retrieval.selector.stats_ttl" env:"CTX_RETRIEVAL_SELECTOR_STATS_TTL" default:"60" mut:"hot" tenancy:"global-only"`
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

// ClusterRRF converts the cluster ranking group to the rrf-stage parameter
// struct (same converter pattern as GraphRRF — rrf never reads internal/config).
// The zero config is Enabled=false, i.e. the stage does not run and the pipeline
// is byte-identical to pre-C3.
//
// Only the fields the C3 stage actually CONSUMES are mapped. cluster.centroid_*
// stay unmapped until C8 wires them, for the same reason cluster.inject_max is
// not declared until C9 (design/03 §4.9): a knob that is visibly plumbed but
// inert is a trap for whoever tunes it.
func (c *Config) ClusterRRF() rrf.ClusterConfig {
	return rrf.ClusterConfig{
		Enabled:     c.Cluster.Enabled,
		SeedCount:   c.Cluster.SeedCount,
		TopClusters: c.Cluster.TopClusters,
		MinShare:    c.Cluster.MinShare,
		BoostWeight: c.Cluster.BoostWeight,
		SizeDamping: c.Cluster.SizeDamping,
		// The ONE field from the global-only ops group: cluster-map freshness
		// protects a SHARED artefact, so it must not be per-tenant widenable
		// (§4.9). The rrf stage sees one struct; the tenancy split lives here.
		MaxStaleness: c.ClusterOps.MaxStaleness,
	}
}

// SelectorRRF converts the selector group to the rrf-stage policy struct
// (same converter pattern as RerankRRF/GraphRRF — the rrf package never reads
// internal/config, F1 layering: policy travels as a parameter). The zero
// config produces Enabled=false, i.e. rrf's Ist path: no probe roundtrip,
// Decision{ann, disabled} (design/02 §4.2).
func (c *Config) SelectorRRF() rrf.SelectorPolicy {
	return rrf.SelectorPolicy{
		Enabled:        c.Selector.Enabled,
		ExactMax:       c.Selector.ExactMax,
		GreyMax:        c.Selector.GreyMax,
		GreyScanTuples: c.Selector.GreyScanTuples,
		StatsTTL:       c.Selector.StatsTTL,
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
		ScoreThreshold:         c.Query.ScoreThreshold,
		ConfidentThreshold:     c.Query.ConfidentThreshold,
		PromptVersion:          c.Query.PromptVersion,
		ExternalNumCtxFallback: c.Pool.ExternalNumCtxFallback,
		OpenRouterWindowTTL:    c.Pool.OpenRouterWindowTTL,
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
