// Package settings owns the F2 reload pipeline: it loads the
// context_settings override layer from the DB, resolves secret_ref values
// through the sealbox, builds the effective config via config.Build and
// publishes it — at boot into the initial snapshot (Bootstrap), at runtime
// through config.Store.Replace (Reload, called by the W5/W6 handlers and the
// G16 NOTIFY listener).
//
// Boot tolerance (W17) is the package invariant: NOTHING in this layer is
// ever fatal. Kill switch, unreachable DB, missing table (pre-051 schema),
// corrupt values, missing or wrong master key — each degrades to a WARN (or
// one Info line for the kill switch) while the env/default config stays
// active.
//
// Logging invariant (§3.3): log lines name KEYS and SOURCES, never resolved
// sensitive values — resolved secret_ref plaintexts exist only inside the
// in-memory snapshot. Pinned by the leak-scan test.
package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
)

// BackendSecretResolver builds the api_key_ref resolver for the backend
// pool (F3-P1). The sealbox is constructed per call so a master-key rotation
// (§3.6 sweep) needs no resolver rebuild; errors stay name- and value-free
// by the store.ResolveSecret contract. Backends with unresolvable refs stay
// keyless in the snapshot (401 → ClassAuth → reload self-heal).
func BackendSecretResolver(pool *pgxpool.Pool) backends.SecretResolver {
	return func(ctx context.Context, name string) (string, error) {
		if strings.TrimSpace(os.Getenv(sealbox.EnvKey)) == "" {
			return "", fmt.Errorf("secrets master key not configured")
		}
		box, err := sealbox.FromEnv()
		if err != nil {
			return "", fmt.Errorf("secrets master key unusable")
		}
		plaintext, err := store.ResolveSecret(ctx, pool, box, name, store.GlobalScope)
		if err != nil {
			return "", err
		}
		return string(plaintext), nil
	}
}

// EnvDisable is the kill switch: CTX_SETTINGS_DISABLE=1 ignores the whole
// override layer (boot AND reload). Env-only by design — the switch that
// turns off DB overrides cannot itself be a DB override.
const EnvDisable = "CTX_SETTINGS_DISABLE"

// Disabled reports whether the settings override layer is switched off.
func Disabled() bool {
	return os.Getenv(EnvDisable) == "1"
}

// Bootstrap is the boot entry (§2.1 steps 4–6): load overrides, build the
// effective config on top of the validated env config. envCfg/envIssues are
// the env-only build the boot already fail-fasted on; they are returned
// unchanged whenever the override layer contributes nothing (kill switch,
// load failure, zero rows) — the no-override boot is then BYTE-identical to
// the pre-F2 boot by construction, not by re-derivation.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, envCfg *config.Config, envIssues []config.Issue) (*config.Config, []config.Issue) {
	if Disabled() {
		slog.Info("settings: override layer disabled (" + EnvDisable + "=1) — using env/default config only")
		return envCfg, envIssues
	}

	rows, err := loadOverrideRows(ctx, pool)
	if err != nil {
		// Missing table (pre-051 DB), DB hiccup, anything: WARN + env-only.
		slog.Warn("settings: loading overrides failed — continuing with env-only config", "error", err)
		return envCfg, envIssues
	}
	if len(rows) == 0 {
		return envCfg, envIssues
	}

	cfg, issues := BuildFromRows(ctx, pool, rows)

	// Log only what the override layer CHANGED: issues already present in the
	// env-only pass were logged by the boot loop before the pool existed.
	logIssues(diffIssues(issues, envIssues))
	logApplied(cfg, rows)
	return cfg, issues
}

