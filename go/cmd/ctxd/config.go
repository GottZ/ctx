package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

// defaultListenAddr is the default HTTP listen address.
const defaultListenAddr = ":8080"

// Config holds all application configuration loaded from environment variables.
type Config struct {
	// PostgreSQL — Context Store
	ContextDB       string
	ContextDBUser   string
	ContextDBPass   string
	ContextDBHost   string
	ContextDBPort   int
	ContextDBSSL    string

	// Embedding pipeline
	EmbedHost   string
	EmbedAPIKey string
	EmbedModel  string
	EmbedDims   int
	EmbedNumCtx int

	// Synthesis (Chat) pipeline
	ChatHost   string
	ChatAPIKey string
	ChatModel  string
	ChatNumCtx int
	ChatThink  string // "true", "false", or "" (omit from request)

	// Reranker
	RerankEnabled bool

	// Dream pipeline
	DreamEnabled     bool
	DreamHost        string
	DreamAPIKey      string
	DreamModel       string
	DreamNumCtx      int
	DreamThink       string // "true", "false", or "" (fallback: ChatThink)
	DreamEmbedHost   string // Separate embed host for Dream (empty = EmbedHost)
	DreamEmbedAPIKey string
	DreamEmbedModel  string
	DreamEmbedNumCtx int

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

		EmbedHost:   getEnv("CTX_EMBED_HOST", "http://localhost:11434"),
		EmbedAPIKey: getEnv("CTX_EMBED_API_KEY", ""),
		EmbedModel:  getEnv("CTX_EMBED_MODEL", "qwen3-embedding:8b"),
		EmbedDims:   embedDims,
		EmbedNumCtx: embedNumCtx,

		ChatHost:   getEnv("CTX_CHAT_HOST", "http://localhost:11434"),
		ChatAPIKey: getEnv("CTX_CHAT_API_KEY", ""),
		ChatModel:  getEnv("CTX_CHAT_MODEL", "qwen3.5:9b"),
		ChatNumCtx: chatNumCtx,
		ChatThink:  getEnv("CTX_CHAT_THINK", "false"),

		RerankEnabled: getEnv("CTX_RERANK_ENABLED", "false") == "true",

		DreamEnabled: getEnv("CTX_DREAM_ENABLED", "false") == "true",
		DreamHost:    getEnv("CTX_DREAM_HOST", "http://localhost:11434"),
		DreamAPIKey:  getEnv("CTX_DREAM_API_KEY", ""),
		DreamModel:   getEnv("CTX_DREAM_MODEL", ""),
		DreamNumCtx:  dreamNumCtx,
		DreamThink:       getEnv("CTX_DREAM_THINK", ""),
		DreamEmbedHost:   getEnv("CTX_DREAM_EMBED_HOST", ""),
		DreamEmbedAPIKey: getEnv("CTX_DREAM_EMBED_API_KEY", ""),
		DreamEmbedModel:  getEnv("CTX_DREAM_EMBED_MODEL", ""),
		DreamEmbedNumCtx: getEnvIntSafe("CTX_DREAM_EMBED_NUM_CTX", 0),

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
