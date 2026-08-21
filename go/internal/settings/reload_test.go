package settings

// Unit gates for F2-W4 (no DB required — the pool-bound row loading is the
// thin store.LoadSettingOverrides glue, covered by the G02 integration tests
// and the post-merge live gate "SQL override → restart → takeover"):
//
//	(c) boot tolerance, negatively probed: corrupt JSONB value ⇒ WARN + env
//	    value active; unreachable DB ⇒ WARN + env-only config
//	(d) kill switch CTX_SETTINGS_DISABLE=1 ⇒ overrides ignored, one log line
//	    (nil pool proves the DB is never touched)
//	(e) leak scan: secret_ref set + known plaintext marker ⇒ marker in NO
//	    slog line across boot build, boot dump and reload (§3.3 invariant)
//
// Fixture hygiene: documentation hosts (RFC 2606), runtime-concatenated
// markers, never secret-shaped literals.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
)

// resetEnv gives each test a hermetic env: registry vars cleared, required
// DB password set, kill switch and master key explicitly unset.
func resetEnv(t *testing.T) {
	t.Helper()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(EnvDisable, "")
	t.Setenv(sealbox.EnvKey, "")
	t.Setenv(sealbox.EnvKeyPrev, "")
}

// captureLogs routes the default slog through a JSON handler into a buffer
// (restored on cleanup). JSON rendering covers nested boot-dump groups —
// every value that would reach the real log reaches the buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// envBuild is the validated env-only config (what main's fail-fast produced).
func envBuild(t *testing.T) (*config.Config, []config.Issue) {
	t.Helper()
	c, issues := config.FromEnv()
	issues = append(issues, config.Validate(c)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture must validate cleanly: %v", issues)
	}
	return c, issues
}

// deadPool returns a lazily-connected pool whose queries fail fast (loopback
// port 1 ⇒ immediate connection refused; nothing listens there).
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://ctxtest:ctxtest@127.0.0.1:1/ctxtest?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatalf("deadPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func row(key, jsonVal string) store.SettingOverride {
	return store.SettingOverride{
		Key: key, Scope: store.GlobalScope,
		Value: json.RawMessage(jsonVal), UpdatedAt: time.Now(),
	}
}

// tenantRow builds a non-global (tenant-scope) override row — the shape W3 will
// load on top of _global. Used to probe the global-only gate (§3.3/§4.6).
func tenantRow(key, scope, jsonVal string) store.SettingOverride {
	return store.SettingOverride{
		Key: key, Scope: scope,
		Value: json.RawMessage(jsonVal), UpdatedAt: time.Now(),
	}
}

// TestToOverridesGlobalOnlyTenantGate pins the §3.3/§4.6 invariant: a
// tenant-scope override on a global-only key is DROPPED with a WARN before it
// becomes a config.Override; a tenant-scope override on a tenant-overridable key
// passes through; a _global override on a global-only key (the operator path)
// is untouched. The embed.host sub-case is the R-SCALE6 guard — a tenant
// override must not change the effective embed tuple, so the process-wide,
// scope-less context_embed_cache is never flushed across all tenants.
func TestToOverridesGlobalOnlyTenantGate(t *testing.T) {
	resetEnv(t)
	const tenant = "work"

	t.Run("tenant override on global-only key is dropped + WARN", func(t *testing.T) {
		overrides, issues := toOverrides([]store.SettingOverride{
			tenantRow("gaming.active", tenant, `true`),
		}, []string{store.GlobalScope, tenant})
		if len(overrides) != 0 {
			t.Errorf("tenant override on global-only gaming.active must be dropped, got %+v", overrides)
		}
		if len(issues) != 1 || issues[0].Field != "gaming.active" || issues[0].Severity != config.SeverityWarn {
			t.Errorf("expected one WARN on gaming.active, got %+v", issues)
		}
	})

	t.Run("tenant override on tenant-overridable key passes", func(t *testing.T) {
		overrides, issues := toOverrides([]store.SettingOverride{
			tenantRow("rerank.blend_weight", tenant, `0.5`),
		}, []string{store.GlobalScope, tenant})
		if len(overrides) != 1 || overrides[0].Key != "rerank.blend_weight" {
			t.Errorf("tenant override on tenant-overridable key must pass, got %+v", overrides)
		}
		if len(issues) != 0 {
			t.Errorf("no WARN expected for a tenant-overridable key, got %+v", issues)
		}
	})

	t.Run("operator _global override on global-only key passes", func(t *testing.T) {
		overrides, issues := toOverrides([]store.SettingOverride{
			row("gaming.active", `true`),
		}, []string{store.GlobalScope})
		if len(overrides) != 1 || overrides[0].Key != "gaming.active" {
			t.Errorf("operator _global override on global-only key must pass, got %+v", overrides)
		}
		if len(issues) != 0 {
			t.Errorf("no WARN expected for the operator _global path, got %+v", issues)
		}
	})

	t.Run("tenant embed.host override does not flush the shared cache (R-SCALE6)", func(t *testing.T) {
		base, _ := config.Build(nil, nil)
		gatedOverrides, _ := toOverrides([]store.SettingOverride{
			tenantRow("embed.host", tenant, `"http://embed.example:9999"`),
		}, []string{store.GlobalScope, tenant})
		gated, _ := config.Build(gatedOverrides, nil)
		if EmbedCacheCoupledChanged(base, gated) {
			t.Errorf("tenant embed.host override must NOT change the effective embed tuple " +
				"(would flush the process-wide context_embed_cache for all tenants)")
		}
	})
}

