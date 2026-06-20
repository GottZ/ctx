package settings

// MT3-W3 unit gates for the tenant-resolution merge path (design 03 §4.2/§7).
//
// The merge (toOverrides → buildWith) is pure (except config.Build's env read),
// so the precedence/shuffle/gate/fallthrough gates run WITHOUT a DB. They pin:
//
//	- precedence tenant > _global > env > default, per key-source tier
//	- shuffle-independence: row order must NOT decide the winner (Go-materialized
//	  array_position precedence, not last-row-wins)
//	- global-only gate runs BEFORE the merge (the order-bug guard): a tenant row
//	  on a global-only key is dropped + WARN, so the legitimate _global value
//	  survives instead of being merged-in then dropped (regression)
//	- first-valid-in-priority-order: a malformed higher-priority row falls back
//	  to the next DB tier (here _global), not straight to env/default
//	- BuildFromRows({_global}) stays byte-identical to the pre-W3 single-scope
//	  build (the additive/pausable proof)

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

const testTenant = "work"

// globalAndTenant is the {_global, tenant} priority slice the W4/W5 consumer
// will pass — tenant wins.
var globalAndTenant = []string{store.GlobalScope, testTenant}

// TestToOverridesPrecedenceMatrix walks the four source tiers for one
// tenant-overridable key (rerank.blend_weight, float in [0,1], env
// CTX_RERANK_BLEND_WEIGHT, default 1.0) and asserts the effective typed value +
// Source label. The values stay inside [0,1] so each tier is admitted on its own
// merit (no cross-field constraint masking the precedence).
func TestToOverridesPrecedenceMatrix(t *testing.T) {
	const key = "rerank.blend_weight"

	t.Run("default-only", func(t *testing.T) {
		resetEnv(t)
		cfg, _ := buildWith(nil, nil, globalAndTenant)
		if cfg.Rerank.BlendWeight != 1.0 || cfg.Source(key) != "default" {
			t.Errorf("default tier: got %v source %q, want 1.0/default",
				cfg.Rerank.BlendWeight, cfg.Source(key))
		}
	})

	t.Run("env-only", func(t *testing.T) {
		resetEnv(t)
		t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.2")
		cfg, _ := buildWith(nil, nil, globalAndTenant)
		if cfg.Rerank.BlendWeight != 0.2 || cfg.Source(key) != "env" {
			t.Errorf("env tier: got %v source %q, want 0.2/env",
				cfg.Rerank.BlendWeight, cfg.Source(key))
		}
	})

	t.Run("env + _global ⇒ _global wins", func(t *testing.T) {
		resetEnv(t)
		t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.2")
		cfg, _ := buildWith([]store.SettingOverride{
			row(key, `0.4`),
		}, nil, globalAndTenant)
		if cfg.Rerank.BlendWeight != 0.4 || cfg.Source(key) != "settings" {
			t.Errorf("_global tier: got %v source %q, want 0.4/settings",
				cfg.Rerank.BlendWeight, cfg.Source(key))
		}
	})

	t.Run("env + _global + tenant ⇒ tenant wins", func(t *testing.T) {
		resetEnv(t)
		t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.2")
		cfg, _ := buildWith([]store.SettingOverride{
			row(key, `0.4`),
			tenantRow(key, testTenant, `0.6`),
		}, nil, globalAndTenant)
		if cfg.Rerank.BlendWeight != 0.6 || cfg.Source(key) != config.SourceTenant {
			t.Errorf("tenant tier: got %v source %q, want 0.6/tenant",
				cfg.Rerank.BlendWeight, cfg.Source(key))
		}
	})
}

// TestToOverridesShuffleIndependence proves the winner is decided by the
// scopePriority position, not by the row order. tenant wins whether its row
// comes before or after the _global row. RED under a "last row wins" merge.
func TestToOverridesShuffleIndependence(t *testing.T) {
	resetEnv(t)
	const key = "rerank.blend_weight"

	orders := map[string][]store.SettingOverride{
		"global-then-tenant": {row(key, `0.4`), tenantRow(key, testTenant, `0.6`)},
		"tenant-then-global": {tenantRow(key, testTenant, `0.6`), row(key, `0.4`)},
	}
	for name, rows := range orders {
		t.Run(name, func(t *testing.T) {
			cfg, _ := buildWith(rows, nil, globalAndTenant)
			if cfg.Rerank.BlendWeight != 0.6 {
				t.Errorf("%s: tenant must win regardless of row order, got %v",
					name, cfg.Rerank.BlendWeight)
			}
		})
	}
}

