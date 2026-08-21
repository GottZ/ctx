package config

// Tests for the F2-W4 override build path (build.go): precedence matrix
// (default / env / env+db), admission gates, validate-with-individual-drop,
// secret_ref resolution and boot tolerance (W17: a DB row never gains
// boot-abort power, strict-parse env semantics stay env-only).
//
// Fixture hygiene (wave-1 mandate): documentation values only — RFC-2606
// hostnames (*.example), runtime-concatenated markers. Never secret-shaped
// literals, not even synthetic ones (scanners match FORM, not authenticity).

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// resetBuildEnv neutralizes every registry env var and injects the required
// DB password (V11 would otherwise error on every Build pass).
func resetBuildEnv(t *testing.T) {
	t.Helper()
	for _, v := range EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
}

func warnFor(t *testing.T, issues []Issue, field, substr string) {
	t.Helper()
	for _, is := range issues {
		if is.Field == field && is.Severity == SeverityWarn && strings.Contains(is.Msg, substr) {
			return
		}
	}
	t.Errorf("no WARN on %q containing %q in %v", field, substr, issues)
}

// --- Gate (a): no overrides ⇒ identical to the env build ---.

func TestBuildNoOverridesIdenticalToEnvBuild(t *testing.T) {
	resetBuildEnv(t)
	t.Setenv("CTX_CHAT_HOST", "http://chat.example:8089")
	t.Setenv("CTX_CHAT_NUM_CTX", "4096")
	t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.5")
	t.Setenv("CTX_READ_SCOPES", "private,shared")
	t.Setenv("CTX_TIMEZONE", "Europe/Berlin")

	envCfg, envIssues := FromEnv()
	envIssues = append(envIssues, Validate(envCfg)...)

	built, builtIssues := Build(nil, nil)

	if !reflect.DeepEqual(envCfg, built) {
		t.Errorf("Build(nil, nil) differs from env build:\nenv:   %+v\nbuilt: %+v", envCfg, built)
	}
	if !reflect.DeepEqual(envIssues, builtIssues) {
		t.Errorf("issues differ: env=%v built=%v", envIssues, builtIssues)
	}
}

// --- Override source attribution (06-C2): tenant vs settings label ---.

// TestBuildOverrideSourceLabel pins the 06-C2 source attribution carried by
// Override.Source: a zero-value Source defaults to "settings" (the boot/reload
// _global path stays byte-identical to pre-C2), while the per-tenant overlay
// sets SourceTenant so Source(key) and the boot dump attribute a tenant
// override. RED before Override gains the Source field / buildCandidate honors it.
func TestBuildOverrideSourceLabel(t *testing.T) {
	resetBuildEnv(t)
	const key = "rerank.blend_weight"

	def, _ := Build(nil, nil)
	if def.Source(key) == SourceSettings || def.Source(key) == SourceTenant {
		t.Fatalf("fixture: %s must not be override-sourced without a row, got %q", key, def.Source(key))
	}

	settings, _ := Build([]Override{{Key: key, Value: "0.4"}}, nil)
	if settings.Rerank.BlendWeight != 0.4 || settings.Source(key) != SourceSettings {
		t.Errorf("zero-value Source must default to %q: got %v source %q",
			SourceSettings, settings.Rerank.BlendWeight, settings.Source(key))
	}

	tenant, _ := Build([]Override{{Key: key, Value: "0.6", Source: SourceTenant}}, nil)
	if tenant.Rerank.BlendWeight != 0.6 || tenant.Source(key) != SourceTenant {
		t.Errorf("explicit Source must be honored: got %v source %q, want 0.6/%q",
			tenant.Rerank.BlendWeight, tenant.Source(key), SourceTenant)
	}
}

// --- Gate (b): precedence matrix default / env / env+db per key type ---.

