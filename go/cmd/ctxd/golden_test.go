package main

// Golden equivalence test (F1-W1): the legacy LoadConfig logic is FROZEN
// below as an independent reference implementation (ref* — byte-for-byte the
// pre-F1 cmd/ctxd/config.go logic, sharing no code with the new loader).
// Every fixture asserts reflect.DeepEqual between the reference parse and the
// new internal/config parse through the legacy bridge, PLUS the explicit
// fatal-semantics column: DeepEqual alone cannot prove fatal paths, because
// in the old fatal case no Config object exists.
//
// The Delta-6 classes (V2, V4, V7-malformed, V9, V12 — plus V1, same class)
// are deliberately NOT under the equivalence proof: they are pinned as
// old-boots/new-ERROR fixtures, making the behavior delta explicit
// (design/01-config-core.md §4 Delta 6, §5).
//
// Fixture hygiene: documentation values only (RFC-2606 hosts, RFC-5737 IPs,
// synthetic keys) — the repo is public.

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
)

// --- frozen reference implementation (pre-F1 LoadConfig, verbatim logic) ---.

func refGetEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func refGetEnvInt(key string, fallback int) (int, error) {
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

func refGetEnvIntSafe(key string, fallback int) int {
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

func refGetEnvFloatSafe(key string, fallback float64) float64 {
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

var refCooldownUnitHours = map[byte]float64{
	'h': 1, 'd': 24, 'w': 24 * 7, 'm': 24 * 30, 'y': 24 * 365,
}

func refParseCooldownHours(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	last := s[len(s)-1]
	if mult, ok := refCooldownUnitHours[last|0x20]; ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(s[:len(s)-1]), 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n * mult, true
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func refGetEnvCooldownHours(key string, fallbackHours float64) float64 {
	if h, ok := refParseCooldownHours(os.Getenv(key)); ok {
		return h
	}
	return fallbackHours
}

func refParseScopes(envVal string, defaultVal []string) []string {
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

func refLoadConfig() (Config, error) {
	port, err := refGetEnvInt("CONTEXT_DB_PORT", 5432)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CONTEXT_DB_PORT: %w", err)
	}
	embedDims, err := refGetEnvInt("CTX_EMBED_DIMS", 4096)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_EMBED_DIMS: %w", err)
	}
	embedNumCtx, err := refGetEnvInt("CTX_EMBED_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_EMBED_NUM_CTX: %w", err)
	}
	chatNumCtx, err := refGetEnvInt("CTX_CHAT_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_CHAT_NUM_CTX: %w", err)
	}
	dreamNumCtx, err := refGetEnvInt("CTX_DREAM_NUM_CTX", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_DREAM_NUM_CTX: %w", err)
	}
	rateLimitWrite, err := refGetEnvInt("CTX_RATE_LIMIT_WRITE", 100)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_RATE_LIMIT_WRITE: %w", err)
	}
	rateLimitRead, err := refGetEnvInt("CTX_RATE_LIMIT_READ", 0)
	if err != nil {
		return Config{}, fmt.Errorf("parsing CTX_RATE_LIMIT_READ: %w", err)
	}

	tz := time.UTC
	if tzName := refGetEnv("CTX_TIMEZONE", ""); tzName != "" {
		loc, err := time.LoadLocation(tzName)
		if err != nil {
			return Config{}, fmt.Errorf("parsing CTX_TIMEZONE %q: %w", tzName, err)
		}
		tz = loc
	}

	cfg := Config{
		ContextDB:     refGetEnv("CONTEXT_DB", "context_store"),
		ContextDBUser: refGetEnv("CONTEXT_DB_USER", "context_user"),
		ContextDBPass: refGetEnv("CONTEXT_DB_PASSWORD", ""),
		ContextDBHost: refGetEnv("CONTEXT_DB_HOST", "localhost"),
		ContextDBPort: port,
		ContextDBSSL:  refGetEnv("CONTEXT_DB_SSLMODE", "disable"),

		EmbedHost:     refGetEnv("CTX_EMBED_HOST", "http://localhost:11434"),
		EmbedAPIKey:   refGetEnv("CTX_EMBED_API_KEY", ""),
		EmbedProtocol: refGetEnv("CTX_EMBED_PROTOCOL", "ollama"),
		EmbedModel:    refGetEnv("CTX_EMBED_MODEL", "qwen3-embedding:8b"),
		EmbedDims:     embedDims,
		EmbedNumCtx:   embedNumCtx,

		ChatHost:     refGetEnv("CTX_CHAT_HOST", "http://localhost:11434"),
		ChatAPIKey:   refGetEnv("CTX_CHAT_API_KEY", ""),
		ChatProtocol: refGetEnv("CTX_CHAT_PROTOCOL", "ollama"),
		ChatModel:    refGetEnv("CTX_CHAT_MODEL", "qwen3.5:9b"),
		ChatNumCtx:   chatNumCtx,
		ChatThink:    refGetEnv("CTX_CHAT_THINK", "false"),

		ChatFallbackHost:     refGetEnv("CTX_CHAT_FALLBACK_HOST", ""),
		ChatFallbackAPIKey:   refGetEnv("CTX_CHAT_FALLBACK_API_KEY", ""),
		ChatFallbackProtocol: refGetEnv("CTX_CHAT_FALLBACK_PROTOCOL", "openai"),
		ChatFallbackTimeoutS: refGetEnvIntSafe("CTX_CHAT_FALLBACK_TIMEOUT", 420),

		RerankEnabled:     refGetEnv("CTX_RERANK_ENABLED", "false") == "true",
		RerankHost:        refGetEnv("CTX_RERANK_HOST", ""),
		RerankAPIKey:      refGetEnv("CTX_RERANK_API_KEY", ""),
		RerankModel:       refGetEnv("CTX_RERANK_MODEL", ""),
		RerankMaxDocs:     refGetEnvIntSafe("CTX_RERANK_MAX_DOCS", 50),
		RerankBlendWeight: refGetEnvFloatSafe("CTX_RERANK_BLEND_WEIGHT", 1.0),

		GraphExpandEnabled:          refGetEnv("CTX_GRAPH_EXPAND_ENABLED", "false") == "true",
		GraphDirected:               refGetEnv("CTX_GRAPH_EXPAND_DIRECTED", "true") == "true",
		GraphHopDepth:               refGetEnvIntSafe("CTX_GRAPH_EXPAND_HOP_DEPTH", 1),
		GraphSeedCount:              refGetEnvIntSafe("CTX_GRAPH_EXPAND_SEED_COUNT", 5),
		GraphSeedScoreFloor:         refGetEnvFloatSafe("CTX_GRAPH_EXPAND_SEED_SCORE_FLOOR", 0.5),
		GraphPerSeedCap:             refGetEnvIntSafe("CTX_GRAPH_EXPAND_PER_SEED_CAP", 3),
		GraphMaxInjected:            refGetEnvIntSafe("CTX_GRAPH_EXPAND_MAX_INJECTED", 10),
		GraphMinConfidence:          refGetEnvFloatSafe("CTX_GRAPH_EXPAND_MIN_CONFIDENCE", 0.75),
		GraphMinConfidenceRecurrent: refGetEnvFloatSafe("CTX_GRAPH_EXPAND_MIN_CONFIDENCE_RECURRENT", 0.8),
		GraphBoostWeight:            refGetEnvFloatSafe("CTX_GRAPH_EXPAND_BOOST_WEIGHT", 0.20),
		GraphHubDamping:             refGetEnv("CTX_GRAPH_EXPAND_HUB_DAMPING", "true") == "true",
		GraphWeightTopical:          refGetEnvFloatSafe("CTX_GRAPH_EXPAND_WEIGHT_TOPICAL", 0.5),
		GraphWeightFactual:          refGetEnvFloatSafe("CTX_GRAPH_EXPAND_WEIGHT_FACTUAL", 0.9),
		GraphWeightCausal:           refGetEnvFloatSafe("CTX_GRAPH_EXPAND_WEIGHT_CAUSAL", 0.9),
		GraphWeightRecurrent:        refGetEnvFloatSafe("CTX_GRAPH_EXPAND_WEIGHT_RECURRENT", 1.0),
		GraphNewPlacementFrac:       refGetEnvFloatSafe("CTX_GRAPH_EXPAND_NEW_PLACEMENT_FRAC", 0.6),

		DreamEnabled:            refGetEnv("CTX_DREAM_ENABLED", "false") == "true",
		DreamHost:               refGetEnv("CTX_DREAM_HOST", "http://localhost:11434"),
		DreamAPIKey:             refGetEnv("CTX_DREAM_API_KEY", ""),
		DreamProtocol:           refGetEnv("CTX_DREAM_PROTOCOL", "ollama"),
		DreamModel:              refGetEnv("CTX_DREAM_MODEL", ""),
		DreamNumCtx:             dreamNumCtx,
		DreamThink:              refGetEnv("CTX_DREAM_THINK", ""),
		DreamEmbedHost:          refGetEnv("CTX_DREAM_EMBED_HOST", ""),
		DreamEmbedAPIKey:        refGetEnv("CTX_DREAM_EMBED_API_KEY", ""),
		DreamEmbedProtocol:      refGetEnv("CTX_DREAM_EMBED_PROTOCOL", ""),
		DreamEmbedModel:         refGetEnv("CTX_DREAM_EMBED_MODEL", ""),
		DreamEmbedNumCtx:        refGetEnvIntSafe("CTX_DREAM_EMBED_NUM_CTX", 0),
		DreamIdleWait:           refGetEnvIntSafe("CTX_DREAM_IDLE_WAIT", 20),
		DreamParallelism:        refGetEnvIntSafe("CTX_DREAM_PARALLELISM", 1),
		DreamBackoffMode:        refGetEnv("CTX_DREAM_BACKOFF_MODE", "exp"),
		DreamBackoffFactor:      refGetEnvFloatSafe("CTX_DREAM_BACKOFF_FACTOR", 1.6),
		DreamBackoffGrace:       refGetEnvIntSafe("CTX_DREAM_BACKOFF_GRACE", 0),
		DreamBackoffCapHours:    refGetEnvCooldownHours("CTX_DREAM_BACKOFF_CAP", 45*24),
		DreamBackoffMinHours:    refGetEnvCooldownHours("CTX_DREAM_BACKOFF_MIN", 12),
		DreamBackoffInertOffset: refGetEnvIntSafe("CTX_DREAM_BACKOFF_INERT_OFFSET", 7),

		Timezone: tz,

		RateLimitWrite: rateLimitWrite,
		RateLimitRead:  rateLimitRead,

		ListenAddr: refGetEnv("LISTEN_ADDR", defaultListenAddr),
	}

	if cfg.ContextDBPass == "" {
		return Config{}, fmt.Errorf("CONTEXT_DB_PASSWORD is required")
	}
	return cfg, nil
}