// Reload is the F2 contract function (§2.4): LoadOverrides → Build (with
// secret_ref resolution) → Validate → store.Replace. Callers are the
// settings/secrets handlers (W5/W6) and the NOTIFY listener (G16). On any
// error the previous snapshot stays active — the WARN names what failed.
//
// X2 (G16): when the swap changes an embed-cache-coupled value (effective
// embed/dream_embed host or protocol — including a DELETE reverting to env
// and a psql edit arriving via NOTIFY), the embed cache is flushed AFTER the
// swap: post-swap only the new backend fills it, whereas a pre-swap flush
// would let in-flight requests on the old snapshot repollute it for good.
func Reload(ctx context.Context, pool *pgxpool.Pool, st *config.Store) error {
	var rows []store.SettingOverride
	if Disabled() {
		slog.Info("settings: reload with override layer disabled (" + EnvDisable + "=1) — rebuilding env-only snapshot")
	} else {
		var err error
		rows, err = loadOverrideRows(ctx, pool)
		if err != nil {
			slog.Warn("settings: reload failed to load overrides — keeping the active snapshot", "error", err)
			return fmt.Errorf("settings: reload: %w", err)
		}
	}

	cfg, issues := BuildFromRows(ctx, pool, rows)
	logIssues(issues)

	prev := st.Snapshot()
	if err := st.Replace(cfg); err != nil {
		// Replace re-validates; errors here mean an env-level invariant broke
		// (unreachable while env is immutable per process, but never silent).
		slog.Warn("settings: reload rejected by validation — keeping the active snapshot", "error", err)
		return fmt.Errorf("settings: reload: %w", err)
	}
	logApplied(cfg, rows)

	if EmbedCacheCoupledChanged(prev, cfg) {
		n, err := embedcache.Flush(ctx, pool)
		if err != nil {
			// The new snapshot is live either way; a failed flush leaves stale
			// vectors serving (R5) — loud, and the next coupled write retries.
			slog.Error("settings: embed-cache flush after coupled change failed — stale vectors may serve", "error", err)
			return fmt.Errorf("settings: reload: embed cache flush: %w", err)
		}
		slog.Info("settings: embed-cache-coupled value changed — flushed context_embed_cache", "rows", n)
	}
	return nil
}

// EmbedCacheCoupledChanged reports whether the EFFECTIVE embed-cache-coupled
// values differ between two generations: the host/protocol of the query-path
// and dream embed tuples (resolved through the dream_embed→embed inheritance,
// so an embed.host change that propagates into the dream tuple counts).
// Model changes are deliberately NOT covered — the cache keys on model, and
// model stays mut:"coupled" (re-embed migration, not overridable).
func EmbedCacheCoupledChanged(a, b *config.Config) bool {
	ae, be := a.EmbedBackend(), b.EmbedBackend()
	ad, bd := a.DreamEmbedBackend(), b.DreamEmbedBackend()
	return ae.Host != be.Host || ae.Protocol != be.Protocol ||
		ad.Host != bd.Host || ad.Protocol != bd.Protocol
}

// loadOverrideRows reads the global-scope override rows.
//
// TODO(multi-tenant): this loads scope='_global' exclusively — the
// server-wide config. Per-tenant settings resolution must read the caller's
// tenant scope ON TOP of GlobalScope (precedence tenant > global > env >
// default) and build per-tenant snapshots instead of one process-global one;
// the 051 scope column already carries the dimension.
func loadOverrideRows(ctx context.Context, pool *pgxpool.Pool) ([]store.SettingOverride, error) {
	return store.LoadSettingOverrides(ctx, pool, store.GlobalScope)
}

// BuildFromRows is the pool-bound build: rows + a sealbox-backed resolver
// into config.Build. Split from the row conversion so the precedence/
// tolerance/leak gates run without a DB (buildWith). Exported for the W5
// settings handler: a PUT validates by building the candidate config from
// the would-be row set through EXACTLY this path — no second validation
// logic that could drift from what the reload actually applies.
func BuildFromRows(ctx context.Context, pool *pgxpool.Pool, rows []store.SettingOverride) (*config.Config, []config.Issue) {
	resolve, keyIssue := poolResolver(ctx, pool)
	cfg, issues := buildWith(rows, resolve)
	if keyIssue != nil {
		issues = append([]config.Issue{*keyIssue}, issues...)
	}
	return cfg, issues
}

// buildWith converts rows to typed overrides and builds the effective config.
// Pure except for config.Build's env read — fully unit-testable.
func buildWith(rows []store.SettingOverride, resolve config.SecretResolver) (*config.Config, []config.Issue) {
	overrides, convIssues := toOverrides(rows)
	cfg, issues := config.Build(overrides, resolve)
	return cfg, append(convIssues, issues...)
}

// poolResolver builds the secret_ref resolver for one build pass. A missing
// master key is NOT an issue by itself (secrets are optional; installs
// without CTX_SECRETS_KEY get the per-field WARN from config.Build only if a
// secret_ref override actually exists). A SET but malformed key is a config
// error worth one WARN — the sealbox error names the env var, never a value.
func poolResolver(ctx context.Context, pool *pgxpool.Pool) (config.SecretResolver, *config.Issue) {
	if strings.TrimSpace(os.Getenv(sealbox.EnvKey)) == "" {
		return nil, nil
	}
	box, err := sealbox.FromEnv()
	if err != nil {
		return nil, &config.Issue{Field: "settings", Severity: config.SeverityWarn,
			Msg: fmt.Sprintf("master key unusable (%v) — secret_ref overrides will be ignored", err)}
	}
	return func(name string) (string, error) {
		plaintext, err := store.ResolveSecret(ctx, pool, box, name, store.GlobalScope)
		if err != nil {
			return "", err // name-free by the store.ResolveSecret contract
		}
		return string(plaintext), nil
	}, nil
}

