package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
)

// defaultListenAddr is the default HTTP listen address.
const defaultListenAddr = ":8080"

// Config is the legacy flat view of the runtime configuration. F1-W1 bridge:
// it is no longer parsed here — loadConfig maps it from internal/config so
// every consumer (server.go, scheduler wiring) keeps its shape until the
// wave-by-wave store adoption (G09–G14); the bridge dies in F1-W7.
type Config struct {
	// PostgreSQL — Context Store
	ContextDB     string
	ContextDBUser string
	ContextDBPass string
	ContextDBHost string
	ContextDBPort int
	ContextDBSSL  string

	// Embedding pipeline
	EmbedHost     string
	EmbedAPIKey   string
	EmbedProtocol string // "ollama" (default) or "openai"
	EmbedModel    string
	EmbedDims     int
	EmbedNumCtx   int

	// Synthesis (Chat) pipeline
	ChatHost     string
	ChatAPIKey   string
	ChatProtocol string // "ollama" (default) or "openai"
	ChatModel    string
	ChatNumCtx   int
	ChatThink    string // "true", "false", or "" (omit from request)

	// Emergency chat fallback for the query path (synthesis only) when the
	// primary chat backend is unreachable at transport level. Empty host = off.
	ChatFallbackHost     string
	ChatFallbackAPIKey   string
	ChatFallbackProtocol string // default "openai" (llama.cpp CPU sidecar)
	ChatFallbackTimeoutS int    // seconds; CPU-27B synthesis runs 4.5-5.5 min

	// Reranker. RerankEnabled is the master gate for both paths. RerankHost
	// dispatches: empty => LLM-as-judge on the chat model; set => local
	// cross-encoder sidecar (Wave 2). The cross-encoder fields are sweep knobs.
	RerankEnabled     bool
	RerankHost        string
	RerankAPIKey      string
	RerankModel       string
	RerankMaxDocs     int
	RerankBlendWeight float64

	// Dream-graph expansion (GottZ Graph Expansion, Wave 1). Post-RRF stage that
	// 1-hop-expands the top RRF seeds along the positive Dream link types.
	// All fields are runtime knobs (the A/B sweep surface). Default = off.
	GraphExpandEnabled          bool
	GraphDirected               bool
	GraphHopDepth               int
	GraphSeedCount              int
	GraphSeedScoreFloor         float64
	GraphPerSeedCap             int
	GraphMaxInjected            int
	GraphMinConfidence          float64
	GraphMinConfidenceRecurrent float64
	GraphBoostWeight            float64
	GraphHubDamping             bool
	GraphWeightTopical          float64
	GraphWeightFactual          float64
	GraphWeightCausal           float64
	GraphWeightRecurrent        float64
	GraphNewPlacementFrac       float64

	// Dream pipeline
	DreamEnabled            bool
	DreamHost               string
	DreamAPIKey             string
	DreamProtocol           string // "ollama" (default) or "openai"
	DreamModel              string
	DreamNumCtx             int
	DreamThink              string // "true", "false", or "" (fallback: ChatThink)
	DreamEmbedHost          string // Separate embed host for Dream (empty = EmbedHost)
	DreamEmbedAPIKey        string
	DreamEmbedProtocol      string // "ollama" or "openai" (empty = EmbedProtocol)
	DreamEmbedModel         string
	DreamEmbedNumCtx        int
	DreamIdleWait           int     // seconds between dream cycles when idle (default 20)
	DreamParallelism        int     // concurrent dream cycles (default 1, max 16). PickBlock uses FOR UPDATE SKIP LOCKED so workers don't collide.
	DreamBackoffMode        string  // CTX_DREAM_BACKOFF_MODE: exp|log|linear|off
	DreamBackoffFactor      float64 // CTX_DREAM_BACKOFF_FACTOR
	DreamBackoffGrace       int     // CTX_DREAM_BACKOFF_GRACE
	DreamBackoffCapHours    float64 // CTX_DREAM_BACKOFF_CAP (h|d|w|m|y, e.g. 45d)
	DreamBackoffMinHours    float64 // CTX_DREAM_BACKOFF_MIN (h|d|w|m|y, e.g. 12h)
	DreamBackoffInertOffset int     // CTX_DREAM_BACKOFF_INERT_OFFSET

	// Synthesis scoring thresholds + prompt version for the query path.
	// Pre-F1 these were read by llm's package init() (CTX_SCORE_THRESHOLD /
	// CTX_CONFIDENT_THRESHOLD / CTX_PROMPT_VERSION); F1-W2 routes them through
	// the registry (Query group) into llm.SynthesisSettings.
	ScoreThreshold     float64
	ConfidentThreshold float64
	PromptVersion      string

	// Timezone for temporal resolution (e.g. "Europe/Berlin").
	// Defaults to UTC. Ensures "heute" resolves correctly for the user's timezone.
	Timezone *time.Location

	// Rate limiting (per API key, per 60 seconds).
	// 0 = disabled.
	RateLimitWrite int // default 100
	RateLimitRead  int // default 0 (reverse proxy handles read limiting)

	// HTTP Server
	ListenAddr string
}