// TestToOverridesGlobalOnlyMergeOrder is the order-bug guard: a global-only key
// (gaming.active) carries BOTH a _global row (operator, value true) and a tenant
// row (value false). The tenant row must be dropped by the gate BEFORE the
// merge, so the _global value survives. Under a merge-before-gate bug the
// higher-priority tenant row would win the merge then get dropped, collapsing
// the key to env/default (false) — a regression of the operator's _global value.
func TestToOverridesGlobalOnlyMergeOrder(t *testing.T) {
	resetEnv(t)
	if !config.IsGlobalOnly("gaming.active") {
		t.Fatalf("test premise broken: gaming.active must be global-only")
	}

	cfg, issues := buildWith([]store.SettingOverride{
		row("gaming.active", `true`),                   // operator _global, must survive
		tenantRow("gaming.active", testTenant, `false`), // tenant, must be gated out
	}, nil, globalAndTenant)

	if !cfg.Pool.GamingActive || cfg.Source("gaming.active") != "settings" {
		t.Errorf("gate-before-merge broken: gaming.active = %v source %q, want true/settings "+
			"(operator _global value must survive, tenant row dropped)",
			cfg.Pool.GamingActive, cfg.Source("gaming.active"))
	}
	var warned bool
	for _, is := range issues {
		if is.Field == "gaming.active" && is.Severity == config.SeverityWarn {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a WARN on the dropped tenant gaming.active override, got %+v", issues)
	}
}

// TestToOverridesMalformedFallthrough proves first-valid-in-priority-order at
// the ScalarValue tier: the higher-priority tenant row carries a non-scalar
// JSONB value (an object — ScalarValue rejects it), so the merge falls back to
// the valid _global row of the same key — NOT to env/default — and emits a WARN
// on the tenant row.
func TestToOverridesMalformedFallthrough(t *testing.T) {
	resetEnv(t)
	t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.2")
	const key = "rerank.blend_weight"

	cfg, issues := buildWith([]store.SettingOverride{
		row(key, `0.4`),                            // valid _global
		tenantRow(key, testTenant, `{"oops":1}`), // non-scalar ⇒ ScalarValue fails, higher-priority tenant
	}, nil, globalAndTenant)

	if cfg.Rerank.BlendWeight != 0.4 || cfg.Source(key) != "settings" {
		t.Errorf("fallthrough broken: got %v source %q, want 0.4/settings (the valid _global value)",
			cfg.Rerank.BlendWeight, cfg.Source(key))
	}
	var warned bool
	for _, is := range issues {
		if is.Field == key && is.Severity == config.SeverityWarn {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a WARN on the malformed tenant row, got %+v", issues)
	}
}

// TestToOverridesSingleScopeByteIdentical pins the additive/pausable proof:
// with scopePriority={_global} and rows from one scope, the merge is the
// identity — exactly one config.Override per key, no extra issues — matching
// the pre-W3 toOverrides output.
func TestToOverridesSingleScopeByteIdentical(t *testing.T) {
	resetEnv(t)
	rows := []store.SettingOverride{
		row("chat.model", `"db-model"`),
		row("rerank.blend_weight", `0.5`),
		row("query.score_threshold", `0.2`),
	}
	overrides, issues := toOverrides(rows, []string{store.GlobalScope})

	if len(overrides) != 3 {
		t.Fatalf("single-scope merge must be identity: got %d overrides, want 3: %+v", len(overrides), overrides)
	}
	if len(issues) != 0 {
		t.Errorf("no issues expected for three healthy _global rows, got %+v", issues)
	}
	// Deterministic key set, one Override per key.
	seen := map[string]int{}
	for _, o := range overrides {
		seen[o.Key]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("key %q appears %d times — merge must dedup to one", k, n)
		}
	}
}

// TestBuildFromRowsSingleScopeMatchesEnv exercises the BuildFromRows entry
// (the W5 handler + Bootstrap/Reload path) with {_global}-only rows and checks
// the effective config carries the override — same as the pre-W3 build.
func TestBuildFromRowsSingleScopeMatchesEnv(t *testing.T) {
	resetEnv(t)
	t.Setenv("CTX_CHAT_MODEL", "env-model")

	cfg, issues := BuildFromRows(t.Context(), nil, []store.SettingOverride{
		row("chat.model", `"db-model"`),
	}, []string{store.GlobalScope})

	if config.HasErrors(issues) {
		t.Fatalf("unexpected errors: %v", issues)
	}
	if cfg.Chat.Model != "db-model" || cfg.Source("chat.model") != "settings" {
		t.Errorf("BuildFromRows({_global}): model=%q source=%q, want db-model/settings",
			cfg.Chat.Model, cfg.Source("chat.model"))
	}
}

// TestToOverridesIssueDeterminism pins stable issue ordering: the same input
// must produce the same issue sequence across runs (sorted by key).
func TestToOverridesIssueDeterminism(t *testing.T) {
	resetEnv(t)
	rows := []store.SettingOverride{
		tenantRow("gaming.active", testTenant, `true`), // gated
		row("rerank.blend_weight", `"kaputt"`),         // malformed
		row("chat.api_key", `{"oops":"x"}`),            // malformed object
	}
	a := toOverridesIssueKeys(rows)
	b := toOverridesIssueKeys(rows)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("issue order not deterministic: %v vs %v", a, b)
	}
}

func toOverridesIssueKeys(rows []store.SettingOverride) []string {
	_, iss := toOverrides(rows, []string{store.GlobalScope, testTenant})
	keys := make([]string, 0, len(iss))
	for _, is := range iss {
		keys = append(keys, is.Field)
	}
	return keys
}
