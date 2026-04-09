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
	DreamEnabled bool
	DreamHost    string
	DreamAPIKey  string
	DreamModel   string
	DreamNumCtx  int
	DreamThink   string // "true", "false", or "" (fallback: ChatThink)

	// Timezone for temporal resolution (e.g. "Europe/Berlin").
	// Defaults to UTC. Ensures "heute" resolves correctly for the user's timezone.
	Timezone *time.Location

	// HTTP Server
	ListenAddr string
}

// LoadConfig reads configuration from environment variables with sensible defaults.
func LoadConfig() (Config, error) {
	port, err := getEnvInt("CONTEXT_DB_PORT", 5432)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CONTEXT_DB_PORT: %w", err)
	}

	// Shared Ollama host as fallback for per-pipeline hosts.
	ollamaHost := getEnv("OLLAMA_HOST", "http://localhost:11434")

	embedDims, err := getEnvInt("OLLAMA_EMBED_DIMS", 4096)
	if err != nil {
		return Config{}, fmt.Errorf("parsing OLLAMA_EMBED_DIMS: %w", err)
	}

	embedNumCtx, err := getEnvInt("OLLAMA_EMBED_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing OLLAMA_EMBED_NUM_CTX: %w", err)
	}

	chatNumCtx, err := getEnvInt("OLLAMA_CHAT_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing OLLAMA_CHAT_NUM_CTX: %w", err)
	}

	dreamNumCtx, err := getEnvInt("OLLAMA_DREAM_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing OLLAMA_DREAM_NUM_CTX: %w", err)
	}

	// Per-pipeline think mode. OLLAMA_THINK is the shared default.
	chatThink := getEnv("OLLAMA_THINK", "false")
	dreamThink := getEnv("OLLAMA_DREAM_THINK", "")

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

		// Embedding pipeline: CTX_EMBED_* overrides, fallback to OLLAMA_*
		EmbedHost:   getEnv("CTX_EMBED_HOST", ollamaHost),
		EmbedAPIKey: getEnv("CTX_EMBED_API_KEY", ""),
		EmbedModel:  getEnv("CTX_EMBED_MODEL", getEnv("OLLAMA_EMBED_MODEL", "qwen3-embedding:8b")),
		EmbedDims:   embedDims,
		EmbedNumCtx: embedNumCtx,

		// Synthesis pipeline: CTX_CHAT_* overrides, fallback to OLLAMA_*
		ChatHost:   getEnv("CTX_CHAT_HOST", ollamaHost),
		ChatAPIKey: getEnv("CTX_CHAT_API_KEY", ""),
		ChatModel:  getEnv("CTX_CHAT_MODEL", getEnv("OLLAMA_CHAT_MODEL", "qwen3.5:9b")),
		ChatNumCtx: chatNumCtx,
		ChatThink:  getEnv("CTX_CHAT_THINK", chatThink),

		RerankEnabled: getEnv("CTX_RERANK_ENABLED", "false") == "true",

		// Dream pipeline: CTX_DREAM_* overrides, fallback to OLLAMA_*
		DreamEnabled: getEnv("CTX_DREAM_ENABLED", "false") == "true",
		DreamHost:    getEnv("CTX_DREAM_HOST", ollamaHost),
		DreamAPIKey:  getEnv("CTX_DREAM_API_KEY", ""),
		DreamModel:   getEnv("CTX_DREAM_MODEL", getEnv("OLLAMA_DREAM_MODEL", "")),
		DreamNumCtx:  dreamNumCtx,
		DreamThink:   getEnv("CTX_DREAM_THINK", dreamThink),

		Timezone: tz,

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