// loadConfig is the boot entry: env → internal/config (FromEnv + Validate) →
// legacy bridge view. It returns everything main needs — the legacy view for
// the unchanged consumers, the canonical config for the store + boot dump,
// and ALL issues (logged before any fail-fast). err is non-nil only for the
// bridge-local CTX_EMBED_DIMS parse (kept strict until F1-W7, Delta 4);
// SeverityError issues are the caller's fail-fast signal.
func loadConfig() (Config, *config.Config, []config.Issue, error) {
	cc, issues := config.FromEnv()
	issues = append(issues, config.Validate(cc)...)
	legacy, err := legacyConfig(cc)
	return legacy, cc, issues, err
}

// LoadConfig keeps the original (Config, error) contract on top of the new
// loader: any SeverityError surfaces as the error, exactly as the old
// fatal-parse and required-password paths did.
func LoadConfig() (Config, error) {
	legacy, _, issues, err := loadConfig()
	if err != nil {
		return Config{}, err
	}
	for _, is := range issues {
		if is.Severity == config.SeverityError {
			return Config{}, fmt.Errorf("config: %s: %s", is.Field, is.Msg)
		}
	}
	return legacy, nil
}

// legacyConfig maps the canonical config onto the legacy struct. The only
// remaining direct env read is CTX_EMBED_DIMS: the field is dead (no
// consumer) and deleted from internal/config, but its strict parse stays
// fatal here until wave 7 so waves 1-6 boot bit-identically (Delta 4).
func legacyConfig(cc *config.Config) (Config, error) {
	embedDims, err := getEnvInt("CTX_EMBED_DIMS", 4096)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_EMBED_DIMS: %w", err)
	}

	return Config{
		ContextDB:     cc.Server.DB,
		ContextDBUser: cc.Server.DBUser,
		ContextDBPass: cc.Server.DBPass,
		ContextDBHost: cc.Server.DBHost,
		ContextDBPort: cc.Server.DBPort,
		ContextDBSSL:  cc.Server.DBSSL,

		EmbedHost:     cc.Embed.Host,
		EmbedAPIKey:   cc.Embed.APIKey,
		EmbedProtocol: string(cc.Embed.Protocol),
		EmbedModel:    cc.Embed.Model,
		EmbedDims:     embedDims,
		EmbedNumCtx:   cc.Embed.NumCtx,

		ChatHost:     cc.Chat.Host,
		ChatAPIKey:   cc.Chat.APIKey,
		ChatProtocol: string(cc.Chat.Protocol),
		ChatModel:    cc.Chat.Model,
		ChatNumCtx:   cc.Chat.NumCtx,
		ChatThink:    string(cc.Chat.Think),

		ChatFallbackHost:     cc.Fallback.Host,
		ChatFallbackAPIKey:   cc.Fallback.APIKey,
		ChatFallbackProtocol: string(cc.Fallback.Protocol),
		ChatFallbackTimeoutS: int(cc.Fallback.Timeout / time.Second),

		RerankEnabled:     cc.Rerank.Enabled,
		RerankHost:        cc.Rerank.Host,
		RerankAPIKey:      cc.Rerank.APIKey,
		RerankModel:       cc.Rerank.Model,
		RerankMaxDocs:     cc.Rerank.MaxDocs,
		RerankBlendWeight: cc.Rerank.BlendWeight,

		GraphExpandEnabled:          cc.Graph.Enabled,
		GraphDirected:               cc.Graph.Directed,
		GraphHopDepth:               cc.Graph.HopDepth,
		GraphSeedCount:              cc.Graph.SeedCount,
		GraphSeedScoreFloor:         cc.Graph.SeedScoreFloor,
		GraphPerSeedCap:             cc.Graph.PerSeedCap,
		GraphMaxInjected:            cc.Graph.MaxInjected,
		GraphMinConfidence:          cc.Graph.MinConfidence,
		GraphMinConfidenceRecurrent: cc.Graph.MinConfidenceRecurrent,
		GraphBoostWeight:            cc.Graph.BoostWeight,
		GraphHubDamping:             cc.Graph.HubDamping,
		GraphWeightTopical:          cc.Graph.WeightTopical,
		GraphWeightFactual:          cc.Graph.WeightFactual,
		GraphWeightCausal:           cc.Graph.WeightCausal,
		GraphWeightRecurrent:        cc.Graph.WeightRecurrent,
		GraphNewPlacementFrac:       cc.Graph.NewPlacementFrac,

		DreamEnabled:            cc.Dream.Enabled,
		DreamHost:               cc.Dream.Host,
		DreamAPIKey:             cc.Dream.APIKey,
		DreamProtocol:           string(cc.Dream.Protocol),
		DreamModel:              cc.Dream.Model,
		DreamNumCtx:             cc.Dream.NumCtx,
		DreamThink:              string(cc.Dream.Think),
		DreamEmbedHost:          cc.Dream.Embed.Host,
		DreamEmbedAPIKey:        cc.Dream.Embed.APIKey,
		DreamEmbedProtocol:      string(cc.Dream.Embed.Protocol),
		DreamEmbedModel:         cc.Dream.Embed.Model,
		DreamEmbedNumCtx:        cc.Dream.Embed.NumCtx,
		DreamIdleWait:           int(cc.Dream.IdleWait / time.Second),
		DreamParallelism:        cc.Dream.Parallelism,
		DreamBackoffMode:        cc.Dream.Backoff.Mode,
		DreamBackoffFactor:      cc.Dream.Backoff.Factor,
		DreamBackoffGrace:       cc.Dream.Backoff.Grace,
		DreamBackoffCapHours:    float64(cc.Dream.Backoff.CapHours),
		DreamBackoffMinHours:    float64(cc.Dream.Backoff.MinHours),
		DreamBackoffInertOffset: cc.Dream.Backoff.InertOffset,

		ScoreThreshold:     cc.Query.ScoreThreshold,
		ConfidentThreshold: cc.Query.ConfidentThreshold,
		PromptVersion:      cc.Query.PromptVersion,

		Timezone: cc.Query.Timezone,

		RateLimitWrite: cc.Query.RateLimitWrite,
		RateLimitRead:  cc.Query.RateLimitRead,

		ListenAddr: cc.Server.ListenAddr,
	}, nil
}