// TestEmbedCacheCoupledChangedOperatorPath is the positive half the R-SCALE6
// sub-case above never made: the tenant gate proves what must NOT flush, and
// on its own it would stay green against a function that reports nothing at
// all. Since β5 that is a live risk — the dream_embed half of the comparison
// went with its tuple, leaving the embed host/protocol pair as the whole
// settings-side coupled surface.
//
// The model case pins the deliberate non-coverage: context_embed_cache keys on
// (text_hash, model), so a model change addresses other rows by itself and
// needs no flush (same reasoning as events/listener.go's coupledPair).
func TestEmbedCacheCoupledChangedOperatorPath(t *testing.T) {
	resetEnv(t)
	base, _ := config.Build(nil, nil)

	cases := []struct {
		name string
		key  string
		val  string
		want bool
	}{
		{"operator embed.host move", "embed.host", `"http://embed.example:9999"`, true},
		{"operator embed.protocol switch", "embed.protocol", `"openai"`, true},
		{"model change alone", "embed.model", `"other-embed-4b"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			overrides, _ := toOverrides([]store.SettingOverride{row(c.key, c.val)}, []string{store.GlobalScope})
			if len(overrides) != 1 {
				t.Fatalf("_global override on %s must pass the gate, got %+v", c.key, overrides)
			}
			next, _ := config.Build(overrides, nil)
			if got := EmbedCacheCoupledChanged(base, next); got != c.want {
				t.Errorf("EmbedCacheCoupledChanged after %s = %v, want %v", c.key, got, c.want)
			}
		})
	}
}

// --- JSONB unwrapping ---.

func TestScalarValue(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{`"db-model"`, "db-model", false},
		{`0.7`, "0.7", false},
		{`4096`, "4096", false},
		{`1e3`, "1e3", false}, // literal preserved — the typed parser owns it
		{`true`, "true", false},
		{`false`, "false", false},
		{`""`, "", false}, // empty string is a value (parser/admission decides)
		{`null`, "", true},
		{`[1,2]`, "", true},
		{`{"a":1}`, "", true},
		{``, "", true},
		{`   `, "", true},
	}
	for _, c := range cases {
		got, err := ScalarValue(json.RawMessage(c.in))
		if (err != nil) != c.wantErr {
			t.Errorf("ScalarValue(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("ScalarValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestToOverridesWarnsWithoutEmbeddingValue(t *testing.T) {
	// A corrupt value on a sensitive key could BE a pasted plaintext secret —
	// the WARN must name key and reason, never the raw value.
	pasted := "PASTED-" + strings.Repeat("v", 30)
	overrides, issues := toOverrides([]store.SettingOverride{
		row("chat.model", `"db-model"`),
		row("chat.api_key", `{"oops":"`+pasted+`"}`),
	}, []string{store.GlobalScope})
	if len(overrides) != 1 || overrides[0].Key != "chat.model" {
		t.Fatalf("overrides = %+v, want exactly chat.model", overrides)
	}
	if len(issues) != 1 || issues[0].Field != "chat.api_key" {
		t.Fatalf("issues = %+v, want one on chat.api_key", issues)
	}
	if strings.Contains(issues[0].Msg, pasted) {
		t.Errorf("WARN embeds the raw value: %q", issues[0].Msg)
	}
}

// --- Gate (c): boot tolerance, negatively probed ---.

func TestBuildWithCorruptValueKeepsEnv(t *testing.T) {
	resetEnv(t)
	t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.4")

	cfg, issues := buildWith([]store.SettingOverride{
		row("rerank.blend_weight", `"kaputt"`), // §5 negative probe (valid JSON, unparseable float)
	}, nil, []string{store.GlobalScope})

	if config.HasErrors(issues) {
		t.Fatalf("tolerance broken — override layer produced errors: %v", issues)
	}
	var warned bool
	for _, is := range issues {
		if is.Field == "rerank.blend_weight" && is.Severity == config.SeverityWarn {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no WARN for the corrupt override in %v", issues)
	}
	if cfg.Rerank.BlendWeight != 0.4 || cfg.Source("rerank.blend_weight") != "env" {
		t.Errorf("env value must stay active: %v (source %q)",
			cfg.Rerank.BlendWeight, cfg.Source("rerank.blend_weight"))
	}
}

func TestBootstrapUnreachableDBFallsBackToEnv(t *testing.T) {
	resetEnv(t)
	buf := captureLogs(t)
	envCfg, envIssues := envBuild(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, gotIssues := Bootstrap(ctx, deadPool(t), envCfg, envIssues)

	if got != envCfg {
		t.Errorf("Bootstrap must return the env config unchanged on load failure")
	}
	if len(gotIssues) != len(envIssues) {
		t.Errorf("issues = %v, want env issues unchanged", gotIssues)
	}
	if !strings.Contains(buf.String(), "loading overrides failed") {
		t.Errorf("missing WARN, log: %s", buf.String())
	}
}

// --- Gate (d): kill switch ---.

func TestBootstrapKillSwitchSkipsDB(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvDisable, "1")
	buf := captureLogs(t)
	envCfg, envIssues := envBuild(t)

	// nil pool: with the switch set, the DB must never be touched — any
	// access would panic here.
	got, _ := Bootstrap(context.Background(), nil, envCfg, envIssues)

	if got != envCfg {
		t.Errorf("kill switch must return the env config unchanged")
	}
	if !strings.Contains(buf.String(), "override layer disabled") {
		t.Errorf("missing the one clear log line, log: %s", buf.String())
	}
}

func TestReloadKillSwitchRebuildsEnvOnly(t *testing.T) {
	resetEnv(t)
	t.Setenv("CTX_CHAT_MODEL", "env-model")
	t.Setenv(EnvDisable, "1")
	buf := captureLogs(t)
	envCfg, _ := envBuild(t)
	st := config.NewStore(envCfg)

	if err := Reload(context.Background(), nil, st); err != nil {
		t.Fatalf("Reload with kill switch: %v", err)
	}
	snap := st.Snapshot()
	if snap.Chat.Model != "env-model" || snap.Source("chat.model") != "env" {
		t.Errorf("snapshot must be the env build: model=%q source=%q",
			snap.Chat.Model, snap.Source("chat.model"))
	}
	if !strings.Contains(buf.String(), "override layer disabled") {
		t.Errorf("missing kill-switch log line, log: %s", buf.String())
	}
}

// --- Reload error path: old snapshot stays active ---.

func TestReloadKeepsOldSnapshotOnLoadFailure(t *testing.T) {
	resetEnv(t)
	buf := captureLogs(t)
	envCfg, _ := envBuild(t)
	st := config.NewStore(envCfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Reload(ctx, deadPool(t), st)

	if err == nil {
		t.Fatalf("Reload against an unreachable DB must return an error")
	}
	if st.Snapshot() != envCfg {
		t.Errorf("active snapshot must stay untouched on reload failure")
	}
	if !strings.Contains(buf.String(), "keeping the active snapshot") {
		t.Errorf("missing WARN, log: %s", buf.String())
	}
}

// --- Gate (e): leak scan over the full boot/build/reload log surface ---.

func TestLeakScanBootBuildReload(t *testing.T) {
	resetEnv(t)
	// Three markers, assembled at runtime (never key-shaped literals):
	// an env-provided secret, a resolver-provided secret_ref plaintext, and
	// a plaintext mistakenly stored AS the ref value (pre-W6 write gate).
	envSecret := "ENVMARKER-" + strings.Repeat("e", 30)
	dbSecret := "DBMARKER-" + strings.Repeat("d", 30)
	pastedSecret := "PASTEDMARKER-" + strings.Repeat("p", 30)

	t.Setenv("CTX_CHAT_HOST", "http://chat.example:8089")
	t.Setenv("CTX_DREAM_API_KEY", envSecret)
	buf := captureLogs(t)

	rows := []store.SettingOverride{
		row("chat.api_key", `"prov-main"`), // resolves to dbSecret
		// The resolver error path needs a REGISTERED secret key — a row on a cut
		// key is dropped as unknown before the resolver ever sees it, and the
		// pasted-plaintext marker would then be scanned for in a log surface it
		// never reached. It rode on chat_fallback.api_key until β4 cut the tuple;
		// embed.api_key is the same class (secret:"fp", tenant-overridable) and
		// outlives dream_embed's.
		row("embed.api_key", `"`+pastedSecret+`"`), // ref value IS a plaintext ⇒ resolver error path
		row("rerank.blend_weight", `"kaputt"`),     // corrupt-value WARN path
		row("chat.model", `"db-model"`),            // healthy non-secret override
	}
	resolve := func(name string) (string, error) {
		if name == "prov-main" {
			return dbSecret, nil
		}
		// Mirrors the store.ResolveSecret contract: name-free, value-free.
		return "", fmt.Errorf("secrets: no such secret in scope %q — create it first", store.GlobalScope)
	}

	// Boot surface: build, issue lines, takeover line, effective boot dump.
	cfg, issues := buildWith(rows, resolve, []string{store.GlobalScope})
	logIssues(issues)
	logApplied(cfg, rows)
	slog.Info("config: effective", config.BootDumpArgs(cfg, issues)...)

	if cfg.Chat.APIKey != dbSecret {
		t.Fatalf("secret_ref did not resolve in-memory")
	}
	if cfg.Chat.Model != "db-model" {
		t.Fatalf("healthy override missing — scan would prove nothing")
	}

	// Reload surfaces: kill-switch rebuild (Replace path) + load failure.
	st := config.NewStore(cfg)
	t.Setenv(EnvDisable, "1")
	if err := Reload(context.Background(), nil, st); err != nil {
		t.Fatalf("reload (disabled): %v", err)
	}
	t.Setenv(EnvDisable, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = Reload(ctx, deadPool(t), st) // expected error; its WARN is part of the scanned surface

	logs := buf.String()
	for name, marker := range map[string]string{
		"env secret": envSecret, "resolved secret_ref": dbSecret, "pasted ref value": pastedSecret,
	} {
		if strings.Contains(logs, marker) {
			t.Errorf("LEAK: %s marker found in logs:\n%s", name, logs)
		}
	}
	// Sanity: the scan saw real content — key names and the harmless ref
	// name ARE logged (the takeover must be visible, §W4 gate b).
	for _, want := range []string{"chat.api_key", "rerank.blend_weight", "overrides active", "config: effective"} {
		if !strings.Contains(logs, want) {
			t.Errorf("scan surface incomplete — %q missing from logs", want)
		}
	}
}

// --- diffIssues: Bootstrap logs only the override layer's delta ---.

func TestDiffIssues(t *testing.T) {
	a := config.Issue{Field: "x", Severity: config.SeverityWarn, Msg: "m1"}
	b := config.Issue{Field: "y", Severity: config.SeverityWarn, Msg: "m2"}
	got := diffIssues([]config.Issue{a, b}, []config.Issue{a})
	if len(got) != 1 || got[0] != b {
		t.Errorf("diffIssues = %v, want [%v]", got, b)
	}
}
