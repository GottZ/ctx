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
//	         accepting them. Both coupled classes are UNOCCUPIED since β7:
//	         their last carriers were the embed tuple, and the embed-cache
//	         flush obligation moved to the pool write path (α5,
//	         events/listener.go). A new key taking either tag inherits the
//	         admission behaviour, not a settings-side flush.
//	parse    strict (malformed value = boot abort, today's getEnvInt fatal
//	         paths) | safe/default (malformed value = WARN + default, today's
//	         getEnv*Safe paths). F2 may flip safe fields to strict via this
//	         flag once the UI validates input (decision §7.3: F1 no, F2 yes).
//	secret   fp (machine-generated keys: presence + sha256 fingerprint ≥
//	         fpMinLen, boot dump only) | presence (human-chosen values: never
//	         fingerprinted — an unsalted hash prefix would be an offline
//	         dictionary oracle)
//	         (Historie: a `superseded` tag once marked the keys the
//	         context_backends rows had replaced. Its last carrier, the chat
//	         tuple, left the registry in β8; the tag, its API field, the PUT
//	         409, the CLI hint and the FE legacy card were removed in β9 — E11,
//	         complete teardown. That a retired NAME may never return to the
//	         registry is now stated solely by the collision pin in
//	         config/retired.go, which does not need a marker to say it.)
//	tenancy  tenant-overridable | global-only (MANDATORY — MT3-W2): may a
//	         tenant override this key on top of _global, or does it live in
//	         _global only? A missing/unknown value is a boot panic, so no key
//	         escapes the classification. Classification rule: a key is
//	         tenant-overridable only when overriding it per-tenant touches
//	         NOTHING process-shared — it affects solely that tenant's own
//	         query/dream/policy resolution (retrieval tuning, rate limits,
//	         scope/sensitivity policy, per-tenant budgets). Everything that
//	         touches a host/physical or process-wide resource is global-only:
//	         the DSN/listener (restart), gaming.active (GPU is one host), the
//	         scheduler cadences and offline jobs, and the server egress-audit
//	         retention. Fail-closed default for any NEW key: global-only.
//	         History: the class was written around the six backend tuples —
//	         their HOST/MODEL/PROTOCOL keys were the archetypal global-only
//	         topology, their api_key secret_refs the archetypal tenant-
//	         overridable per-tenant credential, and the embed-cache-coupled
//	         keys the reason a tenant may not reach a process-wide cache
//	         (R-SCALE6). All of them left the registry in β3–β8; per-tenant
//	         backends and per-tenant credentials come from the F3 pool's own
//	         scope dimension now. The rule is unchanged, only its examples.

// Config is the complete runtime configuration. One immutable value per
// generation; consumers take one snapshot per operation and pass values down
// as parameters.
type Config struct {
	Server         ServerConfig
	Dream          DreamConfig
	Rerank         RerankConfig
	Graph          GraphConfig
	GraphOverview  GraphOverviewConfig
	RootMap        RootMapConfig
	Digest         DigestConfig
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
	Distill        DistillConfig

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
	// InstanceKind is what this process claims to BE: "live" (default) or
	// "measure-copy" — a corpus restored from backups/*.dump for the shadow
	// measurement programme (design/05 §5 B4b, M-W2). The armsweep driver reads
	// it off the measured instance and refuses to dump shadow types against an
	// instance that does not carry "measure-copy"; the dump stamp records it, so
	// a later compare can refuse a mixed pair.
	//
	// Restart-only and global-only ON PURPOSE, and both halves are load-bearing:
	// the identity of a corpus is a property of the process that serves it, and
	// a hot or tenant-overridable key would let a settings write turn the live
	// instance into a "measure copy" for the duration of one dump. The value is
	// NOT validated against the two names: an unknown value fails the driver
	// gate like every other non-"measure-copy" value, which is the fail-closed
	// direction, and a boot that refuses to start over a typo in a provenance
	// label would trade a refused measurement for a refused service.
	InstanceKind string `key:"server.instance_kind" env:"CTX_INSTANCE_KIND" default:"live" mut:"restart" tenancy:"global-only"`
}

// ChatConfig died with the primary chat/synthesis tuple in β8 — the LAST of the
// six backend tuples to leave the registry, and with it the whole class. Three
// contracts ended here, none of them silently:
//
//   - chat.api_key was the last carrier of secret:"fp". The class stays valid
//     registry vocabulary and is now unoccupied; server.db_password (presence,
//     env-only, mut:"restart") is the only sensitive key left, and it is never
//     admitted as a settings row — so no registry key can reach the settings-
//     side secret_ref resolver any more. The resolver itself is live and pinned
//     on an injected synthetic registry (config/synthreg_test.go).
//     Its TENANT-DECISION(provider-api-key) is settled by removal: a tenant
//     brings its own provider credential through the pool row's api_key_ref,
//     resolved at the tenant's own scope, never through a config key.
//   - chat.host was the last carrier of the .host namespace. redactHostURL
//     (dump.go) keeps guarding the convention for every FUTURE .host key; today
//     it has none.
//   - chat.protocol and chat.host were the last inputs of V4 and V7, so
//     validateBackendTuples and validateHostURL are gone with them (validate.go
//     carries the history). The userinfo rejection V7 stood for lives on the
//     pool write path since α3 (backends/validate.go validateIdentity).
//
// Which backend serves a synthesis call is a pool question: the query path
// resolves the role-synthesis chain out of context_backends and reads host,
// credential, protocol, model, context window and think mode off the serving
// row (backends.Backend, llm.ChatChainVia).

// EmbedConfig died with the query-path embed tuple in β7. Its five keys were
// the last carriers of two mutability classes: mut:"coupled" (embed.model, the
// vector space) and mut:"coupled:embed-cache" (embed.host/protocol, the two
// values that change the vector space under an unchanged model name). Both
// classes stay valid registry vocabulary (validMut, registry.go) and are now
// unoccupied; the cache obligation they carried lives entirely on the pool
// side since α5 (events/listener.go coupledSet + the Migration-132
// fingerprint), which is where an embed-role backend is actually chosen.