func TestBuildPrecedenceMatrix(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		envVar string
		envVal string
		dbVal  string

		wantDefault any
		wantEnv     any
		wantDB      any
		get         func(c *Config) any
	}{
		{
			name: "string", key: "chat.model", envVar: "CTX_CHAT_MODEL",
			envVal: "env-model", dbVal: "db-model",
			wantDefault: "qwen3.5:9b", wantEnv: "env-model", wantDB: "db-model",
			get: func(c *Config) any { return c.Chat.Model },
		},
		{
			name: "int", key: "chat.num_ctx", envVar: "CTX_CHAT_NUM_CTX",
			envVal: "1111", dbVal: "2222",
			wantDefault: 0, wantEnv: 1111, wantDB: 2222,
			get: func(c *Config) any { return c.Chat.NumCtx },
		},
		{
			name: "float", key: "rerank.blend_weight", envVar: "CTX_RERANK_BLEND_WEIGHT",
			envVal: "0.25", dbVal: "0.75",
			wantDefault: 1.0, wantEnv: 0.25, wantDB: 0.75,
			get: func(c *Config) any { return c.Rerank.BlendWeight },
		},
		{
			name: "bool", key: "graph.directed", envVar: "CTX_GRAPH_EXPAND_DIRECTED",
			envVal: "false", dbVal: "true",
			wantDefault: true, wantEnv: false, wantDB: true,
			get: func(c *Config) any { return c.Graph.Directed },
		},
		{
			name: "duration", key: "dream.idle_wait", envVar: "CTX_DREAM_IDLE_WAIT",
			envVal: "30", dbVal: "45",
			wantDefault: 20 * time.Second, wantEnv: 30 * time.Second, wantDB: 45 * time.Second,
			get: func(c *Config) any { return c.Dream.IdleWait },
		},
		{
			name: "hours", key: "dream.backoff_cap", envVar: "CTX_DREAM_BACKOFF_CAP",
			envVal: "12h", dbVal: "1w",
			wantDefault: Hours(45 * 24), wantEnv: Hours(12), wantDB: Hours(7 * 24),
			get: func(c *Config) any { return c.Dream.Backoff.CapHours },
		},
		{
			name: "scopes", key: "scheduler.read_scopes", envVar: "CTX_READ_SCOPES",
			envVal: "alpha,beta", dbVal: "gamma,delta",
			wantDefault: []string{"private", "shared", "work"},
			wantEnv:     []string{"alpha", "beta"},
			wantDB:      []string{"gamma", "delta"},
			get:         func(c *Config) any { return c.Scheduler.ReadScopes },
		},
		{
			name: "protocol", key: "chat.protocol", envVar: "CTX_CHAT_PROTOCOL",
			envVal: "openai", dbVal: "ollama",
			wantDefault: "ollama", wantEnv: "openai", wantDB: "ollama",
			get: func(c *Config) any { return string(c.Chat.Protocol) },
		},
		{
			name: "timezone-strict-tagged", key: "query.timezone", envVar: "CTX_TIMEZONE",
			envVal: "Europe/Berlin", dbVal: "America/New_York",
			wantDefault: "UTC", wantEnv: "Europe/Berlin", wantDB: "America/New_York",
			get: func(c *Config) any { return c.Query.Timezone.String() },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := func(stage string, c *Config, issues []Issue, want any, wantSource string) {
				t.Helper()
				if HasErrors(issues) {
					t.Fatalf("%s: unexpected errors: %v", stage, issues)
				}
				if got := tc.get(c); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: value = %v, want %v", stage, got, want)
				}
				if got := c.Source(tc.key); got != wantSource {
					t.Errorf("%s: source = %q, want %q", stage, got, wantSource)
				}
			}

			// default-only.
			resetBuildEnv(t)
			c, issues := Build(nil, nil)
			check("default", c, issues, tc.wantDefault, "default")

			// env.
			t.Setenv(tc.envVar, tc.envVal)
			c, issues = Build(nil, nil)
			check("env", c, issues, tc.wantEnv, "env")

			// env + db ⇒ DB wins.
			c, issues = Build([]Override{{Key: tc.key, Value: tc.dbVal}}, nil)
			check("env+db", c, issues, tc.wantDB, "settings")
		})
	}
}

