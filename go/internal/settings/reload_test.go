package settings

// Unit gates for F2-W4 (no DB required — the pool-bound row loading is the
// thin store.LoadSettingOverrides glue, covered by the G02 integration tests
// and the post-merge live gate "SQL override → restart → takeover"):
//
//	(c) boot tolerance, negatively probed: corrupt JSONB value ⇒ WARN + env
//	    value active; unreachable DB ⇒ WARN + env-only config
//	(d) kill switch CTX_SETTINGS_DISABLE=1 ⇒ overrides ignored, one log line
//	    (nil pool proves the DB is never touched)
//	(e) leak scan: a known plaintext marker on every sensitive path the
//	    settings layer still has ⇒ marker in NO slog line across boot build,
//	    boot dump and reload (§3.3 invariant). The secret_ref half of this
//	    gate lost its subject in β8 — see TestLeakScanBootBuildReload.
//
// Fixture hygiene: documentation hosts (RFC 2606), runtime-concatenated
// markers, never secret-shaped literals.

import (
	"bytes"
	"context"
	"encoding/json"
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
// is untouched.
//
// The R-SCALE6 sub-case that rode here (a tenant embed.host override must not
// change the effective embed tuple, or the process-wide, scope-less
// context_embed_cache would be flushed across all tenants) left with the embed
// tuple in β7: both its key and the function it asserted against are gone, and
// the cache it protected is guarded on the pool side alone now (α5,
// events/listener.go). gaming.active carries the gate's statement — the drop
// happens before the merge, per row, for every global-only key.
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

}

