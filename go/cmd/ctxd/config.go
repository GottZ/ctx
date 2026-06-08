package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/rrf"
)

// defaultListenAddr is the default HTTP listen address.
const defaultListenAddr = ":8080"

// Config holds all application configuration loaded from environment variables.
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

	// Reranker
	RerankEnabled bool

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

// LoadConfig reads configuration from environment variables with sensible defaults.
func LoadConfig() (Config, error) {
	port, err := getEnvInt("CONTEXT_DB_PORT", 5432)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CONTEXT_DB_PORT: %w", err)
	}

	embedDims, err := getEnvInt("CTX_EMBED_DIMS", 4096)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_EMBED_DIMS: %w", err)
	}

	embedNumCtx, err := getEnvInt("CTX_EMBED_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_EMBED_NUM_CTX: %w", err)
	}

	chatNumCtx, err := getEnvInt("CTX_CHAT_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_CHAT_NUM_CTX: %w", err)
	}

	dreamNumCtx, err := getEnvInt("CTX_DREAM_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_DREAM_NUM_CTX: %w", err)
	}

	rateLimitWrite, err := getEnvInt("CTX_RATE_LIMIT_WRITE", 100)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_RATE_LIMIT_WRITE: %w", err)
	}

	rateLimitRead, err := getEnvInt("CTX_RATE_LIMIT_READ", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_RATE_LIMIT_READ: %w", err)
	}

	tz := time.UTC
	if tzName := getEnv("CTX_TIMEZONE", ""); tzName != "" {
		loc, err := time.LoadLocation(tzName)
		if err != nil {
			return Config{}, fmt.Errorf("parsing CTX_TIMEZONE %q: %w", tzName, err)
		}
		tz = loc
	}

	cfg := Config{
		ContextDB:     getEnv("CONTEXT_DB", "context_store"),
		ContextDBUser: getEnv("CONTEXT_DB_USER", "context_user"),
		ContextDBPass: getEnv("CONTEXT_DB_PASSWORD", ""),
		ContextDBHost: getEnv("CONTEXT_DB_HOST", "localhost"),
		ContextDBPort: port,
		ContextDBSSL:  getEnv("CONTEXT_DB_SSLMODE", "disable"),

		EmbedHost:     getEnv("CTX_EMBED_HOST", "http://localhost:11434"),
		EmbedAPIKey:   getEnv("CTX_EMBED_API_KEY", ""),
		EmbedProtocol: getEnv("CTX_EMBED_PROTOCOL", "ollama"),
		EmbedModel:    getEnv("CTX_EMBED_MODEL", "qwen3-embedding:8b"),
		EmbedDims:     embedDims,
		EmbedNumCtx:   embedNumCtx,

		ChatHost:     getEnv("CTX_CHAT_HOST", "http://localhost:11434"),
		ChatAPIKey:   getEnv("CTX_CHAT_API_KEY", ""),
		ChatProtocol: getEnv("CTX_CHAT_PROTOCOL", "ollama"),
		ChatModel:    getEnv("CTX_CHAT_MODEL", "qwen3.5:9b"),
		ChatNumCtx:   chatNumCtx,
		ChatThink:    getEnv("CTX_CHAT_THINK", "false"),

		RerankEnabled: getEnv("CTX_RERANK_ENABLED", "false") == "true",

		// Dream-graph expansion (Wave 1). Defaults = OFF and the calibration
		// from the Wave-1 design; every value is an env-overridable sweep knob.
		GraphExpandEnabled:          getEnv("CTX_GRAPH_EXPAND_ENABLED", "false") == "true",
		GraphDirected:               getEnv("CTX_GRAPH_EXPAND_DIRECTED", "true") == "true",
		GraphHopDepth:               getEnvIntSafe("CTX_GRAPH_EXPAND_HOP_DEPTH", 1),
		GraphSeedCount:              getEnvIntSafe("CTX_GRAPH_EXPAND_SEED_COUNT", 5),
		GraphSeedScoreFloor:         getEnvFloatSafe("CTX_GRAPH_EXPAND_SEED_SCORE_FLOOR", 0.5),
		GraphPerSeedCap:             getEnvIntSafe("CTX_GRAPH_EXPAND_PER_SEED_CAP", 3),
		GraphMaxInjected:            getEnvIntSafe("CTX_GRAPH_EXPAND_MAX_INJECTED", 10),
		GraphMinConfidence:          getEnvFloatSafe("CTX_GRAPH_EXPAND_MIN_CONFIDENCE", 0.75),
		GraphMinConfidenceRecurrent: getEnvFloatSafe("CTX_GRAPH_EXPAND_MIN_CONFIDENCE_RECURRENT", 0.8),
		GraphBoostWeight:            getEnvFloatSafe("CTX_GRAPH_EXPAND_BOOST_WEIGHT", 0.20),
		GraphHubDamping:             getEnv("CTX_GRAPH_EXPAND_HUB_DAMPING", "true") == "true",
		GraphWeightTopical:          getEnvFloatSafe("CTX_GRAPH_EXPAND_WEIGHT_TOPICAL", 0.5),
		GraphWeightFactual:          getEnvFloatSafe("CTX_GRAPH_EXPAND_WEIGHT_FACTUAL", 0.9),
		GraphWeightCausal:           getEnvFloatSafe("CTX_GRAPH_EXPAND_WEIGHT_CAUSAL", 0.9),
		GraphWeightRecurrent:        getEnvFloatSafe("CTX_GRAPH_EXPAND_WEIGHT_RECURRENT", 1.0),

		DreamEnabled:            getEnv("CTX_DREAM_ENABLED", "false") == "true",
		DreamHost:               getEnv("CTX_DREAM_HOST", "http://localhost:11434"),
		DreamAPIKey:             getEnv("CTX_DREAM_API_KEY", ""),
		DreamProtocol:           getEnv("CTX_DREAM_PROTOCOL", "ollama"),
		DreamModel:              getEnv("CTX_DREAM_MODEL", ""),
		DreamNumCtx:             dreamNumCtx,
		DreamThink:              getEnv("CTX_DREAM_THINK", ""),
		DreamEmbedHost:          getEnv("CTX_DREAM_EMBED_HOST", ""),
		DreamEmbedAPIKey:        getEnv("CTX_DREAM_EMBED_API_KEY", ""),
		DreamEmbedProtocol:      getEnv("CTX_DREAM_EMBED_PROTOCOL", ""),
		DreamEmbedModel:         getEnv("CTX_DREAM_EMBED_MODEL", ""),
		DreamEmbedNumCtx:        getEnvIntSafe("CTX_DREAM_EMBED_NUM_CTX", 0),
		DreamIdleWait:           getEnvIntSafe("CTX_DREAM_IDLE_WAIT", 20),
		DreamParallelism:        getEnvIntSafe("CTX_DREAM_PARALLELISM", 1),
		DreamBackoffMode:        getEnv("CTX_DREAM_BACKOFF_MODE", "exp"),
		DreamBackoffFactor:      getEnvFloatSafe("CTX_DREAM_BACKOFF_FACTOR", 1.6),
		DreamBackoffGrace:       getEnvIntSafe("CTX_DREAM_BACKOFF_GRACE", 0),
		DreamBackoffCapHours:    getEnvCooldownHours("CTX_DREAM_BACKOFF_CAP", 45*24),
		DreamBackoffMinHours:    getEnvCooldownHours("CTX_DREAM_BACKOFF_MIN", 12),
		DreamBackoffInertOffset: getEnvIntSafe("CTX_DREAM_BACKOFF_INERT_OFFSET", 7),

		Timezone: tz,

		RateLimitWrite: rateLimitWrite,
		RateLimitRead:  rateLimitRead,

		ListenAddr: getEnv("LISTEN_ADDR", defaultListenAddr),
	}

	if cfg.ContextDBPass == "" {
		return Config{}, fmt.Errorf("CONTEXT_DB_PASSWORD is required")
	}

	return cfg, nil
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

