package config

// pool.capability_probe_interval (FOLGE-Z Z1): the cadence of the backend
// capability probe — a registry key with default 600 s, hot, global-only,
// env CTX_POOL_CAPABILITY_PROBE_INTERVAL, and V59: at least one second (0
// would start the next probe round without a pause).

import (
	"strings"
	"testing"
	"time"
)

const capabilityProbeKey = "pool.capability_probe_interval"

func TestCapabilityProbeIntervalKey(t *testing.T) {
	var e *entry
	for _, x := range registry() {
		if x.Key == capabilityProbeKey {
			x := x
			e = &x
		}
	}
	if e == nil {
		t.Fatalf("Z1: %s is not a registry key", capabilityProbeKey)
	}
	if e.typ != typDuration || e.Mut != "hot" || e.Tenancy != "global-only" || e.EnvVar != "CTX_POOL_CAPABILITY_PROBE_INTERVAL" {
		t.Errorf("Z1: %s type %v, mutability %q, tenancy %q, env %q — want seconds, hot, global-only, CTX_POOL_CAPABILITY_PROBE_INTERVAL",
			capabilityProbeKey, e.typ, e.Mut, e.Tenancy, e.EnvVar)
	}
	if e.defVal != 600*time.Second {
		t.Errorf("Z1: %s default %v, want 600s", capabilityProbeKey, e.defVal)
	}
	c, _ := cfgFrom(t, map[string]string{})
	if c.Pool.CapabilityProbeInterval != 600*time.Second {
		t.Errorf("Z1: %s loads as %v, want 600s", capabilityProbeKey, c.Pool.CapabilityProbeInterval)
	}
	if desc := keyDescriptions[capabilityProbeKey]; desc == "" {
		t.Errorf("Z1: %s has no description", capabilityProbeKey)
	}
}

// TestValidateCapabilityProbeIntervalV59: 0 is one error on the key naming
// V59 and the key; a negative value is V17's error alone; 1 s and the
// default are legal.
func TestValidateCapabilityProbeIntervalV59(t *testing.T) {
	errorsOn := func(raw string) []Issue {
		t.Helper()
		c, issues := cfgFrom(t, map[string]string{capabilityProbeKey: raw})
		issues = append(issues, Validate(c)...)
		var errs []Issue
		for _, is := range issues {
			if is.Severity != SeverityError {
				continue
			}
			if is.Field != capabilityProbeKey {
				t.Errorf("%s=%s: unexpected error on %s: %s", capabilityProbeKey, raw, is.Field, is.Msg)
				continue
			}
			errs = append(errs, is)
		}
		return errs
	}
	if errs := errorsOn("0"); len(errs) != 1 || !strings.Contains(errs[0].Msg, "V59") || !strings.Contains(errs[0].Msg, capabilityProbeKey) {
		t.Errorf("Z1 V59: %s=0: errors %+v, want exactly one naming V59 and the key", capabilityProbeKey, errs)
	}
	if errs := errorsOn("-1"); len(errs) != 1 || strings.Contains(errs[0].Msg, "V59") {
		t.Errorf("Z1 V59: %s=-1: errors %+v, want exactly one — V17's sign error, no second V59 report", capabilityProbeKey, errs)
	}
	for _, raw := range []string{"1", "600", "86400"} {
		if errs := errorsOn(raw); len(errs) != 0 {
			t.Errorf("Z1 V59: %s=%s is legal, got errors %+v", capabilityProbeKey, raw, errs)
		}
	}
	c, _ := cfgFrom(t, map[string]string{})
	c.Pool.CapabilityProbeInterval = 500 * time.Millisecond
	found := false
	for _, is := range Validate(c) {
		if is.Field == capabilityProbeKey && is.Severity == SeverityError && strings.Contains(is.Msg, "V59") {
			found = true
		}
	}
	if !found {
		t.Errorf("Z1 V59: a sub-second interval (hand-built snapshot) passed, want the V59 floor of one second")
	}
}
