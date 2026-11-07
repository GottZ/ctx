package config

// The two keys of the /v1 chain rule (wave W06-14, design/06 §4.11):
// gateway.consumer_allow_external_default (bool, false) and
// gateway.consumer_monthly_cost_usd_default (float, 1.0, strict) — hot,
// global-only, in the boot record; V51: the cost budget > 0, finite.

import (
	"strings"
	"testing"
)

// TestEgressKeysAreHotGlobalKeys: both keys in the registry with their
// design defaults in the boot record; the cost budget parse-strict.
func TestEgressKeysAreHotGlobalKeys(t *testing.T) {
	byKey := map[string]entry{}
	for _, x := range registry() {
		byKey[x.Key] = x
	}
	c, issues := cfgFrom(t, map[string]string{})
	args := BootDumpArgs(c, issues)
	var group map[string]any
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "gateway" {
			group, _ = args[i+1].(map[string]any)
		}
	}
	for key, want := range map[string]struct {
		def    any
		strict bool
	}{
		"gateway.consumer_allow_external_default":   {false, false},
		"gateway.consumer_monthly_cost_usd_default": {1.0, true},
	} {
		e, ok := byKey[key]
		if !ok {
			t.Errorf("%s: not a registry key", key)
			continue
		}
		env := "CTX_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
		if e.Mut != "hot" || e.Tenancy != "global-only" || e.Strict != want.strict || e.EnvVar != env {
			t.Errorf("%s: mutability %q, tenancy %q, strict %v, env %q", key, e.Mut, e.Tenancy, e.Strict, e.EnvVar)
		}
		if got, ok := group[strings.TrimPrefix(key, "gateway.")]; !ok || got != want.def {
			t.Errorf("%s in the boot record = %v (present %v), want %v", key, got, ok, want.def)
		}
	}
	m := c.GatewaySettings().Meter
	if m.AllowExternal || m.MonthlyCostUSD != 1.0 {
		t.Errorf("meter settings: allow %v, cost %v — want false, 1.0", m.AllowExternal, m.MonthlyCostUSD)
	}
}

// TestValidateCostBudgetV51 is V51: 0 and a negative amount are one error on
// the key; 0.01 is legal.
func TestValidateCostBudgetV51(t *testing.T) {
	const key = "gateway.consumer_monthly_cost_usd_default"
	for _, tc := range []struct {
		val  string
		errs int
	}{{"0", 1}, {"-1", 1}, {"0.01", 0}, {"1.0", 0}, {"250", 0}} {
		c, issues := cfgFrom(t, map[string]string{key: tc.val})
		issues = append(issues, Validate(c)...)
		var errs []Issue
		for _, is := range issues {
			if is.Severity != SeverityError {
				continue
			}
			if is.Field != key {
				t.Errorf("%s=%s: unexpected error on %s: %s", key, tc.val, is.Field, is.Msg)
				continue
			}
			errs = append(errs, is)
		}
		if len(errs) != tc.errs || (tc.errs == 1 && !strings.Contains(errs[0].Msg, "no 0 sentinel")) {
			t.Errorf("%s=%s: errors %+v, want %d naming the missing 0 sentinel", key, tc.val, errs, tc.errs)
		}
	}
}
