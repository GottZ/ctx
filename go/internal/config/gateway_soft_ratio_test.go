package config

// The soft threshold of the day and month budgets (wave W06-18, design/06
// §4.11, RC-D register V54): gateway.soft_ratio, float, default 0.8, hot,
// strict, global-only; 0 = off; V54: range [0, 1), a number.

import (
	"strings"
	"testing"
)

// TestSoftRatioKey: the key is in the registry with the design default in
// the boot record, hot, strict, global-only.
func TestSoftRatioKey(t *testing.T) {
	const key = "gateway.soft_ratio"
	var e entry
	found := false
	for _, x := range registry() {
		if x.Key == key {
			e, found = x, true
		}
	}
	if !found {
		t.Fatalf("W06-18 gate (setting): %s is not a registry key", key)
	}
	if e.Mut != "hot" || e.Tenancy != "global-only" || !e.Strict || e.EnvVar != "CTX_GATEWAY_SOFT_RATIO" {
		t.Errorf("W06-18 gate (setting): %s mutability %q, tenancy %q, strict %v, env %q — want hot, global-only, strict, CTX_GATEWAY_SOFT_RATIO",
			key, e.Mut, e.Tenancy, e.Strict, e.EnvVar)
	}
	c, issues := cfgFrom(t, map[string]string{})
	args := BootDumpArgs(c, issues)
	var group map[string]any
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "gateway" {
			group, _ = args[i+1].(map[string]any)
		}
	}
	if got, ok := group["soft_ratio"]; !ok || got != 0.8 {
		t.Errorf("W06-18 gate (setting): soft_ratio in the boot record = %v (present %v), want 0.8", got, ok)
	}
}

// TestValidateSoftRatioV54 is V54: a negative share, 1 and above, and NaN
// are one error on the key; 0 (off), 0.5 and 0.8 are legal.
func TestValidateSoftRatioV54(t *testing.T) {
	const key = "gateway.soft_ratio"
	for _, tc := range []struct {
		val  string
		errs int
	}{{"0", 0}, {"0.5", 0}, {"0.8", 0}, {"0.999", 0}, {"1", 1}, {"1.5", 1}, {"-0.1", 1}, {"NaN", 1}} {
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
		if len(errs) != tc.errs || (tc.errs == 1 && !strings.Contains(errs[0].Msg, "[0, 1)")) {
			t.Errorf("W06-18 gate (V54): %s=%s: errors %+v, want %d naming the range [0, 1)", key, tc.val, errs, tc.errs)
		}
	}
}
