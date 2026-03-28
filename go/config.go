package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
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

	// Ollama
	OllamaHost       string
	OllamaEmbedModel string
	OllamaEmbedDims  int
	OllamaChatModel  string

	// Reranker
	RerankEnabled bool

	// HTTP Server
	ListenAddr string
}

// LoadConfig reads configuration from environment variables with sensible defaults.
func LoadConfig() (Config, error) {
	port, err := getEnvInt("CONTEXT_DB_PORT", 5432)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CONTEXT_DB_PORT: %w", err)
	}

	embedDims, err := getEnvInt("OLLAMA_EMBED_DIMS", 4096)
	if err != nil {
		return Config{}, fmt.Errorf("parsing OLLAMA_EMBED_DIMS: %w", err)
	}

	cfg := Config{
		ContextDB:     getEnv("CONTEXT_DB", "context_store"),
		ContextDBUser: getEnv("CONTEXT_DB_USER", "context_user"),
		ContextDBPass: getEnv("CONTEXT_DB_PASSWORD", ""),
		ContextDBHost: getEnv("CONTEXT_DB_HOST", "localhost"),
		ContextDBPort: port,
		ContextDBSSL:  getEnv("CONTEXT_DB_SSLMODE", "disable"),

		OllamaHost:       getEnv("OLLAMA_HOST", "http://localhost:11434"),
		OllamaEmbedModel: getEnv("OLLAMA_EMBED_MODEL", "qwen3-embedding:8b"),
		OllamaEmbedDims:  embedDims,
		OllamaChatModel:  getEnv("OLLAMA_CHAT_MODEL", "qwen3.5:9b"),

		RerankEnabled: getEnv("CTX_RERANK_ENABLED", "false") == "true",

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