// --- harness ---.

// resetAllEnv neutralizes every env var either loader reads. The registry is
// the authoritative list; CTX_EMBED_DIMS is bridge-only (Delta 4).
func resetAllEnv(t *testing.T) {
	t.Helper()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CTX_EMBED_DIMS", "")
}

func applyEnv(t *testing.T, env map[string]string) {
	t.Helper()
	resetAllEnv(t)
	if _, ok := env["CONTEXT_DB_PASSWORD"]; !ok {
		env["CONTEXT_DB_PASSWORD"] = "test-password-123"
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func hasErrorOn(issues []config.Issue, field string) bool {
	for _, is := range issues {
		if is.Field == field && is.Severity == config.SeverityError {
			return true
		}
	}
	return false
}

// fullEnvFixture sets every env var to a non-default, Validate-clean value.
func fullEnvFixture() map[string]string {
	return map[string]string{
		"CONTEXT_DB": "ctxtest", "CONTEXT_DB_USER": "ctxuser",
		"CONTEXT_DB_PASSWORD": "test-password-123",
		"CONTEXT_DB_HOST":     "db.example", "CONTEXT_DB_PORT": "5433",
		"CONTEXT_DB_SSLMODE": "require", "LISTEN_ADDR": ":9090",

		"CTX_EMBED_HOST":     "http://embed.example:8081",
		"CTX_EMBED_API_KEY":  "sk-embed-0123456789abcdefghijklmn",
		"CTX_EMBED_PROTOCOL": "openai", "CTX_EMBED_MODEL": "test-embed-8b",
		"CTX_EMBED_NUM_CTX": "2048", "CTX_EMBED_DIMS": "1024",

		"CTX_CHAT_HOST":     "http://192.0.2.10:8089",
		"CTX_CHAT_API_KEY":  "sk-chat-0123456789abcdefghijklmn",
		"CTX_CHAT_PROTOCOL": "openai", "CTX_CHAT_MODEL": "test-chat-27b",
		"CTX_CHAT_NUM_CTX": "98304", "CTX_CHAT_THINK": "false",

		"CTX_CHAT_FALLBACK_HOST":     "http://fallback.example:8090",
		"CTX_CHAT_FALLBACK_API_KEY":  "sk-fallback-0123456789abcdefghij",
		"CTX_CHAT_FALLBACK_PROTOCOL": "openai", "CTX_CHAT_FALLBACK_TIMEOUT": "300",

		"CTX_RERANK_ENABLED": "true", "CTX_RERANK_HOST": "http://rerank.example:8082",
		"CTX_RERANK_API_KEY":  "sk-rerank-0123456789abcdefghijkl",
		"CTX_RERANK_MODEL":    "test-reranker-m3",
		"CTX_RERANK_MAX_DOCS": "40", "CTX_RERANK_BLEND_WEIGHT": "0.5",

		"CTX_GRAPH_EXPAND_ENABLED": "true", "CTX_GRAPH_EXPAND_DIRECTED": "false",
		"CTX_GRAPH_EXPAND_HOP_DEPTH": "2", "CTX_GRAPH_EXPAND_SEED_COUNT": "7",
		"CTX_GRAPH_EXPAND_SEED_SCORE_FLOOR": "0.4", "CTX_GRAPH_EXPAND_PER_SEED_CAP": "4",
		"CTX_GRAPH_EXPAND_MAX_INJECTED": "12", "CTX_GRAPH_EXPAND_MIN_CONFIDENCE": "0.7",
		"CTX_GRAPH_EXPAND_MIN_CONFIDENCE_RECURRENT": "0.85",
		"CTX_GRAPH_EXPAND_BOOST_WEIGHT":             "0.25",
		"CTX_GRAPH_EXPAND_HUB_DAMPING":              "false",
		"CTX_GRAPH_EXPAND_WEIGHT_TOPICAL":           "0.6",
		"CTX_GRAPH_EXPAND_WEIGHT_FACTUAL":           "0.8",
		"CTX_GRAPH_EXPAND_WEIGHT_CAUSAL":            "0.85",
		"CTX_GRAPH_EXPAND_WEIGHT_RECURRENT":         "0.95",
		"CTX_GRAPH_EXPAND_NEW_PLACEMENT_FRAC":       "0.45",

		"CTX_DREAM_ENABLED": "true", "CTX_DREAM_HOST": "http://192.0.2.10:8089",
		"CTX_DREAM_API_KEY":  "sk-dream-0123456789abcdefghijklm",
		"CTX_DREAM_PROTOCOL": "openai", "CTX_DREAM_MODEL": "test-dream-27b",
		"CTX_DREAM_NUM_CTX": "98304", "CTX_DREAM_THINK": "true",

		"CTX_DREAM_EMBED_HOST":     "http://embed.example:8081",
		"CTX_DREAM_EMBED_API_KEY":  "sk-dream-embed-0123456789abcdefg",
		"CTX_DREAM_EMBED_PROTOCOL": "openai", "CTX_DREAM_EMBED_MODEL": "test-embed-8b",
		"CTX_DREAM_EMBED_NUM_CTX": "2048",

		"CTX_DREAM_IDLE_WAIT": "30", "CTX_DREAM_PARALLELISM": "4",
		"CTX_DREAM_BACKOFF_MODE": "log", "CTX_DREAM_BACKOFF_FACTOR": "2.5",
		"CTX_DREAM_BACKOFF_GRACE": "3", "CTX_DREAM_BACKOFF_CAP": "30d",
		"CTX_DREAM_BACKOFF_MIN": "6h", "CTX_DREAM_BACKOFF_INERT_OFFSET": "5",

		"CTX_TIMEZONE":         "Europe/Berlin",
		"CTX_RATE_LIMIT_WRITE": "200", "CTX_RATE_LIMIT_READ": "50",
		"CTX_READ_SCOPES": "private,shared,work,crag",

		"CTX_SCORE_THRESHOLD": "0.002", "CTX_CONFIDENT_THRESHOLD": "0.01",
		"CTX_PROMPT_VERSION": "v6",
	}
}

// TestGoldenEquivalence: for every fixture the reference parse and the new
// loader (through the legacy bridge) produce a DeepEqual Config, and the new
// loader reports no SeverityError.
func TestGoldenEquivalence(t *testing.T) {
	fixtures := map[string]map[string]string{
		"empty (defaults only)": {},
		"full":                  fullEnvFixture(),

		// Duration suffix forms h/d/w/m/y + bare + uppercase (cap/min pair).
		"suffix 12h/45d":  {"CTX_DREAM_BACKOFF_CAP": "45d", "CTX_DREAM_BACKOFF_MIN": "12h"},
		"suffix 1w/1m":    {"CTX_DREAM_BACKOFF_CAP": "1m", "CTX_DREAM_BACKOFF_MIN": "1w"},
		"suffix 1y/bare":  {"CTX_DREAM_BACKOFF_CAP": "1y", "CTX_DREAM_BACKOFF_MIN": "36"},
		"suffix 2D upper": {"CTX_DREAM_BACKOFF_CAP": "2D", "CTX_DREAM_BACKOFF_MIN": "1.5d"},

		// Bare ints / seconds forms.
		"bare seconds": {"CTX_CHAT_FALLBACK_TIMEOUT": "90", "CTX_DREAM_IDLE_WAIT": "45"},

		// Malformed values on safe fields: old = silent default, new = same
		// value + WARN (Delta 3 — visibility only, value-identical).
		"safe malformed": {
			"CTX_RERANK_MAX_DOCS": "abc", "CTX_RERANK_BLEND_WEIGHT": "xyz",
			"CTX_DREAM_BACKOFF_FACTOR": "junk", "CTX_DREAM_BACKOFF_GRACE": "g",
			"CTX_DREAM_IDLE_WAIT": "zz", "CTX_DREAM_PARALLELISM": "pp",
			"CTX_CHAT_FALLBACK_TIMEOUT": "oops", "CTX_DREAM_EMBED_NUM_CTX": "qq",
			"CTX_DREAM_BACKOFF_CAP": "garbage", "CTX_DREAM_BACKOFF_MIN": "-5d",
		},

		// Bool semantics: only the exact literal "true" activates.
		"bool exact match": {
			"CTX_RERANK_ENABLED": "1", "CTX_DREAM_ENABLED": "TRUE",
			"CTX_GRAPH_EXPAND_ENABLED": "yes", "CTX_GRAPH_EXPAND_DIRECTED": "true",
		},
	}

	for name, env := range fixtures {
		t.Run(name, func(t *testing.T) {
			applyEnv(t, env)

			refCfg, refErr := refLoadConfig()
			if refErr != nil {
				t.Fatalf("reference must boot for equivalence fixtures, got: %v", refErr)
			}
			newCfg, cc, issues, err := loadConfig()
			if err != nil {
				t.Fatalf("new loader bridge error: %v", err)
			}
			if config.HasErrors(issues) {
				t.Fatalf("new loader must not error on equivalence fixtures, got: %v", issues)
			}
			if !reflect.DeepEqual(refCfg, newCfg) {
				t.Errorf("config mismatch:\nref: %+v\nnew: %+v", refCfg, newCfg)
			}

			// CTX_READ_SCOPES lived outside LoadConfig (raw read in main):
			// compare against the reference parseScopes semantics.
			refScopes := refParseScopes(os.Getenv("CTX_READ_SCOPES"), []string{"private", "shared", "work"})
			if !reflect.DeepEqual(refScopes, cc.Scheduler.ReadScopes) {
				t.Errorf("read scopes mismatch: ref %v, new %v", refScopes, cc.Scheduler.ReadScopes)
			}
		})
	}
}

// TestGoldenSafeFieldsWarn pins Delta 3: the safe-malformed fixture is
// value-identical (proven above) but now VISIBLE — one WARN per field.
func TestGoldenSafeFieldsWarn(t *testing.T) {
	applyEnv(t, map[string]string{"CTX_RERANK_MAX_DOCS": "abc"})
	_, _, issues, err := loadConfig()
	if err != nil || config.HasErrors(issues) {
		t.Fatalf("safe-malformed must not be fatal: err=%v issues=%v", err, issues)
	}
	for _, is := range issues {
		if is.Field == "rerank.max_docs" && is.Severity == config.SeverityWarn {
			return
		}
	}
	t.Errorf("expected WARN on rerank.max_docs, got %v", issues)
}

// TestGoldenFatalSemantics is the fatal-semantics column: per strict field
// one malformed fixture — the reference errors, the new loader reports a
// SeverityError on the same field (or the bridge error for CTX_EMBED_DIMS,
// whose strict parse stays in cmd/ctxd until wave 7, Delta 4).
func TestGoldenFatalSemantics(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		field string // "" = expect bridge err instead of issue
	}{
		{"db port", map[string]string{"CONTEXT_DB_PORT": "not_a_port"}, "server.db_port"},
		{"embed num_ctx", map[string]string{"CTX_EMBED_NUM_CTX": "abc"}, "embed.num_ctx"},
		{"chat num_ctx", map[string]string{"CTX_CHAT_NUM_CTX": "abc"}, "chat.num_ctx"},
		{"dream num_ctx", map[string]string{"CTX_DREAM_NUM_CTX": "abc"}, "dream.num_ctx"},
		{"rate limit write", map[string]string{"CTX_RATE_LIMIT_WRITE": "abc"}, "query.rate_limit_write"},
		{"rate limit read", map[string]string{"CTX_RATE_LIMIT_READ": "abc"}, "query.rate_limit_read"},
		{"timezone", map[string]string{"CTX_TIMEZONE": "Nope/Nowhere"}, "query.timezone"},
		{"whitespace int", map[string]string{"CONTEXT_DB_PORT": " 5432 "}, "server.db_port"},
		{"required password", map[string]string{"CONTEXT_DB_PASSWORD": ""}, "server.db_password"},
		{"embed dims (bridge)", map[string]string{"CTX_EMBED_DIMS": "abc"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyEnv(t, c.env)

			if _, refErr := refLoadConfig(); refErr == nil {
				t.Fatal("reference must refuse to boot")
			}
			_, _, issues, err := loadConfig()
			if c.field == "" {
				if err == nil {
					t.Fatal("bridge must keep the strict CTX_EMBED_DIMS parse fatal (Delta 4)")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected bridge error: %v", err)
			}
			if !hasErrorOn(issues, c.field) {
				t.Errorf("want SeverityError on %s, got %v", c.field, issues)
			}

			// The public LoadConfig contract carries the fatality unchanged.
			if _, loadErr := LoadConfig(); loadErr == nil {
				t.Error("LoadConfig must surface the SeverityError as error")
			}
		})
	}
}

// TestGoldenDelta6Pinned pins the deliberate behavior delta (§4 Delta 6 + V1,
// same class): configs that BOOTED before F1 — and were already broken at
// runtime — now abort with a SeverityError naming the field. These fixtures
// run OUTSIDE the equivalence proof on purpose: red-proof is structural (the
// reference implementation boots, the new loader errors).
func TestGoldenDelta6Pinned(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		field string
	}{
		{"V2 inverted thresholds", map[string]string{
			"CTX_SCORE_THRESHOLD": "0.5", "CTX_CONFIDENT_THRESHOLD": "0.008",
		}, "query.score_threshold"},
		{"V4 protocol typo", map[string]string{"CTX_CHAT_PROTOCOL": "olama"}, "chat.protocol"},
		{"V7 unparseable host", map[string]string{"CTX_CHAT_HOST": "http://bad host"}, "chat.host"},
		{"V7 non-http scheme", map[string]string{"CTX_CHAT_HOST": "ftp://chat.example"}, "chat.host"},
		{"V7 trailing slash", map[string]string{"CTX_CHAT_HOST": "http://chat.example/"}, "chat.host"},
		{"V7 userinfo", map[string]string{
			"CTX_CHAT_HOST": "http://admin:delta-secret-marker@chat.example",
		}, "chat.host"},
		{"V9 blend weight range", map[string]string{"CTX_RERANK_BLEND_WEIGHT": "1.5"}, "rerank.blend_weight"},
		{"V9 hop depth zero", map[string]string{"CTX_GRAPH_EXPAND_HOP_DEPTH": "0"}, "graph.hop_depth"},
		{"V9 negative rate limit", map[string]string{"CTX_RATE_LIMIT_WRITE": "-1"}, "query.rate_limit_write"},
		{"V12 cross-host credential", map[string]string{
			"CTX_EMBED_API_KEY":    "sk-embed-0123456789abcdefghijklmn",
			"CTX_DREAM_EMBED_HOST": "http://other-embed.example:8081",
		}, "dream_embed.api_key"},
		{"V1 dual-runner num_ctx (ollama)", map[string]string{
			"CTX_CHAT_NUM_CTX": "98304", "CTX_DREAM_NUM_CTX": "32768",
		}, "dream.num_ctx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyEnv(t, c.env)

			// Old world: this configuration boots (broken at runtime instead).
			if _, refErr := refLoadConfig(); refErr != nil {
				t.Fatalf("delta-6 fixture must BOOT on the reference, got: %v", refErr)
			}
			// New world: explicit boot abort on the named field.
			_, _, issues, err := loadConfig()
			if err != nil {
				t.Fatalf("unexpected bridge error: %v", err)
			}
			if !hasErrorOn(issues, c.field) {
				t.Errorf("want SeverityError on %s, got %v", c.field, issues)
			}

			// Leak hygiene on the userinfo case: the password never reaches
			// an issue message ((*url.Error).Error() embeds the raw URL).
			for _, is := range issues {
				if strings.Contains(is.Msg, "delta-secret-marker") {
					t.Errorf("issue message leaks userinfo credential: %s", is.Msg)
				}
			}
		})
	}
}
