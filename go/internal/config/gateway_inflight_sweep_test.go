package config

// gateway.inflight_sweep_interval, the period of the in-flight registry's
// periodic sweep at the model-key door (wave W01-8): a registry key with
// default 30 s, parse-strict, hot, global-only, env
// CTX_GATEWAY_INFLIGHT_SWEEP_INTERVAL, in the boot record, and V49 — at least
// one second. The tests address the key by name, the form a settings write
// and the environment use.

import (
	"strings"
	"testing"
	"time"
)

const inflightSweepKey = "gateway.inflight_sweep_interval"

// inflightSweepEntry is the registry entry of the key, or nil.
func inflightSweepEntry() *entry {
	for _, e := range registry() {
		if e.Key == inflightSweepKey {
			return &e
		}
	}
	return nil
}

// gatewayDumpGroup is the gateway group of the boot record "config:
// effective", or nil.
func gatewayDumpGroup(c *Config, issues []Issue) map[string]any {
	args := BootDumpArgs(c, issues)
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "gateway" {
			group, _ := args[i+1].(map[string]any)
			return group
		}
	}
	return nil
}

// TestInflightSweepKeyIsAStrictHotGlobalDuration: a seconds-typed registry
// key, default 30 s, parse-strict, hot, global-only, env
// CTX_GATEWAY_INFLIGHT_SWEEP_INTERVAL, and in the boot record with its
// default.
func TestInflightSweepKeyIsAStrictHotGlobalDuration(t *testing.T) {
	e := inflightSweepEntry()
	if e == nil {
		t.Fatalf("%s: not a registry key", inflightSweepKey)
	}
	if e.typ != typDuration || e.Mut != "hot" || e.Tenancy != "global-only" || !e.Strict || e.EnvVar != "CTX_GATEWAY_INFLIGHT_SWEEP_INTERVAL" {
		t.Errorf("%s: type %v, mutability %q, tenancy %q, strict %v, env %q — want seconds, hot, global-only, strict, CTX_GATEWAY_INFLIGHT_SWEEP_INTERVAL",
			inflightSweepKey, e.typ, e.Mut, e.Tenancy, e.Strict, e.EnvVar)
	}
	if e.defVal != 30*time.Second {
		t.Errorf("%s: default %v, want 30s", inflightSweepKey, e.defVal)
	}
	c, issues := cfgFrom(t, map[string]string{})
	if got, ok := gatewayDumpGroup(c, issues)["inflight_sweep_interval"]; !ok || got != "30s" {
		t.Errorf("%s in the boot record = %v (present %v), want the default 30s", inflightSweepKey, got, ok)
	}
}

// TestInflightSweepKeyFromEnv: CTX_GATEWAY_INFLIGHT_SWEEP_INTERVAL reaches the
// key (source env) and the boot record; a malformed value is an error, not a
// silent fallback to the default (parse-strict).
func TestInflightSweepKeyFromEnv(t *testing.T) {
	resetBuildEnv(t)
	t.Setenv("CTX_GATEWAY_INFLIGHT_SWEEP_INTERVAL", "5")
	c, issues := Build(nil, nil)
	if HasErrors(issues) {
		t.Fatalf("env 5: build errors %+v", issues)
	}
	if src := c.Source(inflightSweepKey); src != "env" {
		t.Errorf("%s source = %q, want env", inflightSweepKey, src)
	}
	if got := gatewayDumpGroup(c, issues)["inflight_sweep_interval"]; got != "5s" {
		t.Errorf("%s in the boot record = %v, want 5s from the environment", inflightSweepKey, got)
	}

	_, issues = cfgFrom(t, map[string]string{inflightSweepKey: "30s"})
	if got := severityFor(issues, inflightSweepKey); got != SeverityError {
		t.Errorf("%s = \"30s\" (not bare seconds): severity %v, want SeverityError (parse-strict): %+v", inflightSweepKey, got, issues)
	}
}

// TestValidateInflightSweepV49 is V49: 0 is one error on the key that names
// V49 and the key; a negative value is V17's one error (the sign stays with
// the generic walk, TestValidateRejectsNegativeOnEveryDurationKey); 1 s and
// the default are legal.
func TestValidateInflightSweepV49(t *testing.T) {
	errorsOn := func(raw string) []Issue {
		t.Helper()
		c, issues := cfgFrom(t, map[string]string{inflightSweepKey: raw})
		issues = append(issues, Validate(c)...)
		var errs []Issue
		for _, is := range issues {
			if is.Severity != SeverityError {
				continue
			}
			if is.Field != inflightSweepKey {
				t.Errorf("%s=%s: unexpected error on %s: %s", inflightSweepKey, raw, is.Field, is.Msg)
				continue
			}
			errs = append(errs, is)
		}
		return errs
	}
	if errs := errorsOn("0"); len(errs) != 1 || !strings.Contains(errs[0].Msg, "V49") || !strings.Contains(errs[0].Msg, inflightSweepKey) {
		t.Errorf("%s=0: errors %+v, want exactly one naming V49 and the key", inflightSweepKey, errs)
	}
	if errs := errorsOn("-1"); len(errs) != 1 || strings.Contains(errs[0].Msg, "V49") {
		t.Errorf("%s=-1: errors %+v, want exactly one — V17's sign error, no second V49 report on the field", inflightSweepKey, errs)
	}
	for _, raw := range []string{"1", "30", "3600"} {
		if errs := errorsOn(raw); len(errs) != 0 {
			t.Errorf("%s=%s is legal, got errors %+v", inflightSweepKey, raw, errs)
		}
	}
	// The floor is one second, not "not zero": the seconds parser cannot
	// produce a fraction, a hand-built snapshot can.
	c := Defaults()
	c.Gateway.InflightSweepInterval = time.Second - time.Nanosecond
	if hits := issuesOn(Validate(c), inflightSweepKey); len(hits) != 1 || hits[0].Severity != SeverityError || !strings.Contains(hits[0].Msg, "V49") {
		t.Errorf("%s = 999.999999ms: %+v, want exactly one V49 error", inflightSweepKey, hits)
	}
	c.Gateway.InflightSweepInterval = time.Second
	if hits := issuesOn(Validate(c), inflightSweepKey); len(hits) != 0 {
		t.Errorf("%s = 1s is legal, got %+v", inflightSweepKey, hits)
	}
}

// TestInflightSweepReachesAuthSettings: the key is the door's
// Settings().Auth.InflightSweepInterval — the default 30 s in the snapshot,
// a settings value replaces it.
func TestInflightSweepReachesAuthSettings(t *testing.T) {
	if got := Defaults().GatewaySettings().Auth.InflightSweepInterval; got != 30*time.Second {
		t.Errorf("default Settings().Auth.InflightSweepInterval = %v, want 30s", got)
	}
	c := validCfg(t, map[string]string{inflightSweepKey: "7"})
	if got := c.GatewaySettings().Auth.InflightSweepInterval; got != 7*time.Second {
		t.Errorf("%s=7: Settings().Auth.InflightSweepInterval = %v, want 7s", inflightSweepKey, got)
	}
}
