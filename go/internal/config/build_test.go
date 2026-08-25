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
	t.Setenv("CTX_DIGEST_MODE", "env-mode")
	t.Setenv("CTX_RERANK_MAX_DOCS", "4096")
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
		// The string and int rows rode on chat.model/chat.num_ctx until β8 cut
		// the tuple. Both are pure type carriers — the matrix asserts the
		// precedence ladder once per Go TYPE, not once per key — so they simply
		// moved to live keys of the same types.
		{
			name: "string", key: "digest.mode", envVar: "CTX_DIGEST_MODE",
			envVal: "env-mode", dbVal: "db-mode",
			wantDefault: "full", wantEnv: "env-mode", wantDB: "db-mode",
			get: func(c *Config) any { return c.Digest.Mode },
		},
		{
			name: "int", key: "rerank.max_docs", envVar: "CTX_RERANK_MAX_DOCS",
			envVal: "1111", dbVal: "2222",
			wantDefault: 50, wantEnv: 1111, wantDB: 2222,
			get: func(c *Config) any { return c.Rerank.MaxDocs },
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
		// The protocol row died with chat.protocol in β8 and has no successor
		// here: it was the LAST typProtocol field in the registry, as chat.think
		// was the last typThink one. Unlike the string and int rows above this is
		// not a vehicle swap — there is no live key of either type to swap to.
		// The two parsers stay (they are generic registry vocabulary, and a
		// backends.Protocol is still what a pool row carries), so their coverage
		// moved down one layer to TestParserForUnoccupiedTypes, which drives
		// parserFor directly. Restoring a row here needs a real registry carrier
		// first.
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
	t.Setenv("CTX_GRAPH_OVERVIEW_LABEL_BATCH", "4096")

	c, issues := Build([]Override{
		{Key: "nope.nope", Value: "1"},                // unknown key (Risk 7: code downgrade)
		{Key: "dream.parallelism", Value: "4"},        // mut:"restart"
		{Key: "server.db_host", Value: "db.example"},  // mut:"restart" (DSN group, circular)
		{Key: "rerank.blend_weight", Value: "kaputt"}, // unparseable float
		// The strict-int row rode on chat.num_ctx until β8 cut the tuple; the
		// statement is about the parse:"strict" CLASS, not about that key, and
		// the class still has 33 registry carriers.
		{Key: "graph_overview.label_batch", Value: "not-a-number"}, // parse:"strict" field — DB stays WARN (W17)
		{Key: "query.timezone", Value: "Atlantis/Nowhere"},         // strict timezone — DB stays WARN
		{Key: "scheduler.read_scopes", Value: " , "},               // no usable scope
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
	warnFor(t, issues, "graph_overview.label_batch", "invalid integer")
	warnFor(t, issues, "query.timezone", "unknown timezone")
	warnFor(t, issues, "scheduler.read_scopes", "no non-empty scope")

	// Env/default values stay active for every rejected override.
	if c.Dream.Parallelism != 1 || c.Rerank.BlendWeight != 1.0 || c.GraphOverview.LabelBatch != 4096 {
		t.Errorf("rejected overrides leaked into the config: %+v", c)
	}
	if c.Query.Timezone != time.UTC {
		t.Errorf("Timezone = %v, want UTC", c.Query.Timezone)
	}
	if c.Source("graph_overview.label_batch") != "env" {
		t.Errorf("graph_overview.label_batch source = %q, want env", c.Source("graph_overview.label_batch"))
	}
}

// --- Validate-with-individual-drop: only the offending override falls ---.

func TestBuildValidationDropsOffenderKeepsRest(t *testing.T) {
	resetBuildEnv(t)

	c, issues := Build([]Override{
		{Key: "rerank.blend_weight", Value: "1.5"}, // parses, V9 SeverityError
		{Key: "digest.mode", Value: "db-mode"},     // healthy
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
	if c.Digest.Mode != "db-mode" || c.Source("digest.mode") != "settings" {
		t.Errorf("healthy override must survive the drop pass: mode=%q source=%q",
			c.Digest.Mode, c.Source("digest.mode"))
	}
}

// TestBuildOutOfRangeDurationOverridesDropped is the issue-#29 class pin at
// the surface an operator actually touches: a settings row. Both halves of
// "out of range" are checked, because they take DIFFERENT routes to the same
// outcome — the negative one parses cleanly and is dropped by Validate (V17,
// SeverityError → dropOffenders), the overflowing one never becomes a value
// at all (parseDurationSeconds rejects it, admitOverride WARNs). Before the
// wave both were applied: `-30` rendered as `-30s` and `9223372036855` as
// `224.192ms`, each with source "settings".
//
// The WARN is what the settings PUT turns into a 422 (handler/settings.go
// checks the resulting source, then reads the issue for that key), so the
// per-key attribution asserted here is the API contract, not decoration.
func TestBuildOutOfRangeDurationOverridesDropped(t *testing.T) {
	resetBuildEnv(t)

	c, issues := Build([]Override{
		{Key: "graph_cache.rebuild_interval", Value: "-30"},     // parses, V17 SeverityError
		{Key: "root_map.count_timeout", Value: "9223372036855"}, // never parses (wraps to 224.192ms)
		{Key: "graph_cache.debounce_window", Value: "120"},      // healthy
	}, nil)

	if HasErrors(issues) {
		t.Fatalf("override layer must never produce errors, got: %v", issues)
	}
	warnFor(t, issues, "graph_cache.rebuild_interval", "settings override dropped")
	warnFor(t, issues, "root_map.count_timeout", "out of range")

	if c.GraphCache.RebuildInterval != 21600*time.Second || c.Source("graph_cache.rebuild_interval") != "default" {
		t.Errorf("rebuild_interval = %v (source %q), want the 6h default after the drop",
			c.GraphCache.RebuildInterval, c.Source("graph_cache.rebuild_interval"))
	}
	if c.RootMap.CountTimeout != 5*time.Second || c.Source("root_map.count_timeout") != "default" {
		t.Errorf("count_timeout = %v (source %q), want the 5s default — the wrapped value must never reach the config",
			c.RootMap.CountTimeout, c.Source("root_map.count_timeout"))
	}
	if c.GraphCache.DebounceWindow != 120*time.Second || c.Source("graph_cache.debounce_window") != "settings" {
		t.Errorf("healthy override must survive the drop pass: debounce=%v source=%q",
			c.GraphCache.DebounceWindow, c.Source("graph_cache.debounce_window"))
	}
}

// TestBuildSemanticFloorOverrideDropped is the V26 half of the class above at
// the surface an operator touches. The E-M6 gate is a REFUSAL switch: an
// out-of-range floor that survived the drop pass would silently stop answering
// queries, so the boot path must fall back to the 0 default (= gate off) and
// say so per key — the same attribution handler/settings.go turns into the 422.
func TestBuildSemanticFloorOverrideDropped(t *testing.T) {
	resetBuildEnv(t)

	c, issues := Build([]Override{
		{Key: "query.semantic_floor", Value: "1.0"},        // parses, V26 SeverityError
		{Key: "query.confident_threshold", Value: "0.009"}, // healthy neighbour
	}, nil)

	if HasErrors(issues) {
		t.Fatalf("override layer must never produce errors, got: %v", issues)
	}
	warnFor(t, issues, "query.semantic_floor", "settings override dropped")

	if c.Query.SemanticFloor != 0 || c.Source("query.semantic_floor") != "default" {
		t.Errorf("semantic_floor = %v (source %q), want the 0 default after the drop — a refusal switch may never boot out of range",
			c.Query.SemanticFloor, c.Source("query.semantic_floor"))
	}
	if c.Query.ConfidentThreshold != 0.009 || c.Source("query.confident_threshold") != "settings" {
		t.Errorf("healthy override must survive the drop pass: confident_threshold=%v source=%q",
			c.Query.ConfidentThreshold, c.Source("query.confident_threshold"))
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

// TestBuildSecretRefResolution died with chat.api_key in β8. It drove the three
// outcomes of admitOverride's secret branch — resolve, resolver error, no
// resolver — on the registry's last secret:"fp" key. The strecke is very much
// alive; what it lost is a MEMBER: server.db_password is the only sensitive key
// left, and it is mut:"restart", so admitOverride drops a row on it before the
// secret branch is ever reached (a substitution there would have been a test
// that passes without touching the code it names).
//
// The three outcomes moved one layer out, onto the injected-registry vehicle:
// synthreg_test.go's TestSynthSecretRefResolutionEndToEnd runs them through the
// real Build on a synthetic fp key transplanted onto a live field, and
// TestSynthSecretRefWarningNeverEchoesTheRefValue keeps the pasted-plaintext
// leak probe this test carried in its first subtest.