// GraphConfig builds the rrf.GraphConfig for the Dream-graph expansion stage
// from the loaded Graph* env values.
func (c Config) GraphConfig() rrf.GraphConfig {
	return rrf.GraphConfig{
		Enabled:                c.GraphExpandEnabled,
		Directed:               c.GraphDirected,
		HopDepth:               c.GraphHopDepth,
		SeedCount:              c.GraphSeedCount,
		SeedScoreFloor:         c.GraphSeedScoreFloor,
		PerSeedCap:             c.GraphPerSeedCap,
		MaxInjected:            c.GraphMaxInjected,
		MinConfidence:          c.GraphMinConfidence,
		MinConfidenceRecurrent: c.GraphMinConfidenceRecurrent,
		BoostWeight:            c.GraphBoostWeight,
		HubDamping:             c.GraphHubDamping,
		WeightTopical:          c.GraphWeightTopical,
		WeightFactual:          c.GraphWeightFactual,
		WeightCausal:           c.GraphWeightCausal,
		WeightRecurrent:        c.GraphWeightRecurrent,
		NewPlacementFrac:       c.GraphNewPlacementFrac,
	}
}

// RerankConfig builds the rrf.RerankConfig for the post-RRF rerank stage from
// the loaded Rerank* env values.
func (c Config) RerankConfig() rrf.RerankConfig {
	return rrf.RerankConfig{
		Enabled:     c.RerankEnabled,
		Host:        c.RerankHost,
		APIKey:      c.RerankAPIKey,
		Model:       c.RerankModel,
		MaxDocs:     c.RerankMaxDocs,
		BlendWeight: c.RerankBlendWeight,
	}
}

// SynthesisSettings builds the llm.SynthesisSettings for the query-path
// synthesis stage from the loaded Query* values (F1-W2: constructor argument
// instead of llm package vars; moves onto the snapshot in F1-W4).
func (c Config) SynthesisSettings() llm.SynthesisSettings {
	return llm.SynthesisSettings{
		ScoreThreshold:     c.ScoreThreshold,
		ConfidentThreshold: c.ConfidentThreshold,
		PromptVersion:      c.PromptVersion,
	}
}

// DSN returns a PostgreSQL connection string for the context store database.
// User and password are URL-encoded to handle special characters.
func (c Config) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(c.ContextDBUser),
		url.QueryEscape(c.ContextDBPass),
		c.ContextDBHost,
		c.ContextDBPort,
		c.ContextDB,
		c.ContextDBSSL,
	)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseThinkMode converts a string to a *bool for the Ollama think parameter.
// "true" → &true, "false" → &false, "" → nil (omit from request).
func parseThinkMode(s string) *bool {
	switch s {
	case "true":
		t := true
		return &t
	case "false":
		t := false
		return &t
	default:
		return nil
	}
}

func getEnvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q for %s: %w", v, key, err)
	}
	return n, nil
}