// scheduler.home_scope has NO env var (env:"-") — the DB override must reach
// it without a special path (the F1 key-keyed loader contract).
func TestBuildOverrideOnEnvLessKey(t *testing.T) {
	resetBuildEnv(t)
	c, issues := Build([]Override{{Key: "scheduler.home_scope", Value: "tenant-a"}}, nil)
	if HasErrors(issues) {
		t.Fatalf("unexpected errors: %v", issues)
	}
	if c.Scheduler.HomeScope != "tenant-a" {
		t.Errorf("HomeScope = %q, want tenant-a", c.Scheduler.HomeScope)
	}
	if c.Source("scheduler.home_scope") != "settings" {
		t.Errorf("source = %q, want settings", c.Source("scheduler.home_scope"))
	}
}

// --- Admission gates: every unusable override degrades to a WARN ---.

func TestBuildAdmissionRejections(t *testing.T) {
	resetBuildEnv(t)
	t.Setenv("CTX_CHAT_NUM_CTX", "4096")

	c, issues := Build([]Override{
		{Key: "nope.nope", Value: "1"},                     // unknown key (Risk 7: code downgrade)
		{Key: "dream.parallelism", Value: "4"},             // mut:"restart"
		{Key: "server.db_host", Value: "db.example"},       // mut:"restart" (DSN group, circular)
		{Key: "rerank.blend_weight", Value: "kaputt"},      // unparseable float
		{Key: "chat.num_ctx", Value: "not-a-number"},       // parse:"strict" field — DB stays WARN (W17)
		{Key: "query.timezone", Value: "Atlantis/Nowhere"}, // strict timezone — DB stays WARN
		{Key: "scheduler.read_scopes", Value: " , "},       // no usable scope
	}, nil)

	if HasErrors(issues) {
		t.Fatalf("override layer must never produce errors, got: %v", issues)
	}
	warnFor(t, issues, "nope.nope", "unknown settings key")
	warnFor(t, issues, "dream.parallelism", `mutability "restart"`)
	// The mut:"coupled" case rode on embed.model, the tag's only carrier, until
	// β7 cut the embed tuple. The class stays valid registry vocabulary
	// (validMut) with nothing in it, and admitOverride treats every non-hot,
	// non-coupled:embed-cache value identically — the two restart cases here
	// exercise the same branch and quote the same message with a different %q.
	// The COUPLED-SPECIFIC operator text (the re-embed hint) is a handler
	// concern and stays pinned on a synthetic KeyInfo in
	// handler/settings_test.go, which needs no registry carrier.
	warnFor(t, issues, "server.db_host", `mutability "restart"`)
	warnFor(t, issues, "rerank.blend_weight", "invalid number")
	warnFor(t, issues, "chat.num_ctx", "invalid integer")
	warnFor(t, issues, "query.timezone", "unknown timezone")
	warnFor(t, issues, "scheduler.read_scopes", "no non-empty scope")

	// Env/default values stay active for every rejected override.
	if c.Dream.Parallelism != 1 || c.Rerank.BlendWeight != 1.0 || c.Chat.NumCtx != 4096 {
		t.Errorf("rejected overrides leaked into the config: %+v", c)
	}
	if c.Query.Timezone != time.UTC {
		t.Errorf("Timezone = %v, want UTC", c.Query.Timezone)
	}
	if c.Source("chat.num_ctx") != "env" {
		t.Errorf("chat.num_ctx source = %q, want env", c.Source("chat.num_ctx"))
	}
}

// --- Validate-with-individual-drop: only the offending override falls ---.

func TestBuildValidationDropsOffenderKeepsRest(t *testing.T) {
	resetBuildEnv(t)

	c, issues := Build([]Override{
		{Key: "rerank.blend_weight", Value: "1.5"}, // parses, V9 SeverityError
		{Key: "chat.model", Value: "db-model"},     // healthy
	}, nil)

	if HasErrors(issues) {
		t.Fatalf("override layer must never produce errors, got: %v", issues)
	}
	warnFor(t, issues, "rerank.blend_weight", "settings override dropped")
	if c.Rerank.BlendWeight != 1.0 {
		t.Errorf("BlendWeight = %v, want default 1.0 after drop", c.Rerank.BlendWeight)
	}
	if c.Source("rerank.blend_weight") != "default" {
		t.Errorf("source = %q, want default after drop", c.Source("rerank.blend_weight"))
	}
	if c.Chat.Model != "db-model" || c.Source("chat.model") != "settings" {
		t.Errorf("healthy override must survive the drop pass: model=%q source=%q",
			c.Chat.Model, c.Source("chat.model"))
	}
}