func getEnvFloatSafe(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

// cooldownUnitHours maps the domain duration suffixes to hours. Deliberately
// NOT Go's time.ParseDuration: that uses m=minutes / s=seconds (meaningless for
// re-dream intervals, and m=minutes is a months trap). Here the units are the
// ones that make sense for cooldowns — h/d/w/m/y — with m = month (30d).
var cooldownUnitHours = map[byte]float64{
	'h': 1,
	'd': 24,
	'w': 24 * 7,
	'm': 24 * 30,
	'y': 24 * 365,
}

// parseCooldownHours parses a cooldown duration to hours. Accepts a unit suffix
// h|d|w|m|y (e.g. "12h", "45d", "1w", "1m", "1y") or a bare number (interpreted
// as hours). Returns (hours, true) on success; (0, false) on a malformed value
// so callers fall back to their default.
func parseCooldownHours(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	last := s[len(s)-1]
	if mult, ok := cooldownUnitHours[last|0x20]; ok { // |0x20 lowercases ASCII letter
		n, err := strconv.ParseFloat(strings.TrimSpace(s[:len(s)-1]), 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n * mult, true
	}
	// No recognized suffix → bare number is hours.
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// getEnvCooldownHours reads a duration env var (h|d|w|m|y suffix, or bare hours)
// and returns it in hours, falling back on empty/malformed input.
func getEnvCooldownHours(key string, fallbackHours float64) float64 {
	if h, ok := parseCooldownHours(os.Getenv(key)); ok {
		return h
	}
	return fallbackHours
}

func getEnvIntSafe(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
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

// parseScopes parses a comma-separated scope list from an environment variable.
// Empty input or all-whitespace input falls back to defaultVal. Each scope is
// trim-whitespaced and empty parts are filtered out (e.g. "a,,b" → ["a","b"]).
// No further validation — v2.0.0 M045 dropped chk_scope so any non-empty
// string is a valid scope. Used by main.go to populate Scheduler.ReadScopes
// from CTX_READ_SCOPES.
func parseScopes(envVal string, defaultVal []string) []string {
	if envVal == "" {
		return defaultVal
	}
	parts := strings.Split(envVal, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return defaultVal
	}
	return result
}