// TestRetiredEmbedRowIsDroppedNotApplied is the settings half of the β7 gate
// (design/01 §7 W6, "embed-bezogene Row-Änderung erzeugt WARN-Drop und KEINEN
// Flush"). The flush half needs the real cache table and lives in
// handler/settings_integration_test.go (RetiredEmbedRowNoFlush); what is
// provable without a database is the drop itself, and it is what makes the
// flush unreachable: a row on one of the five cut embed keys never becomes a
// config.Override at all, so no effective value moves and there is nothing a
// flush could be owed to.
//
// It replaces TestEmbedCacheCoupledChangedOperatorPath, the positive pin β5
// built for the function that died here. Both directions are asserted — the
// row is dropped AND it is dropped for the retired-key reason, not because
// some other gate happened to swallow it.
func TestRetiredEmbedRowIsDroppedNotApplied(t *testing.T) {
	resetEnv(t)
	base, _ := config.Build(nil, nil)

	for _, key := range []string{"embed.host", "embed.protocol", "embed.model", "embed.api_key", "embed.num_ctx"} {
		t.Run(key, func(t *testing.T) {
			overrides, gateIssues := toOverrides(
				[]store.SettingOverride{row(key, `"http://embed.example:9999"`)},
				[]string{store.GlobalScope})
			if len(gateIssues) != 0 {
				t.Fatalf("the global-only gate must not be what drops %s: %+v", key, gateIssues)
			}
			next, issues := config.Build(overrides, nil)
			warned := false
			for _, is := range issues {
				if is.Field == key && is.Severity == config.SeverityWarn &&
					strings.Contains(is.Msg, "unknown settings key") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("row on retired %s must be dropped with the unknown-key WARN, got %+v", key, issues)
			}
			if next.Source(key) != base.Source(key) {
				t.Errorf("retired %s changed its source to %q — the row was applied",
					key, next.Source(key))
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

// TestToOverridesWarnsWithoutEmbeddingValue pins the value-free WARN at the
// row-conversion layer. The motivating case was a corrupt value on a SENSITIVE
// key — it could BE a pasted plaintext secret — and chat.api_key carried it
// until β8; no registry key is sensitive-and-admittable any more (see
// TestLeakScanBootBuildReload). The rule the test states never depended on the
// class: toOverrides runs BEFORE the registry entry is consulted at all, so the
// "name key and reason, never the raw value" contract is unconditional here and
// digest.mode (free-form string, unvalidated) carries it as well as any key.
func TestToOverridesWarnsWithoutEmbeddingValue(t *testing.T) {
	pasted := "PASTED-" + strings.Repeat("v", 30)
	overrides, issues := toOverrides([]store.SettingOverride{
		row("dream.language", `"pt-br"`),
		row("digest.mode", `{"oops":"`+pasted+`"}`),
	}, []string{store.GlobalScope})
	if len(overrides) != 1 || overrides[0].Key != "dream.language" {
		t.Fatalf("overrides = %+v, want exactly dream.language", overrides)
	}
	if len(issues) != 1 || issues[0].Field != "digest.mode" {
		t.Fatalf("issues = %+v, want one on digest.mode", issues)
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
	t.Setenv("CTX_DREAM_LANGUAGE", "pt-br")
	t.Setenv(EnvDisable, "1")
	buf := captureLogs(t)
	envCfg, _ := envBuild(t)
	st := config.NewStore(envCfg)

	if err := Reload(context.Background(), nil, st); err != nil {
		t.Fatalf("Reload with kill switch: %v", err)
	}
	snap := st.Snapshot()
	if snap.Dream.Language != "pt-br" || snap.Source("dream.language") != "env" {
		t.Errorf("snapshot must be the env build: language=%q source=%q",
			snap.Dream.Language, snap.Source("dream.language"))
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

// TestLeakScanBootBuildReload is THE leak scan, re-cut in β8. It still asserts
// the §3.3 invariant — no sensitive value reaches an slog line across boot
// build, boot dump, takeover line and both reload surfaces — but on the paths
// the settings layer still HAS.
//
// What the chat cut took from it: the secret:"fp" strecke. chat.api_key was the
// last registered carrier of that class, so no settings row can reach
// build.go's secret branch any more (server.db_password is sensitive but
// mut:"restart", i.e. dropped by admitOverride BEFORE any parse, redaction or
// resolver call). With it went two of the three markers this test used to
// carry: the resolver-provided secret_ref plaintext and the plaintext
// mistakenly stored AS the ref value. Both statements are alive and pinned one
// layer down, on an injected synthetic registry entry that still carries the
// class: config/synthreg_test.go, TestSynthSecretRefResolutionEndToEnd and
// TestSynthSecretRefWarningNeverEchoesTheRefValue. Asserting them here would
// need a fixture that no longer reaches the branch — so they are NOT asserted
// here.
//
// What is left is genuine and rides three real needles:
//
//	(a) envSecret on CONTEXT_DB_PASSWORD — the env path is untouched by the cut.
//	    The value lands in the snapshot and is rendered into the boot dump under
//	    secret:"presence", so a broken mask shows the marker. A REAL secret on a
//	    REAL live path.
//	(b) a healthy dream.language row holds the row → snapshot → logApplied
//	    strecke open with a NON-secret value: without an applied override the
//	    takeover line would be empty and (a)/(c) would be scanned over a log
//	    surface that never saw the override layer work.
//	(c) rowSecret on a server.db_password ROW pins the DROP path. Be precise
//	    about what this proves: admitOverride formats that WARN from e.Mut
//	    alone, so it is value-free BY CONSTRUCTION — the needle does not catch a
//	    message that was written to echo the value, it catches a future edit
//	    that starts echoing it (and any other line on the way: the toOverrides
//	    conversion, logIssues, logApplied, the boot dump). That is the whole
//	    claim, and it is worth having because server.db_password is the only
//	    sensitive key a row can still name.
func TestLeakScanBootBuildReload(t *testing.T) {
	resetEnv(t)
	// Two markers, assembled at runtime (never key-shaped literals).
	envSecret := "ENVMARKER-" + strings.Repeat("e", 30)
	rowSecret := "ROWMARKER-" + strings.Repeat("r", 30)

	t.Setenv("CONTEXT_DB_PASSWORD", envSecret)
	buf := captureLogs(t)

	rows := []store.SettingOverride{
		row("server.db_password", `"`+rowSecret+`"`), // drop path: mut:"restart" ⇒ WARN before any parse
		row("rerank.blend_weight", `"kaputt"`),       // corrupt-value WARN path
		row("dream.language", `"pt-br"`),             // healthy override, non-secret value
	}

	// Boot surface: build, issue lines, takeover line, effective boot dump.
	// resolve is nil — after β8 no admittable key is secret-class, so a resolver
	// would never be called; passing one would fake a path the build no longer
	// has.
	cfg, issues := buildWith(rows, nil, []string{store.GlobalScope})
	logIssues(issues)
	logApplied(cfg, rows)
	slog.Info("config: effective", config.BootDumpArgs(cfg, issues)...)

	if cfg.Dream.Language != "pt-br" || cfg.Source("dream.language") != config.SourceSettings {
		t.Fatalf("healthy override missing (language=%q source=%q) — the takeover line would be empty and the scan would prove nothing",
			cfg.Dream.Language, cfg.Source("dream.language"))
	}
	if cfg.Server.DBPass != envSecret {
		t.Fatalf("the env marker must be the effective db password — otherwise it rides nothing")
	}
	if cfg.Source("server.db_password") == config.SourceSettings {
		t.Fatalf("a server.db_password ROW must never become the effective value — the drop path is what this needle pins")
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
		"env secret (CONTEXT_DB_PASSWORD)": envSecret,
		"dropped row value":                rowSecret,
	} {
		if strings.Contains(logs, marker) {
			t.Errorf("LEAK: %s marker found in logs:\n%s", name, logs)
		}
	}
	// Sanity: the scan saw real content — key names ARE logged, including the
	// dropped one (the takeover and the drop must both be visible, §W4 gate b).
	for _, want := range []string{
		"server.db_password", "dream.language", "rerank.blend_weight",
		"overrides active", "config: effective",
	} {
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
