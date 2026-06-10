package main

import (
	"os"
	"testing"
)

// --- getEnv ---.

func TestGetEnv_FallbackOnMissing(t *testing.T) {
	// Use a key that definitely does not exist.
	key := "CTX_TEST_GETENV_MISSING_KEY_48291"
	_ = os.Unsetenv(key)
	if got := getEnv(key, "default"); got != "default" {
		t.Errorf("getEnv(%q, %q) = %q, want %q", key, "default", got, "default")
	}
}

func TestGetEnv_ReturnsSetValue(t *testing.T) {
	key := "CTX_TEST_GETENV_SET_KEY_48291"
	t.Setenv(key, "myvalue")
	if got := getEnv(key, "fallback"); got != "myvalue" {
		t.Errorf("getEnv(%q, %q) = %q, want %q", key, "fallback", got, "myvalue")
	}
}

func TestGetEnv_EmptyValueReturnsFallback(t *testing.T) {
	// os.Getenv returns "" for unset AND for empty-set.
	// getEnv treats "" as missing → returns fallback.
	key := "CTX_TEST_GETENV_EMPTY_48291"
	t.Setenv(key, "")
	if got := getEnv(key, "fallback"); got != "fallback" {
		t.Errorf("getEnv(%q, %q) with empty env = %q, want %q", key, "fallback", got, "fallback")
	}
}

func TestGetEnv_WhitespaceIsNotEmpty(t *testing.T) {
	key := "CTX_TEST_GETENV_WHITESPACE_48291"
	t.Setenv(key, "  ")
	if got := getEnv(key, "fallback"); got != "  " {
		t.Errorf("getEnv(%q, %q) with whitespace = %q, want %q", key, "fallback", got, "  ")
	}
}

func TestGetEnv_EmptyFallback(t *testing.T) {
	key := "CTX_TEST_GETENV_EMPTY_FALLBACK_48291"
	_ = os.Unsetenv(key)
	if got := getEnv(key, ""); got != "" {
		t.Errorf("getEnv(%q, %q) = %q, want empty", key, "", got)
	}
}

// --- getEnvInt ---.

func TestGetEnvInt_FallbackOnMissing(t *testing.T) {
	key := "CTX_TEST_GETENVINT_MISSING_48291"
	_ = os.Unsetenv(key)
	got, err := getEnvInt(key, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Errorf("getEnvInt(%q, 42) = %d, want 42", key, got)
	}
}