// A cross-field error NOT attributable to any override withdraws ALL
// overrides — degraded, loud, never fatal.
//
// The vehicle rode on V1 until β6 cut the dream chat tuple: raising
// chat.num_ctx away from dream.num_ctx on a shared host produced an ERROR
// addressed to dream.num_ctx, a field no override had touched. V2 has the same
// shape and outlives the cut train — the inverted-threshold error is ALWAYS
// addressed to query.score_threshold, whichever of the pair moved. Overriding
// confident_threshold downwards therefore produces an error dropOffenders
// cannot attribute, which is exactly the branch under test.
func TestBuildCrossFieldWithdrawsAllOverrides(t *testing.T) {
	resetBuildEnv(t)
	t.Setenv("CTX_SCORE_THRESHOLD", "0.001")
	t.Setenv("CTX_CONFIDENT_THRESHOLD", "0.02")

	// Lowering confident_threshold below the env score_threshold ⇒ V2
	// SeverityError on query.score_threshold, which is not the overridden field.
	c, issues := Build([]Override{{Key: "query.confident_threshold", Value: "0.0005"}}, nil)

	if HasErrors(issues) {
		t.Fatalf("override layer must never produce errors, got: %v", issues)
	}
	warnFor(t, issues, "settings", "withdrawing all")
	if c.Query.ConfidentThreshold != 0.02 {
		t.Errorf("ConfidentThreshold = %v, want env 0.02 after withdrawal", c.Query.ConfidentThreshold)
	}
	if c.Source("query.confident_threshold") != "env" {
		t.Errorf("source = %q, want env", c.Source("query.confident_threshold"))
	}
}

// --- secret_ref resolution ---.

func TestBuildSecretRefResolution(t *testing.T) {
	resetBuildEnv(t)
	// Marker assembled at runtime — never a key-shaped literal (≥ fpMinLen so
	// the dump path exercises the fingerprint branch, not presence-only).
	plaintext := "RESOLVED-" + strings.Repeat("p", 24)

	t.Run("resolves to plaintext in-memory", func(t *testing.T) {
		called := ""
		c, issues := Build([]Override{{Key: "chat.api_key", Value: "prov-main"}},
			func(name string) (string, error) {
				called = name
				return plaintext, nil
			})
		if HasErrors(issues) {
			t.Fatalf("unexpected errors: %v", issues)
		}
		if called != "prov-main" {
			t.Errorf("resolver called with %q, want prov-main", called)
		}
		if c.Chat.APIKey != plaintext {
			t.Errorf("APIKey not resolved (got %q)", c.Chat.APIKey)
		}
		if c.Source("chat.api_key") != "settings" {
			t.Errorf("source = %q, want settings", c.Source("chat.api_key"))
		}
		// The resolved value must never surface in any issue message.
		for _, is := range issues {
			if strings.Contains(is.Msg, plaintext) {
				t.Errorf("issue message leaks resolved plaintext: %q", is.Msg)
			}
		}
	})

	t.Run("resolver failure keeps env value", func(t *testing.T) {
		t.Setenv("CTX_CHAT_API_KEY", "env-key-"+strings.Repeat("e", 24))
		c, issues := Build([]Override{{Key: "chat.api_key", Value: "prov-main"}},
			func(string) (string, error) { return "", fmt.Errorf("no such secret") })
		warnFor(t, issues, "chat.api_key", "secret_ref resolution failed")
		if !strings.HasPrefix(c.Chat.APIKey, "env-key-") {
			t.Errorf("env value must stay active, got %q", c.Chat.APIKey)
		}
		if c.Source("chat.api_key") != "env" {
			t.Errorf("source = %q, want env", c.Source("chat.api_key"))
		}
	})

	t.Run("nil resolver ignores the override", func(t *testing.T) {
		c, issues := Build([]Override{{Key: "chat.api_key", Value: "prov-main"}}, nil)
		warnFor(t, issues, "chat.api_key", "no secret resolver available")
		if c.Chat.APIKey != "" {
			t.Errorf("APIKey = %q, want empty default", c.Chat.APIKey)
		}
	})
}