// toOverrides unwraps the JSONB row values into the raw scalar string form
// config.Build's typed parsers consume. Unsupported shapes degrade to a WARN
// per row; messages never embed the raw value — a corrupt value on a
// sensitive key could BE a mistakenly-pasted plaintext secret.
//
// global-only gate (MT3-W2 §4.6): this pass is the only place the override's
// SCOPE is still present (config.Override is {Key,Value} — build.go drops the
// scope), so the tenancy gate lives here. A non-_global (tenant-scope) override
// on a global-only key is DROPPED with a WARN before it becomes a
// config.Override — fail-closed against a tenant flipping a server-wide switch,
// most critically the embed-cache-coupled keys whose flush nukes the
// process-wide shared cache for ALL tenants (R-SCALE6). _global rows (the
// operator path) and tenant rows on tenant-overridable keys pass through
// untouched. Today loadOverrideRows reads only _global, so this gate is inert
// (W3 introduces the tenant rows it guards) — the no-override boot stays
// byte-identical.
func toOverrides(rows []store.SettingOverride) ([]config.Override, []config.Issue) {
	overrides := make([]config.Override, 0, len(rows))
	var issues []config.Issue
	for _, row := range rows {
		if row.Scope != store.GlobalScope && config.IsGlobalOnly(row.Key) {
			issues = append(issues, config.Issue{Field: row.Key, Severity: config.SeverityWarn,
				Msg: fmt.Sprintf("tenant override on global-only key ignored (scope %q)", row.Scope)})
			continue
		}
		raw, err := ScalarValue(row.Value)
		if err != nil {
			issues = append(issues, config.Issue{Field: row.Key, Severity: config.SeverityWarn,
				Msg: fmt.Sprintf("%v — override ignored, env/default value stays", err)})
			continue
		}
		overrides = append(overrides, config.Override{Key: row.Key, Value: raw})
	}
	return overrides, issues
}

// ScalarValue unwraps one JSONB scalar: strings unquote, numbers and booleans
// keep their literal text (the registry parser owns the typing — no float
// round-trip drift). Arrays, objects and null are not override values.
// Exported for the W5 handler, which unwraps PUT bodies through the same
// rules the reload applies to persisted rows.
func ScalarValue(raw json.RawMessage) (string, error) {
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return "", fmt.Errorf("empty value")
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("malformed JSON string value")
		}
		return s, nil
	case '{', '[':
		return "", fmt.Errorf("value must be a JSON scalar (string, number or boolean)")
	}
	if t == "null" {
		return "", fmt.Errorf("null is not a value — DELETE the override instead")
	}
	return t, nil
}

// diffIssues returns the issues in got that are not already in baseline —
// the boot logs the env-only findings before the pool exists; Bootstrap then
// logs only the override layer's delta instead of repeating them.
func diffIssues(got, baseline []config.Issue) []config.Issue {
	seen := make(map[config.Issue]bool, len(baseline))
	for _, is := range baseline {
		seen[is] = true
	}
	var out []config.Issue
	for _, is := range got {
		if !seen[is] {
			out = append(out, is)
		}
	}
	return out
}

// logIssues writes one line per finding at its severity. Issue messages obey
// the §3.3 convention (no resolved secrets, no raw URLs) by construction.
func logIssues(issues []config.Issue) {
	for _, is := range issues {
		if is.Severity == config.SeverityError {
			slog.Error("settings: override error", "field", is.Field, "msg", is.Msg)
			continue
		}
		slog.Warn("settings: override issue", "field", is.Field, "msg", is.Msg)
	}
}

// logApplied makes the takeover visible (W4 gate b): which keys are active
// from the DB layer in this generation. Keys and counts only — never values.
func logApplied(cfg *config.Config, rows []store.SettingOverride) {
	applied := make([]string, 0, len(rows))
	for _, row := range rows {
		if cfg.Source(row.Key) == "settings" {
			applied = append(applied, row.Key)
		}
	}
	slog.Info("settings: overrides active",
		"applied", len(applied), "loaded", len(rows), "keys", applied)
}