func TestGetEnvInt_ValidInteger(t *testing.T) {
	key := "CTX_TEST_GETENVINT_VALID_48291"
	t.Setenv(key, "8080")
	got, err := getEnvInt(key, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 8080 {
		t.Errorf("getEnvInt(%q, 0) = %d, want 8080", key, got)
	}
}

func TestGetEnvInt_Zero(t *testing.T) {
	key := "CTX_TEST_GETENVINT_ZERO_48291"
	t.Setenv(key, "0")
	got, err := getEnvInt(key, 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("getEnvInt(%q, 99) = %d, want 0", key, got)
	}
}

func TestGetEnvInt_Negative(t *testing.T) {
	key := "CTX_TEST_GETENVINT_NEG_48291"
	t.Setenv(key, "-1")
	got, err := getEnvInt(key, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != -1 {
		t.Errorf("getEnvInt(%q, 0) = %d, want -1", key, got)
	}
}

func TestGetEnvInt_InvalidString(t *testing.T) {
	key := "CTX_TEST_GETENVINT_INVALID_48291"
	t.Setenv(key, "not_a_number")
	_, err := getEnvInt(key, 0)
	if err == nil {
		t.Errorf("expected error for non-numeric value, got nil")
	}
}

func TestGetEnvInt_Float(t *testing.T) {
	key := "CTX_TEST_GETENVINT_FLOAT_48291"
	t.Setenv(key, "3.14")
	_, err := getEnvInt(key, 0)
	if err == nil {
		t.Errorf("expected error for float value, got nil")
	}
}

func TestGetEnvInt_EmptyReturnsDefault(t *testing.T) {
	key := "CTX_TEST_GETENVINT_EMPTY_48291"
	t.Setenv(key, "")
	got, err := getEnvInt(key, 5432)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5432 {
		t.Errorf("getEnvInt(%q, 5432) with empty env = %d, want 5432", key, got)
	}
}

func TestGetEnvInt_Whitespace(t *testing.T) {
	key := "CTX_TEST_GETENVINT_WHITESPACE_48291"
	t.Setenv(key, " 42 ")
	_, err := getEnvInt(key, 0)
	if err == nil {
		t.Errorf("expected error for whitespace-padded integer, got nil")
	}
}

func TestGetEnvInt_LeadingZeros(t *testing.T) {
	key := "CTX_TEST_GETENVINT_LEADING_48291"
	t.Setenv(key, "007")
	got, err := getEnvInt(key, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7 {
		t.Errorf("getEnvInt(%q, 0) = %d, want 7", key, got)
	}
}

func TestGetEnvInt_MaxInt(t *testing.T) {
	key := "CTX_TEST_GETENVINT_MAXINT_48291"
	t.Setenv(key, "9223372036854775807") // math.MaxInt64 on 64-bit
	got, err := getEnvInt(key, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 9223372036854775807 {
		t.Errorf("getEnvInt for MaxInt64 = %d", got)
	}
}

func TestGetEnvInt_Overflow(t *testing.T) {
	key := "CTX_TEST_GETENVINT_OVERFLOW_48291"
	t.Setenv(key, "99999999999999999999999")
	_, err := getEnvInt(key, 0)
	if err == nil {
		t.Errorf("expected error for integer overflow, got nil")
	}
}

// --- Config.DSN ---.

func TestConfig_DSN_Basic(t *testing.T) {
	cfg := Config{
		ContextDBUser: "user",
		ContextDBPass: "pass",
		ContextDBHost: "localhost",
		ContextDBPort: 5432,
		ContextDB:     "mydb",
		ContextDBSSL:  "disable",
	}
	want := "postgres://user:pass@localhost:5432/mydb?sslmode=disable"
	if got := cfg.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

func TestConfig_DSN_SpecialCharsInPassword(t *testing.T) {
	// Password with special chars should be URL-encoded.
	cfg := Config{
		ContextDBUser: "user",
		ContextDBPass: "p@ss:w0rd/slash",
		ContextDBHost: "db.host",
		ContextDBPort: 5433,
		ContextDB:     "ctx",
		ContextDBSSL:  "require",
	}
	got := cfg.DSN()
	// URL-encoded: @ -> %40, : -> %3A, / -> %2F
	if got != "postgres://user:p%40ss%3Aw0rd%2Fslash@db.host:5433/ctx?sslmode=require" {
		t.Errorf("DSN() = %q, special chars not properly encoded", got)
	}
}

func TestConfig_DSN_SpecialCharsInUser(t *testing.T) {
	cfg := Config{
		ContextDBUser: "admin@domain",
		ContextDBPass: "simple",
		ContextDBHost: "localhost",
		ContextDBPort: 5432,
		ContextDB:     "db",
		ContextDBSSL:  "disable",
	}
	got := cfg.DSN()
	if got != "postgres://admin%40domain:simple@localhost:5432/db?sslmode=disable" {
		t.Errorf("DSN() = %q, user not properly encoded", got)
	}
}

func TestConfig_DSN_EmptyPassword(t *testing.T) {
	cfg := Config{
		ContextDBUser: "user",
		ContextDBPass: "",
		ContextDBHost: "localhost",
		ContextDBPort: 5432,
		ContextDB:     "db",
		ContextDBSSL:  "disable",
	}
	want := "postgres://user:@localhost:5432/db?sslmode=disable"
	if got := cfg.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

func TestConfig_DSN_IPv6Host(t *testing.T) {
	cfg := Config{
		ContextDBUser: "user",
		ContextDBPass: "pass",
		ContextDBHost: "::1",
		ContextDBPort: 5432,
		ContextDB:     "db",
		ContextDBSSL:  "disable",
	}
	// Note: IPv6 in DSN without brackets — this is the current behavior.
	want := "postgres://user:pass@::1:5432/db?sslmode=disable"
	if got := cfg.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

func TestConfig_DSN_PortZero(t *testing.T) {
	cfg := Config{
		ContextDBUser: "user",
		ContextDBPass: "pass",
		ContextDBHost: "localhost",
		ContextDBPort: 0,
		ContextDB:     "db",
		ContextDBSSL:  "disable",
	}
	want := "postgres://user:pass@localhost:0/db?sslmode=disable"
	if got := cfg.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

func TestConfig_DSN_UnicodePassword(t *testing.T) {
	cfg := Config{
		ContextDBUser: "user",
		ContextDBPass: "p\u00e4sswort", // "passwort" with umlaut
		ContextDBHost: "localhost",
		ContextDBPort: 5432,
		ContextDB:     "db",
		ContextDBSSL:  "disable",
	}
	got := cfg.DSN()
	// Umlaut ä should be percent-encoded.
	if got == "" {
		t.Error("DSN() returned empty string")
	}
	// Verify it does not contain the raw umlaut.
	for _, r := range got {
		if r == '\u00e4' {
			t.Error("DSN() contains unencoded umlaut")
		}
	}
}

// --- LoadConfig ---.

func TestLoadConfig_MissingPassword(t *testing.T) {
	// Clear all relevant env vars, especially the password.
	for _, key := range []string{
		"CONTEXT_DB", "CONTEXT_DB_USER", "CONTEXT_DB_PASSWORD",
		"CONTEXT_DB_HOST", "CONTEXT_DB_PORT", "CONTEXT_DB_SSLMODE",
		"OLLAMA_HOST", "OLLAMA_EMBED_MODEL", "OLLAMA_EMBED_DIMS",
		"OLLAMA_CHAT_MODEL", "OLLAMA_THINK", "CTX_RERANK_ENABLED",
		"LISTEN_ADDR",
	} {
		t.Setenv(key, "")
	}

	_, err := LoadConfig()
	if err == nil {
		t.Error("expected error for missing CONTEXT_DB_PASSWORD, got nil")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	// Set only the required password, clear everything else.
	for _, key := range []string{
		"CONTEXT_DB", "CONTEXT_DB_USER",
		"CONTEXT_DB_HOST", "CONTEXT_DB_PORT", "CONTEXT_DB_SSLMODE",
		"OLLAMA_HOST", "OLLAMA_EMBED_MODEL", "OLLAMA_EMBED_DIMS",
		"OLLAMA_CHAT_MODEL", "OLLAMA_THINK", "CTX_RERANK_ENABLED",
		"LISTEN_ADDR",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ContextDB != "context_store" {
		t.Errorf("ContextDB default = %q, want %q", cfg.ContextDB, "context_store")
	}
	if cfg.ContextDBUser != "context_user" {
		t.Errorf("ContextDBUser default = %q, want %q", cfg.ContextDBUser, "context_user")
	}
	if cfg.ContextDBHost != "localhost" {
		t.Errorf("ContextDBHost default = %q, want %q", cfg.ContextDBHost, "localhost")
	}
	if cfg.ContextDBPort != 5432 {
		t.Errorf("ContextDBPort default = %d, want 5432", cfg.ContextDBPort)
	}
	if cfg.ContextDBSSL != "disable" {
		t.Errorf("ContextDBSSL default = %q, want %q", cfg.ContextDBSSL, "disable")
	}
	if cfg.EmbedHost != "http://localhost:11434" {
		t.Errorf("EmbedHost default = %q, want %q", cfg.EmbedHost, "http://localhost:11434")
	}
	if cfg.ChatHost != "http://localhost:11434" {
		t.Errorf("ChatHost default = %q, want %q", cfg.ChatHost, "http://localhost:11434")
	}
	if cfg.EmbedModel != "qwen3-embedding:8b" {
		t.Errorf("EmbedModel default = %q, want %q", cfg.EmbedModel, "qwen3-embedding:8b")
	}
	if cfg.EmbedDims != 4096 {
		t.Errorf("EmbedDims default = %d, want 4096", cfg.EmbedDims)
	}
	if cfg.ChatModel != "qwen3.5:9b" {
		t.Errorf("ChatModel default = %q, want %q", cfg.ChatModel, "qwen3.5:9b")
	}
	if cfg.ChatThink != "false" {
		t.Errorf("ChatThink default = %q, want %q", cfg.ChatThink, "false")
	}
	if cfg.EmbedAPIKey != "" {
		t.Errorf("EmbedAPIKey default = %q, want empty", cfg.EmbedAPIKey)
	}
	if cfg.ChatAPIKey != "" {
		t.Errorf("ChatAPIKey default = %q, want empty", cfg.ChatAPIKey)
	}
	if cfg.RerankEnabled != false {
		t.Errorf("RerankEnabled default = %v, want false", cfg.RerankEnabled)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr default = %q, want %q", cfg.ListenAddr, ":8080")
	}
}

func TestLoadConfig_GraphExpandDefaults(t *testing.T) {
	// Clear all graph env vars + required password so defaults apply.
	for _, key := range []string{
		"CTX_GRAPH_EXPAND_ENABLED", "CTX_GRAPH_EXPAND_DIRECTED",
		"CTX_GRAPH_EXPAND_HOP_DEPTH", "CTX_GRAPH_EXPAND_SEED_COUNT",
		"CTX_GRAPH_EXPAND_SEED_SCORE_FLOOR", "CTX_GRAPH_EXPAND_PER_SEED_CAP",
		"CTX_GRAPH_EXPAND_MAX_INJECTED", "CTX_GRAPH_EXPAND_MIN_CONFIDENCE",
		"CTX_GRAPH_EXPAND_MIN_CONFIDENCE_RECURRENT", "CTX_GRAPH_EXPAND_BOOST_WEIGHT",
		"CTX_GRAPH_EXPAND_HUB_DAMPING", "CTX_GRAPH_EXPAND_WEIGHT_TOPICAL",
		"CTX_GRAPH_EXPAND_WEIGHT_FACTUAL", "CTX_GRAPH_EXPAND_WEIGHT_CAUSAL",
		"CTX_GRAPH_EXPAND_WEIGHT_RECURRENT", "CTX_GRAPH_EXPAND_NEW_PLACEMENT_FRAC",
		"CONTEXT_DB_PORT", "CTX_EMBED_DIMS",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Stage off by default — byte-identical pipeline when unset.
	if cfg.GraphExpandEnabled {
		t.Error("GraphExpandEnabled default = true, want false")
	}
	if !cfg.GraphDirected {
		t.Error("GraphDirected default = false, want true")
	}
	if cfg.GraphHopDepth != 1 {
		t.Errorf("GraphHopDepth default = %d, want 1", cfg.GraphHopDepth)
	}
	if cfg.GraphSeedCount != 5 {
		t.Errorf("GraphSeedCount default = %d, want 5", cfg.GraphSeedCount)
	}
	if cfg.GraphSeedScoreFloor != 0.5 {
		t.Errorf("GraphSeedScoreFloor default = %v, want 0.5", cfg.GraphSeedScoreFloor)
	}
	if cfg.GraphPerSeedCap != 3 {
		t.Errorf("GraphPerSeedCap default = %d, want 3", cfg.GraphPerSeedCap)
	}
	if cfg.GraphMaxInjected != 10 {
		t.Errorf("GraphMaxInjected default = %d, want 10", cfg.GraphMaxInjected)
	}
	if cfg.GraphMinConfidence != 0.75 {
		t.Errorf("GraphMinConfidence default = %v, want 0.75", cfg.GraphMinConfidence)
	}
	if cfg.GraphMinConfidenceRecurrent != 0.8 {
		t.Errorf("GraphMinConfidenceRecurrent default = %v, want 0.8", cfg.GraphMinConfidenceRecurrent)
	}
	if cfg.GraphBoostWeight != 0.20 {
		t.Errorf("GraphBoostWeight default = %v, want 0.20", cfg.GraphBoostWeight)
	}
	if !cfg.GraphHubDamping {
		t.Error("GraphHubDamping default = false, want true")
	}
	if cfg.GraphWeightTopical != 0.5 {
		t.Errorf("GraphWeightTopical default = %v, want 0.5", cfg.GraphWeightTopical)
	}
	if cfg.GraphWeightFactual != 0.9 {
		t.Errorf("GraphWeightFactual default = %v, want 0.9", cfg.GraphWeightFactual)
	}
	if cfg.GraphWeightCausal != 0.9 {
		t.Errorf("GraphWeightCausal default = %v, want 0.9", cfg.GraphWeightCausal)
	}
	if cfg.GraphWeightRecurrent != 1.0 {
		t.Errorf("GraphWeightRecurrent default = %v, want 1.0", cfg.GraphWeightRecurrent)
	}
	if cfg.GraphNewPlacementFrac != 0.6 {
		t.Errorf("GraphNewPlacementFrac default = %v, want 0.6", cfg.GraphNewPlacementFrac)
	}

	// GraphConfig() must mirror the loaded values 1:1.
	gc := cfg.GraphConfig()
	if gc.Enabled != cfg.GraphExpandEnabled || gc.Directed != cfg.GraphDirected ||
		gc.HopDepth != cfg.GraphHopDepth || gc.SeedCount != cfg.GraphSeedCount ||
		gc.SeedScoreFloor != cfg.GraphSeedScoreFloor || gc.PerSeedCap != cfg.GraphPerSeedCap ||
		gc.MaxInjected != cfg.GraphMaxInjected || gc.MinConfidence != cfg.GraphMinConfidence ||
		gc.MinConfidenceRecurrent != cfg.GraphMinConfidenceRecurrent || gc.BoostWeight != cfg.GraphBoostWeight ||
		gc.HubDamping != cfg.GraphHubDamping || gc.WeightTopical != cfg.GraphWeightTopical ||
		gc.WeightFactual != cfg.GraphWeightFactual || gc.WeightCausal != cfg.GraphWeightCausal ||
		gc.WeightRecurrent != cfg.GraphWeightRecurrent || gc.NewPlacementFrac != cfg.GraphNewPlacementFrac {
		t.Errorf("GraphConfig() does not mirror loaded Config: %+v", gc)
	}
}

func TestLoadConfig_GraphExpandEnabled_True(t *testing.T) {
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")
	t.Setenv("CONTEXT_DB_PORT", "")
	t.Setenv("CTX_EMBED_DIMS", "")
	t.Setenv("CTX_GRAPH_EXPAND_ENABLED", "true")
	t.Setenv("CTX_GRAPH_EXPAND_DIRECTED", "false")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.GraphExpandEnabled {
		t.Error("GraphExpandEnabled should be true when CTX_GRAPH_EXPAND_ENABLED=true")
	}
	if cfg.GraphDirected {
		t.Error("GraphDirected should be false when CTX_GRAPH_EXPAND_DIRECTED=false")
	}
}

func TestLoadConfig_RerankEnabled_True(t *testing.T) {
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")
	t.Setenv("CTX_RERANK_ENABLED", "true")
	// Clear port and dims to use defaults.
	t.Setenv("CONTEXT_DB_PORT", "")
	t.Setenv("OLLAMA_EMBED_DIMS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.RerankEnabled {
		t.Error("RerankEnabled should be true when CTX_RERANK_ENABLED=true")
	}
}

func TestLoadConfig_RerankEnabled_One(t *testing.T) {
	// "1" is NOT "true" — should be false.
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")
	t.Setenv("CTX_RERANK_ENABLED", "1")
	t.Setenv("CONTEXT_DB_PORT", "")
	t.Setenv("OLLAMA_EMBED_DIMS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RerankEnabled {
		t.Error("RerankEnabled should be false when CTX_RERANK_ENABLED=1 (only 'true' activates)")
	}
}

func TestLoadConfig_RerankEnabled_TrueUppercase(t *testing.T) {
	// "TRUE" is NOT "true" — should be false.
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")
	t.Setenv("CTX_RERANK_ENABLED", "TRUE")
	t.Setenv("CONTEXT_DB_PORT", "")
	t.Setenv("OLLAMA_EMBED_DIMS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RerankEnabled {
		t.Error("RerankEnabled should be false when CTX_RERANK_ENABLED=TRUE (case-sensitive)")
	}
}

func TestLoadConfig_RerankConfigDefaults(t *testing.T) {
	for _, key := range []string{
		"CTX_RERANK_ENABLED", "CTX_RERANK_HOST", "CTX_RERANK_API_KEY",
		"CTX_RERANK_MODEL", "CTX_RERANK_MAX_DOCS", "CTX_RERANK_BLEND_WEIGHT",
		"CONTEXT_DB_PORT", "CTX_EMBED_DIMS",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Host empty by default → dispatch stays on the LLM judge (no cross-encoder).
	if cfg.RerankHost != "" {
		t.Errorf("RerankHost default = %q, want empty", cfg.RerankHost)
	}
	if cfg.RerankMaxDocs != 50 {
		t.Errorf("RerankMaxDocs default = %d, want 50", cfg.RerankMaxDocs)
	}
	if cfg.RerankBlendWeight != 1.0 {
		t.Errorf("RerankBlendWeight default = %v, want 1.0", cfg.RerankBlendWeight)
	}

	// RerankConfig() must mirror the loaded values 1:1.
	rc := cfg.RerankConfig()
	if rc.Enabled != cfg.RerankEnabled || rc.Host != cfg.RerankHost ||
		rc.APIKey != cfg.RerankAPIKey || rc.Model != cfg.RerankModel ||
		rc.MaxDocs != cfg.RerankMaxDocs || rc.BlendWeight != cfg.RerankBlendWeight {
		t.Errorf("RerankConfig() does not mirror loaded Config: %+v", rc)
	}
}

func TestLoadConfig_InvalidPort(t *testing.T) {
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")
	t.Setenv("CONTEXT_DB_PORT", "not_a_port")
	t.Setenv("OLLAMA_EMBED_DIMS", "")

	_, err := LoadConfig()
	if err == nil {
		t.Error("expected error for invalid CONTEXT_DB_PORT, got nil")
	}
}

func TestLoadConfig_InvalidEmbedDims(t *testing.T) {
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")
	t.Setenv("CONTEXT_DB_PORT", "")
	t.Setenv("CTX_EMBED_DIMS", "abc")

	_, err := LoadConfig()
	if err == nil {
		t.Error("expected error for invalid CTX_EMBED_DIMS, got nil")
	}
}

// parseScopes and parseCooldownHours moved to internal/config (F1-W1); their
// semantics tests live there (load_test.go) and in the golden equivalence
// matrix (golden_test.go).
