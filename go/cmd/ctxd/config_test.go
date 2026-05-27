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

// --- parseScopes (v2.0.0 C1: CTX_READ_SCOPES) ---.

func TestParseScopes_EmptyReturnsDefault(t *testing.T) {
	def := []string{"private", "shared", "work"}
	got := parseScopes("", def)
	if !equalStringSlices(got, def) {
		t.Errorf("parseScopes(%q, %v) = %v, want %v", "", def, got, def)
	}
}

func TestParseScopes_SingleValue(t *testing.T) {
	got := parseScopes("crag", []string{"private"})
	want := []string{"crag"}
	if !equalStringSlices(got, want) {
		t.Errorf("parseScopes(%q) = %v, want %v", "crag", got, want)
	}
}

func TestParseScopes_MultiComma(t *testing.T) {
	got := parseScopes("private,shared,work,crag", []string{"private"})
	want := []string{"private", "shared", "work", "crag"}
	if !equalStringSlices(got, want) {
		t.Errorf("parseScopes multi = %v, want %v", got, want)
	}
}

func TestParseScopes_WhitespaceTrim(t *testing.T) {
	// Whitespace around values should be trimmed.
	got := parseScopes("  private , shared  , work  ", []string{"private"})
	want := []string{"private", "shared", "work"}
	if !equalStringSlices(got, want) {
		t.Errorf("parseScopes whitespace = %v, want %v", got, want)
	}
}

func TestParseScopes_AllEmptyReturnsDefault(t *testing.T) {
	// Only commas + whitespace → no non-empty parts → fallback to default.
	def := []string{"private", "shared", "work"}
	got := parseScopes(" , , , ", def)
	if !equalStringSlices(got, def) {
		t.Errorf("parseScopes all-empty = %v, want default %v", got, def)
	}
}

func TestParseScopes_EmptyPartsFiltered(t *testing.T) {
	// "a,,b" → ["a", "b"] — empty parts are dropped.
	got := parseScopes("a,,b,,,c", []string{"private"})
	want := []string{"a", "b", "c"}
	if !equalStringSlices(got, want) {
		t.Errorf("parseScopes empty-parts = %v, want %v", got, want)
	}
}

func TestParseScopes_DefaultPreservedOnEmpty(t *testing.T) {
	// Default slice should be returned by-reference (or value-equal); confirm
	// callers get the exact backwards-compat default.
	def := []string{"private", "shared", "work"}
	got := parseScopes("", def)
	if len(got) != 3 || got[0] != "private" || got[1] != "shared" || got[2] != "work" {
		t.Errorf("parseScopes empty fallback = %v, want exact v1.x default", got)
	}
}

func TestParseScopes_CustomScopeNames(t *testing.T) {
	// v2.0.0 M045 dropped chk_scope: any string is a valid scope here.
	got := parseScopes("hth,crag,project-x,team_alpha", []string{"private"})
	want := []string{"hth", "crag", "project-x", "team_alpha"}
	if !equalStringSlices(got, want) {
		t.Errorf("parseScopes custom-names = %v, want %v", got, want)
	}
}

// equalStringSlices returns true if a and b have the same length and elements
// in the same order.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