// DreamConfig is the dream pipeline: the master switch, the scheduler cadence,
// the report language and the back-off policy. Both backend tuples it once
// carried have left — the embedding tuple in β5, its own chat tuple in β6.
// Which backend serves a dream job is a pool question (context_backends, roles
// dream and dream-embed, resolved in dream/router.go), and with the tuples went
// the field-by-field inheritance from chat/embed as well as the two checks that
// guarded it (V1 dual-runner num_ctx, V12 credential boundary).
type DreamConfig struct {
	Enabled bool `key:"dream.enabled" env:"CTX_DREAM_ENABLED" default:"false" mut:"restart" tenancy:"global-only"`

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
	// JSONMode is the WIRE policy of the four dream stages that PARSE their
	// answer — link evaluation, keyword extraction, the recurrence confirm and
	// the temporal Phase-2 review.
	//
	// "strict" (the default) is today's behavior byte for byte: the request
	// carries the dialect's JSON-mode marker (OpenAI
	// response_format:{"type":"json_object"}, Ollama top-level
	// "format":"json"). "off" sends plain chat; the JSON contract stays in the
	// prompt and the package's local parsers become the ONLY validator, which
	// is what they already are on every answer that arrives well-formed. The
	// empty string reads as strict (legacy sentinel, same doctrine as
	// dream.language "" and the timeout keys' 0); anything else is fatal at
	// boot / 422s the settings write (V20) — a typo must not silently mean
	// strict, because the failure it hides is a slow backend, not a wrong one.
	//
	// Why it exists: on a grammar-enforcing runtime the constraint costs
	// roughly half the decode throughput (and disables speculative decoding on
	// some), and the dream pipeline is the throughput floor of the corpus. The
	// key is hot, so an operator can A/B it on a live install without a
	// restart, and global-only, because the wire policy belongs to whoever
	// operates the backend, not to a tenant.
	//
	// The daily-synthesis stage is NOT covered and never sends the marker: its
	// answer is prose stored verbatim, so JSON mode there is corruption, not
	// validation (dream/synthesize_report.go).
	JSONMode string `key:"dream.json_mode" env:"CTX_DREAM_JSON_MODE" default:"strict" mut:"hot" tenancy:"global-only"`
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
	// TemporalTimeout bounds the dream-temporal Phase-2 LLM review (seconds).
	// Default 90 matches the legacy ValidateTimeout constant; raise for slow
	// reasoning models (nemotron-super-trt needs >90s on full prompts).
	// DEFAULT ONLY: the Phase-2 call runs under role dream, so a timeouts.dream
	// entry on the serving context_backends row takes precedence (Backend.
	// TimeoutFor, walked in llm.ChatChainVia). On such a row raise the row
	// value instead — it bounds dream eval/keywords/recurrence too, while this
	// key is temporal-only.
	TemporalTimeout time.Duration `key:"dream.temporal_timeout" env:"CTX_DREAM_TEMPORAL_TIMEOUT" default:"90" mut:"hot" tenancy:"global-only"`
	// CycleTimeout bounds the WHOLE dream cycle (seconds) — the single
	// context.WithTimeout in RunDreamCycle that wraps pick → temporal →
	// keywords → RRF → eval → recurrence. Default 700 matches the legacy
	// package CycleTimeout constant; raise for slow reasoning models
	// (Qwen3.8-27B-NVFP4 needs >700s on full 16-20-candidate prompts).
	// OUTER CEILING, not a default: a timeouts.dream entry on the serving
	// context_backends row (Backend.TimeoutFor, walked in llm.ChatChainVia)
	// still bounds each single call — but that call runs on a CHILD of the
	// cycle context (dispatch.Acquire derives runCtx from it, llm.ChatJSON
	// puts the per-call WithTimeout under that), and a child can never
	// outlive its parent's deadline. So a row value above the remaining
	// cycle budget has no effect at all: the row value can only SHORTEN a
	// call, never lengthen one. Raise THIS key first, and use the row value
	// only to cap individual calls below it.
	// Necessary but not sufficient: without such a row the per-call
	// ceilings are the code defaults — eval dream.DreamTimeout 180s,
	// keywords dream.KeywordsTimeout 120s — so on a stock row an eval that
	// needs 600-690s is cut at 180s no matter how high this key is. The
	// recipe is two steps, row first: (1) timeouts.dream >= the expected
	// eval duration, (2) this key >= temporal + keywords + eval +
	// recurrence.
	// SHUTDOWN: the cycle context is rooted at context.Background() so that
	// SetDreamMode(Off) owns its cancellation; SIGTERM therefore does NOT
	// cut an in-flight cycle — Scheduler.Wait() drains it, unbounded by the
	// 15s HTTP shutdown budget. Raising this key raises the worst-case
	// shutdown wait by the same amount.
	// Validated by V16c (validateDream): a negative value is an ERROR — 0 is
	// the "package default" sentinel and a negative one would render as a
	// configured deadline while CycleTimeoutFor served the constant — and a
	// positive value below dream.KeywordsTimeout + dream.DreamTimeout is a
	// WARN on this key, because such a cycle is cut before the link-writing
	// stages run.
	CycleTimeout time.Duration `key:"dream.cycle_timeout" env:"CTX_DREAM_CYCLE_TIMEOUT" default:"700" mut:"hot" tenancy:"global-only"`
	// NumPredict caps the OUTPUT tokens of the dream chat calls that share
	// DreamOptions: link evaluation and the recurrence confirm (the
	// scheduler builds one options value for the cycle and both consume it).
	// Not every dream call — temporal keeps its own 1000 and copies only
	// NumCtx from these options, keywords has its own 200.
	// Default 600 = the measured worst case of the object-map drift form
	// (five links, pretty-printed, ~500 tokens on the Qwen3 tokenizer) plus
	// margin; raise it when a backend's drift form is wordier still, which
	// today needs a rebuild or a per-row param edit. 0 = the package default
	// (dream.DefaultNumPredict), which the registry default is pinned to.
	// DEFAULT ONLY: a num_predict / max_tokens param in the serving
	// context_backends row's model_map takes precedence at dispatch
	// (applyModelParams, walked in llm.ChatChainVia) — the same precedence
	// story as dream.temporal_timeout against a timeouts.dream row entry.
	// On such a row tune the row value instead.
	// Validated by V18 (validateDream): a negative value is an ERROR — 0 is
	// the sentinel and a negative one would render as a configured cap while
	// DreamOptionsFor served the constant — and a positive value BELOW the
	// package default is a WARN, because it reopens the truncation the
	// default was measured against. WARN, not a clamp: a shorter cap is a
	// legitimate setting for an install whose backend answers compactly.
	NumPredict int `key:"dream.num_predict" env:"CTX_DREAM_NUM_PREDICT" default:"600" mut:"hot" tenancy:"global-only"`
	// EvalCapRetryFactor scales the output cap of ONE bounded retry after the
	// link evaluation hit that cap. A cap hit is now detectable rather than
	// inferable (the provider's finish_reason since issue #26, the
	// completion-tokens heuristic where the provider reports none), so the
	// cycle no longer has to book a truncated answer as ordinary malformed
	// output: it re-asks once at factor x the RESOLVED cap — resolved meaning
	// after a model_map num_predict/max_tokens override, since the scaling
	// happens where that override is applied (llm.applyModelParams).
	// <= 1 DISABLES the retry and restores today's behaviour exactly: one
	// call, the plain parse error, the 5-minute transient cooldown, a re-pick.
	// The key is a true kill switch — nothing else changes with it off.
	// A SECOND cap hit is booked as a completed-but-inert eval
	// (dream_eval_count + 1, dream_last_inert, the inert back-off offset)
	// instead of the 5-minute transient: an answer that overruns twice the
	// cap is far more plausibly prompt-specific (the eval prompt scales with
	// the candidate count) than a permanent backend defect, and the transient
	// path has no attempt counter — without the escalation such a block
	// re-burns one eval call every five minutes forever.
	// LIMIT: a serving context_backends row whose extra_body carries a
	// numeric max_tokens outbids Options.NumPredict on the wire
	// (applyOpenAIBodyExtras merges last-write-wins), so the retry could not
	// take effect there. The pipeline detects that row and SKIPS the retry,
	// returning the plain parse error — the "no behaviour change when the
	// retry cannot take effect" side, not a silent inert booking. Tune
	// extra_body.max_tokens on such rows instead.
	// Validated by V19 (validateDream): a negative value is an ERROR — the
	// off-semantics are <= 1, and a negative factor would render as a
	// configured multiplier while shrinking the cap if it ever applied.
	EvalCapRetryFactor float64 `key:"dream.eval_cap_retry_factor" env:"CTX_DREAM_EVAL_CAP_RETRY_FACTOR" default:"2" mut:"hot" tenancy:"global-only"`

	Backoff BackoffConfig
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

// RerankConfig is the post-RRF rerank stage. Since the β3 cut it carries the
// query-time knobs only: WHICH reranker runs is a pool question — a rerank-role
// row dispatches to the cross-encoder sidecar, no such row leaves the
// LLM-as-judge path (handler/query.go). host/api_key/model retired with the
// tuple (retired.go names their pool destinations).
type RerankConfig struct {
	// All three surviving keys are query-time tuning of the tenant's OWN
	// queries — isolation-safe, so the whole group is tenant-overridable
	// (blend_weight §3.3-listed). The topology half of the group, which had to
	// stay global-only because it named a backend, is gone with the cut.
	Enabled     bool    `key:"rerank.enabled" env:"CTX_RERANK_ENABLED" default:"false" mut:"hot" tenancy:"tenant-overridable"`
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

	// RunRetention is the horizon of the run journal (Achse 04 / S2, migration
	// 130). Seconds like every other duration key; 90 d is the design proposal.
	//
	// The journal is one row per rebuild ATTEMPT, and the rebuild is a
	// background job on a fixed cadence — at 4 runs/day over 3 scopes that is
	// ~12 rows/day, so 90 d is ~1.100 rows. The retention exists not because
	// the table grows dangerously but because an unbounded audit table is a
	// slow leak: the row count has to have a ceiling somebody chose.
	//
	// 0 keeps every row forever — a deliberate operator choice for a forensic
	// window, not an accident: the purge is a no-op then, never an error.
	//
	// NOT parse:"strict": a retention horizon is a cadence, not a security
	// ceiling — same classification as rebuild_timeout and tombstone_retention.
	RunRetention time.Duration `key:"graph_overview.run_retention" env:"CTX_GRAPH_OVERVIEW_RUN_RETENTION" default:"7776000" mut:"hot" tenancy:"global-only"`

	// CSRLoader switches the rebuild's input path from the []rawEdge
	// materialisation to the two-pass CSR cursor build (Achse 04 / S3).
	//
	// Ships OFF and is meant to stay off until the identity gate has been green
	// across several deploys. The reason is operational, not technical: on a
	// live system whose deploys are built from tag worktrees, rolling a binary
	// back is the most expensive form of undo there is, and flipping a config
	// key is the cheapest. The old path stays in the binary next to it.
	//
	// What flipping it buys, measured: the []rawEdge slice, the symmetrisation
	// map and the string→index map all disappear. At the 200k node cap those
	// three are the bulk of the 423 MB the current path peaks at.
	CSRLoader bool `key:"graph_overview.csr_loader" env:"CTX_GRAPH_OVERVIEW_CSR_LOADER" default:"false" mut:"hot" tenancy:"global-only"`

	// ── S6+S7: engine switch and time budget (design/04 §4.9) ──────────────
	//
	// Engine ships "gonum" and is meant to stay there until an announced
	// release cut: switching it is a ONE-TIME, GLOBAL partition break — every
	// cluster_id is recomputed, and the topic layer absorbs that through the
	// tombstone re-attach window, which has to be widened over the cut first
	// (UD-03-04). Flipping this key casually renames the whole map.
	//
	// parse:"strict" — an unknown value is a BOOT error, never a silent
	// fallback to gonum. Someone who writes engine=leiden has an expectation;
	// a fallback would disappoint it silently, with a partition that looks as
	// if it had been computed as asked.
	Engine string `key:"graph_overview.engine" env:"CTX_GRAPH_OVERVIEW_ENGINE" default:"gonum" mut:"hot" parse:"strict" tenancy:"global-only"`
	// TimeBudget is the PRIMARY liveness guard once engine=ctx: the own mover
	// checks ctx between queue blocks and aborts cleanly, where gonum's
	// Modularize can only be SIGKILLed. Seconds. 0 = off.
	//
	// It bounds the COMPUTE phase alone, not the load: a budget spanning both
	// would report a slow disk as compute time. The load stays under
	// rebuild_timeout like everything else.
	TimeBudget time.Duration `key:"graph_overview.time_budget" env:"CTX_GRAPH_OVERVIEW_TIME_BUDGET" default:"600" mut:"hot" tenancy:"global-only"`
	// MaxNodesCtx is the emergency stop of the own kernel — NOT a second
	// max_nodes. The ENGINE picks which key applies (UD-07-04): max_nodes for
	// gonum, this one for ctx. Two keys with static defaults rather than one
	// engine-dependent default, because the struct-tag mechanism cannot
	// express the latter and a hot engine switch would make it ambiguous.
	// The value that actually applied is in every journal row (max_nodes_eff).
	MaxNodesCtx int `key:"graph_overview.max_nodes_ctx" env:"CTX_GRAPH_OVERVIEW_MAX_NODES_CTX" default:"5000000" mut:"hot" parse:"strict" tenancy:"global-only"`
	// Refine enables the Leiden refinement pass (S5) — every delivered
	// community is connected. Default ON, effective only at engine=ctx
	// (UD-04-04): a disconnected community is a bad topic and a bad
	// core_blocks core, so the quality of axes 01 and 03 depends on it.
	Refine bool `key:"graph_overview.refine" env:"CTX_GRAPH_OVERVIEW_REFINE" default:"true" mut:"hot" tenancy:"global-only"`
	// WorkerMemLimit caps the rebuild CHILD's Go heap, in bytes (S7b). 0 = off.
	//
	// DEFAULT IS OFF, and that deviates from design/04 §3.4 on purpose. The
	// design proposed 160 MiB — a figure that predates the S1 measurement.
	// The current compute path peaks at ~423 MB at the 200k node cap, so a
	// 160 MiB ceiling would abort EVERY run at today's cap: a memory guard
	// would become a memory ban and the map would freeze permanently.
	//
	// Pick the value against `peak_rss_kb` in the run journal, which exists
	// for exactly this. UD-02-04 suggests ~60 % of the container limit as a
	// starting point once the compute path has been measured on your corpus.
	//
	// The child ALSO reads this through the inherited environment
	// (overview.WorkerMemLimitEnv) to limit its own heap before it has any
	// options — a runtime knob has to be available before the protocol is.
	// ComponentSplit enables the connected-component pre-pass with γ rescaling
	// (S8), effective only at engine=ctx. Default ON because it is PROVABLY
	// objective-identical (design/04 §4.4), not a heuristic trade: a community
	// spanning two components can always be split with strictly positive ΔQ, so
	// the global maximisation decomposes exactly — provided γ is rescaled per
	// component (γ_t = γ·m_t/m). Without the rescaling the pre-pass would
	// silently change the resolution for small components.
	//
	// The honest yield today is 6.3 % (75 of 1 192 live nodes sit outside the
	// giant component) and it shrinks as the corpus grows, because a giant
	// component is the structurally enforced normal form above the percolation
	// threshold. The lasting gain is component_n as a measurable quantity.
	// DeltaPersist writes only CHANGED member rows instead of delete-all +
	// insert-all (S9b). Result-identical — that is a gate, not an assumption.
	//
	// What it is actually for is not speed but VACUUM PRESSURE: the full
	// replacement rewrites every member row on every run and marks every old
	// one dead. At 9.8M members and four runs a day that is 39.2M dead tuples
	// per day on a table with 9.8M live rows — permanent autovacuum plus index
	// bloat on three indexes, a operational risk that has nothing to do with
	// compute time.
	//
	// Default ON: it is strictly less work for an identical result. `false`
	// reproduces the pre-S9b behaviour exactly, which is the rollback path.
	DeltaPersist   bool `key:"graph_overview.delta_persist" env:"CTX_GRAPH_OVERVIEW_DELTA_PERSIST" default:"true" mut:"hot" tenancy:"global-only"`
	ComponentSplit bool `key:"graph_overview.component_split" env:"CTX_GRAPH_OVERVIEW_COMPONENT_SPLIT" default:"true" mut:"hot" tenancy:"global-only"`
	WorkerMemLimit int  `key:"graph_overview.worker_mem_limit" env:"CTX_GRAPH_OVERVIEW_WORKER_MEM_LIMIT" default:"0" mut:"hot" tenancy:"global-only"`

	// ── W6: the LLM label pipeline (design/01 §3.5/§4.8, Amendments A01-3/A01-4) ──
	//
	// LabelEnabled ships TRUE, unlike graph_overview.enabled — decision E7-01
	// ("wir wollen ja, dass nutzer das auch verwenden"). The knob that keeps a
	// fresh install quiet is NOT this one but LabelMinTopics one line down: a
	// corpus with fewer living topics than the threshold is not complex enough
	// for automatic naming to be worth an inference budget, and the
	// deterministic W5 fallback already labels every topic. false stays the
	// hard opt-out.
	LabelEnabled bool `key:"graph_overview.label_enabled" env:"CTX_GRAPH_OVERVIEW_LABEL_ENABLED" default:"true" mut:"hot" tenancy:"global-only"`
	// LabelInterval is the label arm's cadence — deliberately DECOUPLED from
	// rebuild_interval (6 h): a label only has to follow a core drift, and the
	// cold-start backlog needs a place to work itself off. Seconds.
	LabelInterval time.Duration `key:"graph_overview.label_interval" env:"CTX_GRAPH_OVERVIEW_LABEL_INTERVAL" default:"3600" mut:"hot" tenancy:"global-only"`
	// LabelBatch is the TICK cap — topics per tick, across ALL scopes of the
	// tenant that tick serves, never per scope (design/01 §3.5). A per-scope
	// limit would be S × batch calls for a tenant with S scopes and the cap
	// would not cap. This is layer one of the three against the cold-start
	// storm (B8); layer two is the demand yield INSIDE the batch loop, layer
	// three the background lease preempt.
	LabelBatch int `key:"graph_overview.label_batch" env:"CTX_GRAPH_OVERVIEW_LABEL_BATCH" default:"200" mut:"hot" parse:"strict" tenancy:"global-only"`
	// LabelMinTopics is the complexity threshold of decision E7-01: below this
	// many LIVING topics a scope is not labelled at all — no LLM call, no
	// error, the W5 fallback carries the map. It is what turns "default on"
	// from a boot-time storm into an event that happens exactly once, when a
	// corpus becomes complex enough to need names, and incrementally after.
	LabelMinTopics int `key:"graph_overview.label_min_topics" env:"CTX_GRAPH_OVERVIEW_LABEL_MIN_TOPICS" default:"10" mut:"hot" parse:"strict" tenancy:"global-only"`
	// LabelPromptMaxTitles caps how many core-block titles reach one prompt.
	// A declared RESOURCE bound (Amendment A01-7), never a semantic one: the
	// Substanz-Kern is self-adaptive and can be large in a hub cluster, and a
	// prompt has a token budget. ~24 titles ≈ 600–800 tokens.
	LabelPromptMaxTitles int `key:"graph_overview.label_prompt_max_titles" env:"CTX_GRAPH_OVERVIEW_LABEL_PROMPT_MAX_TITLES" default:"24" mut:"hot" parse:"strict" tenancy:"global-only"`
	// LabelCredentialsFallbackOnly is stage 3 of the label output hardening
	// (Amendment A01-3 / decision E4-02) and ships OFF by design: stages 1 and
	// 2 — the sensitivity scan over the finished label and the deterministic
	// echo gate — are unconditional, this one is the opt-in that takes a
	// credentials core out of the LLM path entirely and leaves it with its
	// deterministic fallback name.
	LabelCredentialsFallbackOnly bool `key:"graph_overview.label_credentials_fallback_only" env:"CTX_GRAPH_OVERVIEW_LABEL_CREDENTIALS_FALLBACK_ONLY" default:"false" mut:"hot" tenancy:"global-only"`
	// LabelTimeout is the budget of ONE topic label, in seconds. The default 90
	// is the legacy topiclabel package constant verbatim, so an unset key is
	// the behaviour that shipped.
	//
	// It is a TOTAL budget, not a wire ceiling — and that distinction is the
	// whole point of the key (issue #37). The deadline is attached BEFORE the
	// call enters the dispatch admission queue (topiclabel labelOne →
	// llm.ChainCall → dispatch Acquire), so the queue wait is spent from the
	// same 90 s the model call then has to finish inside. On a saturated
	// single-backend pool that is not a theoretical split: the label can burn
	// the entire budget waiting for a slot and die with `acquire_expired`
	// before it ever reaches a wire.
	//
	// A `timeouts.digest` entry on the serving context_backends row is
	// therefore NOT the knob for that situation. It bounds only the wire call,
	// and it is additionally clamped by this key — the outer deadline is
	// already running when the per-backend one is applied, so the smaller of
	// the two wins and a row value above this key cannot take effect. Raise
	// THIS key first, the row second.
	//
	// global-only for the verbatim GraphOverviewConfig rationale: labelling is
	// one background job producing one shared artefact, so its budget steers a
	// process resource, not a tenant's own query resolution.
	LabelTimeout time.Duration `key:"graph_overview.label_timeout" env:"CTX_GRAPH_OVERVIEW_LABEL_TIMEOUT" default:"90" mut:"hot" tenancy:"global-only"`
}

// RootMapConfig is the Achse-02 root-map surface (plan-cluster-topicmap
// design/02 §4.8): the cluster-per-line map that replaces the block-per-line
// topic map. It is the third prefix of the K6 namespace cut — `graph_overview.*`
// owns the rebuild, `cluster.*` the consumption, `root_map.*` the MAP — and
// wave W-D declares it ONCE and COMPLETELY, including the two knobs whose
// consumer (W-F, the meta-cluster level) lands later. A namespace that grows
// wave by wave forces a compose edit per wave, and a knob the container cannot
// receive is not a knob.
//
// EVERY key is global-only, with the GraphOverviewConfig rationale verbatim:
// the map is written per tenant, but its PRODUCTION is one background job over
// one shared artefact — cadence, budget and caps steer a process resource, not
// a tenant's own query resolution.
type RootMapConfig struct {
	// Enabled is the master gate. Default OFF makes W-D a no-op deploy: the
	// scheduler's tail call returns before its first query, so the wave changes
	// nothing observable until the flag is flipped (pausability invariant).
	Enabled bool `key:"root_map.enabled" env:"CTX_ROOT_MAP_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`
	// BudgetBytes is the map's line budget — the deckel, not the target: the
	// renderer MEASURES and stops, so a smaller corpus simply produces a
	// smaller map. parse:"strict" because it is a size ceiling on a persisted
	// artefact: a malformed value must abort the boot, never silently widen the
	// budget. Run refuses anything above the 50 KB public write cap
	// (context_store blockSizeLimit parity) before it touches the database.
	BudgetBytes int `key:"root_map.budget_bytes" env:"CTX_ROOT_MAP_BUDGET_BYTES" default:"15360" mut:"hot" parse:"strict" tenancy:"global-only"`
	// SmallClusterMax is the collector-line threshold (§4.4c): clusters at or
	// below it are counted, never rendered as topics. At scale this is what
	// turns tens of thousands of link-poor clusters into one honest line.
	SmallClusterMax int `key:"root_map.small_cluster_max" env:"CTX_ROOT_MAP_SMALL_CLUSTER_MAX" default:"2" mut:"hot" tenancy:"global-only"`
	// FooterReserveBytes is the space the measuring loop keeps free for the two
	// accounting lines. The renderer errors instead of truncating them: a map
	// that loses its own footer is a map that stops accounting.
	FooterReserveBytes int `key:"root_map.footer_reserve_bytes" env:"CTX_ROOT_MAP_FOOTER_RESERVE_BYTES" default:"512" mut:"hot" tenancy:"global-only"`
	// CountTimeout caps the coverage counts — the only O(corpus) step of the
	// map. Bare seconds like every other duration key. On expiry the map drops
	// the denominator instead of estimating it: pg_class.reltuples can filter
	// neither scope nor is_archived, and a global figure inside a scope-owned
	// block is the BP-1 difference channel.
	CountTimeout time.Duration `key:"root_map.count_timeout" env:"CTX_ROOT_MAP_COUNT_TIMEOUT" default:"5" mut:"hot" tenancy:"global-only"`
	// LabelBudget caps the LLM label requests per cycle (§1.4 B4). 0 = the
	// rendered row budget (NodeLimit), which is the only value that cannot
	// decouple label production from what the map actually shows. Declared
	// here, consumed once axis 01 puts labels on the read path (W7) — without
	// the cap that seam would import 8.400–84.000 inference calls per cycle at
	// the target scale.
	LabelBudget int `key:"root_map.label_budget" env:"CTX_ROOT_MAP_LABEL_BUDGET" default:"0" mut:"hot" tenancy:"global-only"`
	// The three super_* knobs belong to the meta-cluster level (W-F, P3) and
	// ship declared-without-consumer, exactly like the C0 precedent: the
	// namespace is complete from the first wave that owns it.
	SuperEnabled       bool    `key:"root_map.super_enabled" env:"CTX_ROOT_MAP_SUPER_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`
	SuperMinResolution float64 `key:"root_map.super_min_resolution" env:"CTX_ROOT_MAP_SUPER_MIN_RESOLUTION" default:"0.2" mut:"hot" tenancy:"global-only"`
	SuperMaxNodes      int     `key:"root_map.super_max_nodes" env:"CTX_ROOT_MAP_SUPER_MAX_NODES" default:"20000" mut:"hot" parse:"strict" tenancy:"global-only"`
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
	// centroid table (migration 128) exists and is filled — an empty table is a
	// documented state, not an error: share_centroid is then 0 and the fusion
	// falls back onto the seed arm scaled by (1-centroid_weight).
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
	// (UD-02-03): below it the centroid read is an exact scan — no recall
	// question, no index churn — above it C8 builds the HNSW index.
	//
	// 5.000, CALIBRATED AGAINST THE C8 MEASUREMENT, not the 50.000 placeholder
	// C0 shipped. Two numbers moved it (centroid_cost_integration_test.go,
	// reproducible): halfvec(1024) is 2052 B and therefore lands JUST over the
	// 2-kB TOAST threshold, so every centroid is an out-of-line read — 3.604 B
	// touched per row on a full scan, 1,7× the "~2 kB/row ⇒ ~170 MB @83.000" the
	// design assumed; and the measured exact-scan p95 holds the 25 ms retrieval
	// budget up to ≈3.300 centroids on a warm cache. 5.000 keeps the exact scan
	// where it is genuinely cheaper than an index and hands over before the hot
	// path pays for it. At 50.000 one probe would have scanned ≈172 MB.
	CentroidANNThreshold int `key:"cluster.centroid_ann_threshold" env:"CTX_CLUSTER_CENTROID_ANN_THRESHOLD" default:"5000" mut:"hot" tenancy:"global-only"`
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
	// SemanticFloor is the post-fusion confidence gate (E-M6). The 4-Way RRF
	// fusion is purely RANK-based: the semantic arm always returns its 75
	// nearest neighbours no matter how far away they are, so an off-topic query
	// still produces a rank-1 semantic hit (0.45/61 = 0.0074) that a single
	// lexical graze lifts past ConfidentThreshold. The result set then looks
	// answerable, and the only thing that says otherwise is a full synthesis
	// call whose entire output is a refusal.
	//
	// The floor is the distance signal the fusion throws away: a MINIMUM cosine
	// similarity the best embedding-compared result must reach before the
	// question is worth an LLM call. It is deliberately NOT a candidate filter
	// inside the arm (that kills recall on DE↔EN and terse phrasings and acts
	// BEFORE fusion) — it reads the fused set and only decides go/no-go.
	//
	// 0 = off, and off is the shipped default: the separating value is a
	// property of the CORPUS (how far the nearest neighbour of a foreign
	// question lands), so it can only be set from a measurement of the corpus
	// it guards. Range is [0,1) — see V26 in validate.go.
	//
	// Measured on the live corpus (2026-08-25, 39 of the 47 eval queries):
	// off-topic questions top out at 0.407 nearest-neighbour similarity, the
	// weakest genuine question sits at 0.437, and nothing lies between them —
	// 0.42 separates the two classes with room on both sides. Those are ONE
	// corpus's numbers and deliberately not the default: a floor shipped
	// pre-armed would start refusing questions on installs nobody measured.
	//
	// tenant-overridable like the two thresholds above and for the same reason:
	// it changes only how that tenant's OWN queries are answered.
	SemanticFloor float64 `key:"query.semantic_floor" env:"CTX_SEMANTIC_FLOOR" default:"0" mut:"hot" tenancy:"tenant-overridable"`
}

// DigestConfig is the LINEAR topic map's remaining knob (plan-cluster-topicmap
// design/02 §4.6, wave W-E) — one key, and it exists to retire the artefact it
// governs.
//
// The map has one line per BLOCK: 80.103 characters for 1.215 blocks today,
// ~113 MB at the 10M target, built by materialising the whole corpus in memory
// inside a 512 MiB container. Its replacement (root_map.*) has one line per
// CLUSTER. The three modes are the migration path, not a preference:
//
//	full — today's behaviour, byte for byte. The shipped default: the switch is
//	       an operational step after the root map has proven itself live, never
//	       a side effect of a deploy.
//	stub — the linear map stops growing and becomes a ~300 B pointer to the root
//	       map. Consumers that search for it (`ctx search index query:topic-map`,
//	       which is what the ctx-digest skill recommends) still find something
//	       that tells them WHERE to look — the reason this is a stub and not an
//	       archival (E2-02/E9-02: the stub carries the transition).
//	off  — no digest write at all. The existing block stays exactly as it is.
//
// global-only: the digest is an offline background job over a shared pipeline
// (the config.go tenancy rule names scheduler cadences and offline jobs
// explicitly), and a per-tenant mode would make "is the linear map still being
// built" unanswerable for the operator who has to retire it.
type DigestConfig struct {
	Mode string `key:"digest.mode" env:"CTX_DIGEST_MODE" default:"full" mut:"hot" tenancy:"global-only"`
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
	// BlobStageMaxBytes caps the DECODED payload one confirm_writes key may
	// have STAGED on the MCP blob_store tool (W02-8/N-28). It exists because
	// the staged path holds the authoritative payload SERVER-SIDE, in
	// context_pending_writes, until the write is confirmed or its TTL
	// (writes.confirm_ttl) runs out — a 50 MB upload, the blob surface's own
	// ceiling, would sit in a JSONB column for that whole window, and a key
	// that never confirms could park one per distinct payload.
	//
	// A payload above the cap is REFUSED with a named reason. It is
	// deliberately NOT written directly instead: the flag's whole point is
	// that this principal's writes go through a confirmation, and a size that
	// quietly restores the direct path would be a bypass of exactly the gate
	// the operator switched on.
	//
	// 0 does NOT mean "unlimited" here — it disables blob STAGING entirely, so
	// a confirm_writes key cannot store blobs at all. That is the fail-closed
	// reading of the value, and it is the one an operator can act on: the
	// alternative (0 = no cap) would turn the key that bounds a server-held
	// buffer into the switch that unbounds it. The rate-limit siblings' 0-is-
	// off convention does not carry — it governs a counter, not a buffer.
	//
	// Default 1 MiB: it holds the ~180 kB of one externalized tool-payload
	// batch with room to spare, and 1 MiB of base64 in a JSONB column is a
	// bounded cost per staged write. Settings-only and tenant-overridable like
	// its PoolConfig siblings.
	BlobStageMaxBytes int `key:"pool.blob_stage_max_bytes" env:"-" default:"1048576" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
	// BlobScanMaxBytes caps how much of a DECODED blob payload the credentials
	// detector reads on the shared blob write core (W02-9/BP-8). The block path
	// needs no such key — a block is capped at 50 KB, so its scan is free; a
	// blob is capped at 50 MB, and the detector is regex work over every byte
	// of it on a synchronous write path.
	//
	// A payload above the cap is STORED, with metadata.sensitivity='unscanned'.
	// That is deliberately not fail-closed, and it is the same decision the
	// non-UTF-8 case makes: the live corpus is binary uploads and the 61 blobs
	// that predate any Go write path, and refusing them — or calling them
	// credentials — would turn a scanner into an outage. The unscanned state is
	// WRITTEN rather than left implicit, so the limit is a visible property of
	// the row instead of something a reader has to infer from a missing field.
	//
	// 0 means the scan is OFF: every payload is stored 'unscanned'. Unlike
	// pool.blob_stage_max_bytes, where 0 is the fail-closed reading (no
	// staging), 0 here can only ever mean "do not look" — there is no payload
	// size at which "scan nothing" protects anything. Negative is range garbage
	// in the only direction that matters (it would read as a configured byte
	// count while the runtime treated it as off), so it is a boot abort / 422.
	//
	// Default 16 MiB: it covers every payload this surface is built for by
	// three orders of magnitude (an externalized tool-payload batch is ~180 kB)
	// while keeping the worst case a bounded fraction of the 50 MB ceiling.
	// Settings-only and tenant-overridable like its PoolConfig siblings.
	BlobScanMaxBytes int `key:"pool.blob_scan_max_bytes" env:"-" default:"16777216" mut:"hot" parse:"strict" tenancy:"tenant-overridable"`
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
	//
	// The 29 backend tuple keys retired in β3–β8 went the OTHER way, deliberately
	// (E4, design/01 §8): a delete migration removes their rows in every scope
	// and an `unset` audit row records each removal. The precedent above does not
	// carry, and the difference is the reason: gaming.* were two non-sensitive
	// global-only booleans with no env var, no documented deployment surface and
	// no foreign users. The tuple keys were the historically primary
	// configuration surface — six of them secret_ref-capable and
	// tenant-overridable, all of them printed in .env.example and compose. A left
	// row there is not just noise: an inert *.api_key row's secret_ref stops
	// counting as a reference (handler/sealbox.go referencedBy reads the
	// registry's sensitive set), so the secret it points at loses its delete
	// guard while the row remains invisible to every list and UI.
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
	// Default 1min base / 24h cap (design/04 §4.4). Validated by V21
	// (validateEmbedBackoff, issue #38): 0 is an ERROR — the consumer reads
	// it as "retry immediately", a tight loop against a failing backend.
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
	// (design §4.4). Validated by V21 like its backfill sibling: 0 is an
	// ERROR (retry-immediately tight loop).
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

// DistillConfig is the ctxd distiller arm (Achse A03, design/03): a scheduler
// arm that reads a FOREIGN, read-only SQLite state.db of an agent runtime,
// selects archived tool output from it and distills it into insight blocks.
//
// THIS GROUP HAS NO CONSUMER YET (wave W03-3). It is the policy half of the
// doctrine the design states in §4: mechanism = code, policy = data. Every
// number that can change with operation — cadence, budgets, thresholds, path,
// category, scope — is a key here; every number that can only change with a
// code change — the prompt rune budget, the quote minimum — is a constant with
// a test (promptguard.BudgetDistill).
//
// EVERY key is tenancy:"global-only", and the reason is stronger than the
// registry's fail-closed default (§5.4): a state.db is a SINGLE artifact of a
// SINGLE operator. Running the arm over the tenant iteration would write the
// same foreign content into several scopes. The arm does not iterate tenants,
// so no key here has a per-tenant reading.
//
// Mutability follows the arm's structure, which the design copies from
// recall_check (§4.2, "Struktur exakt nach recall_check.go"): the snapshot is
// re-read once per iteration, so every gate, budget and threshold is
// mut:"hot" — including the master switch, exactly as recall_check.enabled is.
// Enabled is NOT restart-class despite dream.enabled being the source of its
// DEFAULT (false, §4.2 gate 0): dream's arm decides at process start whether
// to run, this arm evaluates gate 0 per tick and books a skip_reason for it,
// and a restart-class master switch would make that gate unreachable code.
//
// The default is OFF, and that is a load-bearing default, not caution: a
// vanilla ctx install has no agent runtime next to it, and E03-1 requires that
// ctx stays complete without one. A default-on arm would accumulate a journal
// of "source unreachable" on every install that never asked for a distiller.
type DistillConfig struct {
	// Enabled is gate 0 (§4.2). A disabled arm writes NO journal row at all —
	// only a debug log — which is what keeps a vanilla install's journal empty.
	Enabled bool `key:"distill.enabled" env:"CTX_DISTILL_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`

	// ── Source identity ────────────────────────────────────────────────────
	//
	// SourcePath is the read-only state.db the arm opens per tick (gate 1).
	// EMPTY is the default and means "no source configured": nothing to open,
	// the arm stays inert. That empty default is the second half of the
	// default-off posture — an install that flips distill.enabled without
	// naming a path still cannot reach a foreign file.
	//
	// The path is NOT part of the journal's source identity (SourceLabel is,
	// below): a path in a data row would be an infrastructure statement in a
	// data row, and a mount change would silently tear the watermark
	// derivation apart (design §3.1).
	SourcePath string `key:"distill.source_path" env:"CTX_DISTILL_SOURCE_PATH" default:"" mut:"hot" tenancy:"global-only"`
	// SourceLabel is the stable half of the journal's source_key
	// ("<label>:<session_id>", §3.1) and therefore of the DERIVED watermark.
	// Changing it renames every source: the new key has no journal history, so
	// the arm restarts that source at initial_backfill_rows. That consequence
	// is on the key's description, because it is the operator-visible half.
	SourceLabel string `key:"distill.source_label" env:"CTX_DISTILL_SOURCE_LABEL" default:"hermes" mut:"hot" tenancy:"global-only"`

	// ── The ctx-checkpoint source (design D-02, wave A02-4) ────────────────
	//
	// The arm's SECOND source, and the one that needs no foreign file: the
	// compaction checkpoints this store writes about its own sessions. It gets
	// its own switch, its own label and its own quiet gate rather than reusing
	// the three above, because the two sources differ in every property those
	// keys carry — one is a read-only SQLite file of an agent runtime, the
	// other is rows of context_blocks — and a single set of keys would force
	// one number to mean two things.
	//
	// LIKE THE GROUP AROUND IT, THIS HALF HAS NO CONSUMER YET (A02-4 ships the
	// schema and the rules; the reader is A02-3, the arm A02-5). The rules come
	// WITH the keys for the reason the group doc gives one screen up: a rule
	// added after the key would be a rule added against an already-configurable
	// value.
	//
	// CtxEnabled is the per-source master switch, default OFF for the same
	// load-bearing reason distill.enabled is: an install that has never asked
	// for a distiller must not start deriving blocks from its own transcripts.
	CtxEnabled bool `key:"distill.ctx_enabled" env:"CTX_DISTILL_CTX_ENABLED" default:"false" mut:"hot" tenancy:"global-only"`
	// CtxSourceLabel is the stable half of THIS source's journal source_key,
	// exactly what SourceLabel is for the state.db source. The two must never
	// be the same word: one source_key means one watermark series, and two
	// sources sharing it would advance each other's watermark — the ranges in
	// between are then skipped in silence, not re-read. The validator refuses
	// the collision (case- and space-folded) before it can happen.
	CtxSourceLabel string `key:"distill.ctx_source_label" env:"CTX_DISTILL_CTX_SOURCE_LABEL" default:"ctx-checkpoint" mut:"hot" tenancy:"global-only"`
	// CtxQuietFor is this source's quiet gate in SECONDS — the counterpart of
	// SessionQuietFor, and a separate key because the two measure different
	// things: SessionQuietFor reads the age of the youngest live row in the
	// foreign state.db, this one the age of the youngest checkpoint of a root
	// session.
	//
	// 30 min, derived rather than inherited (decision EA-5): the inherited
	// 10 min was reasoned against a compaction distance of "~2 h 27 min", but
	// the measured median distance is 11.0 min (n = 157). At 10 min the gate
	// would open right before the next compaction in half the cases — i.e.
	// during active work. 30 min sits well above that p50 and far below p95
	// (381 min); much above ~60 min the gate would never fire in a continuous
	// session.
	CtxQuietFor time.Duration `key:"distill.ctx_quiet_for" env:"CTX_DISTILL_CTX_QUIET_FOR" default:"1800" mut:"hot" tenancy:"global-only"`
	// CtxSessionHorizon caps the ctx source's CANDIDATE aggregation to roots
	// whose newest manifest falls inside this window, in SECONDS. It is the
	// arm's half of a reader option (ctxcheckpoint.Options.SessionHorizon):
	// A02-4 deliberately shipped the reader's other values without this one,
	// because the reader reads no configuration — the arm hands it every value
	// (F1 layering rule), and the key belongs to the wave that wires them.
	//
	// It exists because the candidate query is the one query of this source
	// whose cost tracks the corpus rather than the session: GROUP BY … ORDER BY
	// max(created_at) DESC LIMIT n has no skip-scan behaviour in PostgreSQL, so
	// without a window it degenerates into a recurring full scan as the
	// checkpoint corpus grows — and that corpus has no retention path.
	//
	// 30 days, and the number is a measured margin rather than a round one: on
	// a one-million-row fixture derived from the live ingest rate (140.1
	// checkpoint blocks/day) the planner's crossover sits between 1.26 % and
	// 0.42 % selectivity, and the 30-day window selects 0.42 % — the first
	// value below the crossover, where the plan switches to idx_context_created
	// and the cost falls by two orders of magnitude (A02-3 gate, measured
	// series). What the cap buys therefore depends on the ingest RATE, not on
	// the row count. 0 means no cap and is legal: it is the pre-horizon
	// behaviour and the honest setting for a corpus small enough not to need
	// one.
	//
	// THE COST IS EXPLICIT: a root whose newest manifest is older than the
	// window disappears from the CANDIDATE list. Reading it still works — Head,
	// HasNew and Read take a session id and never consult the horizon.
	CtxSessionHorizon time.Duration `key:"distill.ctx_session_horizon" env:"CTX_DISTILL_CTX_SESSION_HORIZON" default:"2592000" mut:"hot" tenancy:"global-only"`

	// ── Cadence and gates (§4.2) ───────────────────────────────────────────
	//
	// Interval is the tick cadence in SECONDS (house convention:
	// parseDurationSeconds, unitless seconds). 15 min, and deliberately NOT
	// anchored to a wall clock the way runDailySynthesis (03:00) and
	// runRecallCheck (offpeak_hour) are: compaction is event-driven, and a
	// night window would mean the insights of a working session reach the
	// corpus the next morning — after the context cut they exist for. The
	// off-peak behavior comes from the gates, not from the clock.
	Interval time.Duration `key:"distill.interval" env:"CTX_DISTILL_INTERVAL" default:"900" mut:"hot" tenancy:"global-only"`
	// SessionQuietFor is gate 3 (§4.2), in seconds: how long the youngest live
	// row of a session must have been quiet before the arm touches that
	// session. It measures the load the ctx-side demand gate CANNOT see — a
	// human working in the foreign runtime, whose every keystroke needs the
	// same decode capacity. 0 turns the gate OFF, which is the documented
	// setting for the snapshot access variant (§4.0 variant S), where the
	// measurement would be stale by construction and therefore wrong rather
	// than merely imprecise.
	SessionQuietFor time.Duration `key:"distill.session_quiet_for" env:"CTX_DISTILL_SESSION_QUIET_FOR" default:"600" mut:"hot" tenancy:"global-only"`
	// MaxSessionsPerRun caps how many sources one tick may touch; the arm
	// rotates over the rest round-robin (§6.3). This bounds the READ order,
	// not the call budget — those are two mechanisms (spend_max_calls is the
	// other one), and the design names conflating them as a first-draft error.
	MaxSessionsPerRun int `key:"distill.max_sessions_per_run" env:"CTX_DISTILL_MAX_SESSIONS_PER_RUN" default:"4" mut:"hot" tenancy:"global-only"`

	// ── Selection (§4.3) ───────────────────────────────────────────────────
	//
	// RowsPerRead is the mandatory LIMIT on every read of the foreign file
	// (§6.3 consequence 1). The foreign schema carries no index the arm may
	// rely on, so an unbounded read is the one shape that turns a background
	// tick into a multi-second scan of a multi-GB file.
	RowsPerRead int `key:"distill.rows_per_read" env:"CTX_DISTILL_ROWS_PER_READ" default:"400" mut:"hot" tenancy:"global-only"`
	// MinRowRunes drops rows without substance before they cost anything
	// (exit codes, "ok", empty tool answers). Measured, not guessed: the mean
	// tool row is ~2 183 chars, and the tool families under 200 runes are
	// exactly the ones whose answers carry no insight.
	MinRowRunes int `key:"distill.min_row_runes" env:"CTX_DISTILL_MIN_ROW_RUNES" default:"200" mut:"hot" tenancy:"global-only"`
	// MaxRowRunes is the HEAD cap per surviving row. Head, not tail: the
	// live distribution has max 54 998 at mean 2 183, and the outliers are
	// terminal dumps and entity listings whose value sits in the head
	// (command, first hits) and whose tail is repetition.
	//
	// Coupled to promptguard.BudgetDistill through V24 (validate.go) and
	// through the static gate TestDistillDefaultsFitPromptBudget: this
	// value times RowsPerCall plus the rule reserve must fit the budget.
	MaxRowRunes int `key:"distill.max_row_runes" env:"CTX_DISTILL_MAX_ROW_RUNES" default:"4000" mut:"hot" tenancy:"global-only"`
	// RowsPerCall is the batch size, i.e. the UPPER BOUND of rows per LLM
	// call; promptguard.Assemble is the safety net that cuts earlier for
	// unusually long rows (§4.3 rule 6, "whichever hits first"). It is the
	// second input of the V24 budget coupling, which is what makes it a real
	// key with a test instead of a number in a comment.
	RowsPerCall int `key:"distill.rows_per_call" env:"CTX_DISTILL_ROWS_PER_CALL" default:"5" mut:"hot" tenancy:"global-only"`
	// DryRunDir is where the arm writes the DRY-RUN dump of the chunks its
	// selection kept, one file per run, while there is no LLM call and no block
	// write yet (wave A02-6).
	//
	// THE DUMP IS AN EGRESS CHANNEL, and the default is chosen against that
	// (§5 BA13). It carries exactly the material the credential detector did
	// NOT catch — label-adjacent hex runs, partial secrets next to [REDACTED],
	// paraphrases — for up to 165 MB of private session prose. The first draft
	// of the design put it under .project/, which is a git submodule with a
	// GitHub remote that is routinely pushed; the target therefore has to lie
	// OUTSIDE every git working copy, and the arm refuses one that does not
	// (a relative path and a path inside a working tree are both refused at
	// the point of use, not only in this validator's half).
	//
	// EMPTY TURNS THE PLAINTEXT DUMP OFF, and that is a supported setting
	// rather than a broken one: BA13's own resolution is that the auditability
	// argument runs primarily over the COUNTED figures and the chunk hashes —
	// both of which stay in the run journal and the dedup ledger — and that
	// plaintext is for an explicit operator request. An operator who wants the
	// numbers without the prose empties this key.
	DryRunDir string `key:"distill.dryrun_dir" env:"CTX_DISTILL_DRYRUN_DIR" default:"/var/lib/ctx/distill-dryrun" mut:"hot" tenancy:"global-only"`

	// InitialBackfillRows is the cold-start depth when a source is seen for
	// the first time and the journal derives watermark 0: the arm starts N
	// ARCHIVED ROWS OF THAT SESSION below its head. 0 (the default) = start at
	// the head, i.e. ignore the backlog — the honest default against a
	// multi-GB file whose whole history would otherwise enter the spend guard
	// in one run.
	//
	// The unit is ROWS OF THE SESSION, never an id delta: ids are a GLOBAL
	// autoincrement over every session of the same file (live: 307 sessions in
	// one file), so subtracting a row count from a global id yields a fraction
	// of N on interleaved sessions. The key says rows, so it must mean rows —
	// the semantics gate for this belongs to the reader wave (W03-2/§3.2).
	InitialBackfillRows int `key:"distill.initial_backfill_rows" env:"CTX_DISTILL_INITIAL_BACKFILL_ROWS" default:"0" mut:"hot" tenancy:"global-only"`

	// ── The call (§4.4) ────────────────────────────────────────────────────
	//
	// LocalOnly keeps the distill call on local/LAN backends regardless of
	// trust. Default TRUE, and it is the mitigation of bruch path B5: raw tool
	// output is the most hostile foreign text in the system — it contains
	// verbatim what a terminal read from the internet, plus the hostnames,
	// paths and usernames of a private infrastructure. llm.ChainCall.LocalOnly
	// discards external backends AFTER the trust gate, so this holds even
	// against a full-trust external row.
	//
	// It covers the CALL only. The block's life after the write (embed
	// backfill, dream, digest, synthesis) has no per-block locality switch in
	// ctx — that chain is sensitivity x trust, and trust is a property of the
	// backend row (§5.5). BlockSensitivity below is that half's lever.
	LocalOnly bool `key:"distill.local_only" env:"CTX_DISTILL_LOCAL_ONLY" default:"true" mut:"hot" tenancy:"global-only"`
	// CallTimeout is the default per-call wire timeout in seconds; a timeouts
	// entry on the serving backend row takes precedence, same relation
	// dream.temporal_timeout has.
	//
	// Derived from the two neighbours rather than set: graph_overview
	// .label_timeout is 90 s for a digest-role call of ~24 titles at 128
	// predict tokens. One distill batch carries up to BudgetDistill runes of
	// prefill (~13 333 tokens) and 640 predict tokens — several times the
	// label's work on both sides — so 180 s is twice the label ceiling and
	// still a fifth of the default interval, i.e. a hung call can never
	// outlast its own cadence.
	CallTimeout time.Duration `key:"distill.call_timeout" env:"CTX_DISTILL_CALL_TIMEOUT" default:"180" mut:"hot" tenancy:"global-only"`
	// NumPredict is the answer budget of one distill call, in tokens. A KEY and
	// not a constant, and the distinction is the group's own doctrine: the
	// number is a COST POLICY, never a security floor. The predecessor design
	// carried 640 as a Go constant; EA-8 settled on 512 as "enough for 4-6
	// insights with their quotes".
	//
	// 1536 SINCE A02-8c, AND THE CORRECTION IS THE MEASUREMENT EA-8 DID NOT
	// HAVE. A02-M2 ran the arm against spark-chat over a live excerpt: 51 of 97
	// calls stopped AT the ceiling (finish_reason="length", completion_tokens =
	// 512) and lost 243 complete insight objects with the broken envelope. What
	// that run could NOT show is the true need — a censored sample says nothing
	// above its own cap — so A02-8c replayed 20 of those recorded prompts
	// against the same endpoint with the cap raised out of the way: every one
	// came back "stop" and parsed, needing 438…1357 tokens (p50 699, p90 1158).
	// 1536 clears the measured maximum with ~13 % of head room; extrapolated
	// over the 97-call population the length quote falls from 47,3 % to 0 %.
	// The extrapolation validates itself — it reproduces 47,3 % where the run
	// measured 52,6 %.
	//
	// The ceiling does not buy tokens (generated tokens are what is paid), it
	// bounds the WORST CASE — and ~82 % of a call's cost is decode, so the
	// worst case is where the money is. Both sides of raising it are measured:
	// the AVERAGE call decodes ~29 % more (439 → 568 tokens, because answers now
	// finish instead of being cut), while the worst case goes from ~17 to ~42
	// GPU-s per call (§6.2 constants as A02-M2 remeasured them). Both stay far
	// inside distill.call_timeout; what an operator sees is fewer calls per
	// distill.spend_max_gpu_seconds window, against roughly twice the yield per
	// call. That the number CAN be corrected without a build is exactly why it
	// is a key — and it is also why the arm no longer DEPENDS on it being right:
	// since A02-8c a cut answer keeps the objects the model finished
	// (distillSalvage) instead of losing the call.
	NumPredict int `key:"distill.num_predict" env:"CTX_DISTILL_NUM_PREDICT" default:"1536" mut:"hot" tenancy:"global-only"`

	// ── The write path (§4.5) ──────────────────────────────────────────────
	//
	// Scope is the target scope of the insight blocks. EMPTY (the default) is
	// the INHERITANCE path — effectiveHomeScope over scheduler.home_scope,
	// like every other arm. "shared" is refused with 422 by V22 (validate.go),
	// fail-closed BEFORE operation: shared would be, in this one case, a
	// propagation path for foreign content across the tenant border.
	//
	// The key alone is not the whole guard, and the design is explicit about
	// why: it only validates what is EXPLICITLY set, never what is inherited.
	// The runtime half — the inherited path resolving to shared — is gate 5 of
	// the arm (skipped/scope_forbidden), built in W03-5.
	Scope string `key:"distill.scope" env:"CTX_DISTILL_SCOPE" default:"" mut:"hot" tenancy:"global-only"`
	// Category is the block category the insights are written to. It is also
	// half of the upsert identity (category, title, scope), so changing it
	// starts a new series rather than rewriting the old one.
	Category string `key:"distill.category" env:"CTX_DISTILL_CATEGORY" default:"session-insights" mut:"hot" tenancy:"global-only"`
	// BlockType is the block-type registry row every insight block is written
	// under. Explicit and not inherited from the classifier, because the type
	// decides three properties of the block that the arm itself cannot: whether
	// it is retrievable at all, whether it is retrievable UNDAMPED, and whether
	// the dedup guard may archive the very originals it quotes.
	//
	// Default "insight" — the derived layer's own type name (masterplan K3
	// shortened it from "session-insight"; derived.TypeInsight is the code-side
	// constant). A key rather than a constant for the reason distill.category
	// is one: the operator surface stays honest about what the arm writes, and
	// the validator keeps the value inside the derived layer instead of the key
	// being a way out of it.
	//
	// The value is normalized (trim + lower) and then checked against the
	// COMPILED registry: it must name a type, that type must not guard, must
	// not be full-pass, and may only be excluded if it belongs to the derived
	// layer. The exception exists because the derived types start excluded by
	// board decision E-4 until the visibility pilots flip them.
	BlockType string `key:"distill.block_type" env:"CTX_DISTILL_BLOCK_TYPE" default:"insight" mut:"hot" tenancy:"global-only"`
	// CheckpointCategory is where the arm LOOKS for the checkpoint manifest it
	// links its blocks to (metadata.manifest_id, §4.5). A neighbour's category,
	// not its own — hence a key: if that neighbour renames its default, this
	// follows without a code change. No hit is not an error; manifest_id stays
	// NULL and the block is written anyway (the distiller is the checkpoint's
	// neighbour, not its child).
	CheckpointCategory string `key:"distill.checkpoint_category" env:"CTX_DISTILL_CHECKPOINT_CATEGORY" default:"compaction-checkpoints" mut:"hot" tenancy:"global-only"`
	// MaxBlockRunes caps one insight block. 6 000 is above the ~1-1.5k
	// zettelkasten doctrine on purpose (decision E03-4): an insight block is an
	// EVIDENCE COLLECTION (claim plus quote per line), not one concept. E03-4
	// settled the number as revisable, which is exactly what a key is.
	//
	// ITS LOWER END IS TWO FLOORS, NOT ONE (wave W-L4). A shard above the first
	// carries a longer frame than shard 1: the title suffix " — Teil <n>" and
	// the chain line naming its predecessor, together ~133 runes at today's
	// title length. The smallest value at which a range can still grow is
	// therefore HIGHER for shard 2 than for shard 1, and in the band between the
	// two floors shard 1 takes an insight while none of its successors can. The
	// arm answers that band by RESTING — it refuses a hand-over whose successor
	// could place nothing, ends the run partial/budget and holds the material
	// back, so the range stands still (measured at 1 900 runes: one hand-over,
	// then no further block) and resumes the moment this key is raised. Before
	// that refusal the same band wrote ONE EMPTY SHARD PER TICK up to the chain
	// bound of 256, and a written block stays corpus. Nothing in the band is
	// lost; what it costs is one read plus the first blind call per tick, which
	// is C3-1's irreducible boundary and the spend guard's business.
	// At the default 6 000 no shard of any ordinal comes near either floor.
	MaxBlockRunes int `key:"distill.max_block_runes" env:"CTX_DISTILL_MAX_BLOCK_RUNES" default:"6000" mut:"hot" tenancy:"global-only"`
	// MaxBlocksPerRoot is the shard cap of amendment C4-2 A.4 (b): how many
	// blocks ONE (root, watermark_from) range may grow to. Since wave W-L2 a
	// full block hands over to the next shard instead of ending the run, so
	// MaxBlockRunes bounds one BLOCK and no longer the layer — this key is the
	// only thing that bounds the chain itself.
	//
	// 0 IS "NO CAP", NEVER "NO BLOCKS", exactly like SpendMaxCalls' 0 is the
	// guard off and not a zero budget, and it is the default because decision
	// E5-2 says the layer "öffnet sich mit dem Material". The cost brake lives
	// in the spend guard, which counts GPU seconds; a block-count cap cannot see
	// cost and would brake the productive roots first (A.8 einwand 2). V25
	// refuses the negative for the house reason: it renders as a configured size
	// while acting as an off-switch.
	//
	// It is a NOT-AUS, not a steering knob, and E-7 applies to whoever sets it:
	// at the 6-9 shards per range the backfill projection expects (A.7 b) a cap
	// has to sit far above that to never bind in ordinary operation — A.4 (b)
	// names 64 as the smallest number that does, not 10.
	//
	// WHAT BINDING LOOKS LIKE, because a not-aus that binds should be recognised
	// rather than guessed: at run start the arm answers skipped/budget and reads
	// nothing; mid-run it answers partial/budget and HOLDS the material back —
	// the chunks whose insights found no shard stay out of the dedup ledger and
	// the watermark does not advance over them. Such a range then repeats that
	// answer on every tick until the cap is raised. Nothing is lost, and nothing
	// progresses either.
	//
	// A HOLDING TICK IS FREE ONLY WHERE max_block_runes CARRIES AN INSIGHT. Above
	// that threshold the rune meter refuses the next call before it is paid for,
	// so the tick costs a read and nothing else. Below it — a cap under the yield
	// of one blind first call — the batch is re-read and re-bought every tick
	// (measured: 20 ticks, 20 calls, no claim, at 1 800 runes). That is C3-1's
	// irreducible first-call boundary and not a property of this key: the
	// uncapped run pays the same rate on the same material, and the backstop is
	// the spend guard, which counts GPU seconds.
	//
	// IT IS ALSO THE KEY TO THE CHAIN'S UNCONDITIONAL BOUND. Reading a range,
	// walking its title chain and writing it are all bounded at 256 shards even
	// with this key at 0; setting it ABOVE 256 raises all three together, so a
	// range that legitimately needs a longer chain is a settings change rather
	// than a deletion.
	//
	// The name says "per root", the axis is the RANGE. That is the amendment's
	// own use of it (A.3 a makes it the search bound of the ascending probe over
	// distillBlockTitle(root, wm, n), which is per (root, watermark_from)), and
	// it is the only reading that does not turn the not-aus into a regular
	// brake: a cap over ALL ranges of a root would silence a root permanently as
	// soon as its watermark had moved often enough, which is the deadlock W-L2
	// exists to end.
	MaxBlocksPerRoot int `key:"distill.max_blocks_per_root" env:"CTX_DISTILL_MAX_BLOCKS_PER_ROOT" default:"0" mut:"hot" tenancy:"global-only"`
	// BlockSensitivity is the sensitivity stamped on every insight block, and
	// the only lever over the block's LIFE CYCLE (§5.5, bruch path B11): every
	// later consumer — embed backfill, dream, digest, query synthesis — derives
	// its required trust from the block's sensitivity, and none of them sets
	// LocalOnly. Without an explicit value the write would fall back to the DDL
	// default, which is the shape in which a forgotten assignment goes silent.
	//
	// Default "credentials" per decision E03-7 (operator: "configurable,
	// default: like credentials"). That is the deliberate deviation from the
	// design's "personal": it closes the no-credentials external egress path.
	// It does NOT close a full-trust external row — the design says so plainly
	// (§5.5: no sensitivity value excludes both live external rows), and the
	// remaining path is the documented exception the W03-9 egress gate carries,
	// not a claim this key makes.
	//
	// Hard floor "internal", enforced by V23: "public" is a 422. NOT
	// guard:"sensitivity-downgrade" — the floor already fails closed at the
	// bottom, and the guard would additionally turn the legitimate
	// credentials -> personal move into a confirm dance, which the wave gate
	// explicitly requires to be a plain accept.
	BlockSensitivity backends.Sensitivity `key:"distill.block_sensitivity" env:"CTX_DISTILL_BLOCK_SENSITIVITY" default:"credentials" mut:"hot" tenancy:"global-only"`

	// ── The substance floor (§4.4, wave C5-E) ──────────────────────────────
	//
	// NoveltyFloor is the per-claim minimum of derived.Adequacy's `novelty` —
	// the share of a claim's tokens that do NOT stand in the quote it cites.
	// A claim below it is discarded in the write path instead of written, and
	// the discard is counted in distill_run.rej_novelty (migration 151).
	//
	// WHY THIS IS A KEY AND THE GOODHART THRESHOLDS ARE NOT. derived's own
	// comment refuses a config key for GoodhartMinNovelty in as many words: "a
	// gate whose threshold is a settings write is not a gate (W19)". That
	// refusal is about the REPORT's verdict — the instrument by which the arm
	// is judged, which an operator must not be able to soften. This key is the
	// opposite direction: it makes the write path STRICTER than the report's
	// verdict, and its 0 does not soften a verdict but restores the behaviour
	// every measurement so far ran under. The report keeps judging at
	// GoodhartMinNovelty either way.
	//
	// DEFAULT 0,15 IS derived.GoodhartMinNovelty, deliberately and not by
	// coincidence: the report's below_floor_share counts exactly the claims
	// this floor discards, so the gate and the instrument measure ONE border
	// rather than two. The coupling is pinned by a test rather than left to
	// this sentence (distill_c5e_test.go), because a struct tag cannot carry a
	// constant. Wave C5-A-M measured what that border holds on the root stand:
	// p10 = 0,0385, 27,1 % of published claims below 0,15 and 5,85 % at exactly
	// 0 — verbatim quote copies that every one of G1-G7 passes, because each of
	// them is a perfectly anchored citation (adequacy.go: "G0-G7 cannot catch
	// that").
	//
	// 0 IS THE DOCUMENTED OFF-SWITCH, the same reading distill.retention_days
	// and the two spend ceilings give their own zero: the floor is not applied,
	// the counter stays 0, and the arm behaves exactly as it did before this
	// wave. It is fail-safe in the direction that matters — an unset value
	// yields the registry default and therefore the floor, and only an explicit
	// 0 turns it off.
	//
	// The upper bound is 1 and V33 refuses anything above it: novelty is a
	// share of a token set, so a floor above 1 rejects every claim the arm
	// ever produces while the settings surface keeps rendering it as a
	// threshold — the "renders as configured, acts as an off-switch" class this
	// file refuses everywhere else, here in its sharpest form (it would not
	// disable the arm, it would silently empty it).
	//
	// Hot like the rest of the group: the arm resolves it into the tick's
	// snapshot (distillCallOpts), so a change reaches the next tick and never
	// the one in flight.
	NoveltyFloor float64 `key:"distill.novelty_floor" env:"CTX_DISTILL_NOVELTY_FLOOR" default:"0.15" mut:"hot" tenancy:"global-only"`

	// ── The spend guard (§4.6) ─────────────────────────────────────────────
	//
	// The division of labor is explicit and taken from the reference
	// implementation: BREAKER = failures, GUARD = success without an end.
	//
	// SpendWindow is the window the guard counts calls in, in seconds. 1 h sits
	// well below the measured compaction distance (~2 h 27 min), so two healthy
	// generations never fall into the same window.
	SpendWindow time.Duration `key:"distill.spend_window" env:"CTX_DISTILL_SPEND_WINDOW" default:"3600" mut:"hot" tenancy:"global-only"`
	// SpendMaxCalls is the call ceiling inside that window, counted DURABLY
	// over context_llm_log rather than in process — a restart loop is the one
	// state in which an in-process window would be blind.
	//
	// 40: one generation is ~25 calls, plus head room for a second in the same
	// window. Whoever produces a THIRD compacts every 20 minutes, which is a
	// loop and not a rhythm, and withdrawal is the right answer there.
	//
	// 0 is the documented KILL SWITCH — guard off, effective from the next
	// tick, because the snapshot is re-read per iteration. Negative is not a
	// second off-switch but a value that renders as configured while acting as
	// off, so V25 rejects it.
	SpendMaxCalls int `key:"distill.spend_max_calls" env:"CTX_DISTILL_SPEND_MAX_CALLS" default:"40" mut:"hot" tenancy:"global-only"`
	// SpendMaxGPUSeconds is the SHARP half of the same window, and it exists
	// because the call axis cannot see the cost it is supposed to cap (NA-12):
	// at the measured 38,92 ms per output token a 640-token answer costs ten
	// times a 64-token one, and the call window counts both as 1. A guard that
	// cannot see the expensive axis bewacht the wrong one, so the two run side
	// by side — the call ceiling as the coarse second deck, this one as the
	// ceiling that actually binds (EA-2).
	//
	// 240 GPU-seconds per window is the PROPORTIONALITY statement of §4.6.1 in
	// a number: the largest single consumer measured over 24 h is dream-eval at
	// 5 642 GPU-s (5 642 / 24 = 235 GPU-s per hour), and the distiller is
	// Veredelung next to a product that already runs. It may therefore reach —
	// never exceed — today's biggest background consumer. At the §6.2 cost band
	// that is 24 calls per hour at the cheap end and 7 at the expensive one,
	// against a call ceiling of 40 that stands unchanged: whichever axis binds
	// first is the one that describes the real load.
	//
	// Counted in whole seconds against a millisecond column on purpose: the
	// operator's unit here is "how much of the GPU hour does this arm get", and
	// a millisecond key would render six digits for a question asked in dozens.
	//
	// 0 is the kill switch of THIS axis alone, exactly like spend_max_calls is
	// of its own — both at 0 is the guard off, and V25 refuses the negative
	// that would render as a configured budget while acting as an off-switch.
	SpendMaxGPUSeconds int `key:"distill.spend_max_gpu_seconds" env:"CTX_DISTILL_SPEND_MAX_GPU_SECONDS" default:"240" mut:"hot" tenancy:"global-only"`
	// SpendBackoff is how long a source rests after tripping the budget, in
	// seconds. 2 h spans a typical compaction distance: a loop is braked
	// effectively, a healthy rhythm loses at most one generation — and that one
	// is not lost but POSTPONED, because the watermark stays put and the next
	// run picks the range up.
	SpendBackoff time.Duration `key:"distill.spend_backoff" env:"CTX_DISTILL_SPEND_BACKOFF" default:"7200" mut:"hot" tenancy:"global-only"`
	// BreakerFailures is how many consecutive failures open the in-process
	// circuit breaker per backend. 3, not the reference implementation's 2: a
	// failed attempt here is consequence-free (the arm is fail-open), and the
	// evidence gate produces an additional legitimate failure class ("the model
	// returned only unsupported insights"), so 2 would be too sharp.
	BreakerFailures int `key:"distill.breaker_failures" env:"CTX_DISTILL_BREAKER_FAILURES" default:"3" mut:"hot" tenancy:"global-only"`
	// BreakerCooldown is how long an open breaker rests, in seconds. Exactly
	// one interval, and the number carries a statement: after a failure series
	// the NEXT tick is skipped and the one after it is a real attempt. (The
	// first draft wrote 10 min "rounded up to a multiple of the interval" —
	// 10 is not a multiple of 15, so the reason was empty.)
	BreakerCooldown time.Duration `key:"distill.breaker_cooldown" env:"CTX_DISTILL_BREAKER_COOLDOWN" default:"900" mut:"hot" tenancy:"global-only"`

	// ── Retention (§6.2) ───────────────────────────────────────────────────
	//
	// The BLOCKS get no retention on purpose — they are knowledge, not
	// telemetry, and live under the ordinary guard/archive regime. These two
	// keys cover the arm's own bookkeeping, both as one line in the 6 h janitor
	// bundle, both with the recall_check no-op semantics: 0 keeps forever.
	//
	// RetentionDays is the run journal's horizon.
	RetentionDays int `key:"distill.retention_days" env:"CTX_DISTILL_RETENTION_DAYS" default:"90" mut:"hot" tenancy:"global-only"`
	// SeenRetentionDays is the cross-run dedup ledger's horizon — shorter,
	// because a content hash is only useful for as long as the same output
	// keeps coming back.
	SeenRetentionDays int `key:"distill.seen_retention_days" env:"CTX_DISTILL_SEEN_RETENTION_DAYS" default:"30" mut:"hot" tenancy:"global-only"`
}

// Source reports the origin of a registry key in this snapshot:
// "env" | "default" (F2 adds "settings"). Unknown keys return "".
func (c *Config) Source(key string) string {
	return c.sources[key]
}

// ChatBackend died with the primary chat tuple in β8 — the last of the five
// tuple accessors, and the one the other four resolved against. It returned a
// backends.Backend built 1:1 from Chat.*; its last caller was the boot-time
// backend seed that β1 removed, which is why the accessor outlived every one of
// its runtime consumers. A synthesis backend is chosen from the pool now
// (role synthesis, backends.Pool), and the row IS the backends.Backend this
// used to assemble.

// DreamBackend died with the dream chat tuple in β6. It resolved Model, NumCtx
// and Think onto Chat.* whenever the dream field was zero (Delta 1), with V1
// rejecting the one divergence that costs VRAM rather than correctness — a
// second runner of the same model on the same host. Both statements are pool
// statements now: the dream dispatch resolves the role-dream chain and nothing
// else (dream/router.go chat, gated by dream.go:141), every row carrying its
// own model_map and num_ctx — and "one runner or two" is a property of the rows
// an operator enables, not of a field pair Validate could compare.

// DreamEmbedBackend died with the dream_embed tuple in β5, EmbedBackend with
// the embed tuple in β7. The first resolved its tuple field by field onto the
// second, with V12 rejecting the one case the fallback could not decide safely
// (an own host but an inherited credential); the second was the query-path
// embedding tuple, 1:1 from Embed.*, and its last caller was the settings-side
// EmbedCacheCoupledChanged that went with it. The pool answers all of it now:
// dream/router.go:130-142 chains role dream-embed when a row carries it and
// falls back to role embed otherwise, a pool row's api_key_ref never travels
// to another row's host, and which model the embed chain would ask is
// Pool.PrimaryModel(RoleEmbed) since α2.

// RerankRRF converts the rerank group to the rrf-stage parameter struct.
func (c *Config) RerankRRF() rrf.RerankConfig {
	return rrf.RerankConfig{
		Enabled:     c.Rerank.Enabled,
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
// Only the fields the stage actually CONSUMES are mapped. cluster.inject_max
// stays unmapped until C9 wires it (design/03 §4.9): a knob that is visibly
// plumbed but inert is a trap for whoever tunes it.
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

		// C8: the centroid READ arm. centroid_enabled/weight/top_k are ranking
		// knobs (tenant-overridable); centroid_ef_search steers the shared index
		// and is global-only — same split as MaxStaleness above. The centroid
		// BUILD knobs (centroid_build/timeout/batch/work_mem/ann_threshold) are
		// deliberately absent: they belong to the background arm
		// (overview.CentroidOptions), and a retrieval struct carrying build policy
		// would invite exactly the coupling K5 forbids.
		CentroidEnabled:  c.Cluster.CentroidEnabled,
		CentroidWeight:   c.Cluster.CentroidWeight,
		CentroidTopK:     c.Cluster.CentroidTopK,
		CentroidEFSearch: c.ClusterOps.CentroidEFSearch,

		// C9: the one knob that can change the result SET instead of its order.
		// Default 0 = the stage never injects; arming it follows the eval
		// measurement, never a deploy.
		InjectMax: c.Cluster.InjectMax,
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
