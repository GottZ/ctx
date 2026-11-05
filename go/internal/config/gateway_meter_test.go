package config

import (
	"strings"
	"testing"
)

// The keys of the per-consumer meter (W06-10, design/06 §4.11) with their
// registry defaults as the boot record renders them. The tests address
// them by name only — the same form a settings write and the environment
// use.
var meterKeyDefaults = map[string]any{
	"chat_rpm_default":    30,
	"chat_tpm_default":    60000,
	"embed_rpm_default":   120,
	"embed_tpm_default":   500000,
	"rerank_rpm_default":  60,
	"rerank_tpm_default":  200000,
	"tpm_reserve_default": 1024,
	"xu_reserve_ratio":    0.25,
}

// TestBootDumpShowsTheMeter: the boot record "config: effective" carries the
// eight meter keys with their defaults (design/06 §4.11, briefing W06-10
// gate "Config").
func TestBootDumpShowsTheMeter(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{})
	args := BootDumpArgs(c, issues)
	var group map[string]any
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "gateway" {
			group, _ = args[i+1].(map[string]any)
		}
	}
	if group == nil {
		t.Fatalf("config: effective lacks the gateway group: %v", args)
	}
	for k, want := range meterKeyDefaults {
		if got, ok := group[k]; !ok || got != want {
			t.Errorf("gateway.%s in the boot record = %v (%T, present %v), want %v (%T)", k, got, got, ok, want, want)
		}
	}
}

// TestMeterKeysAreStrictHotGlobalOnly: each meter key is a registry key,
// parse-strict (a typo'd limit aborts the boot instead of falling back),
// hot and global-only.
func TestMeterKeysAreStrictHotGlobalOnly(t *testing.T) {
	byKey := map[string]entry{}
	for _, e := range registry() {
		byKey[e.Key] = e
	}
	for k := range meterKeyDefaults {
		key := "gateway." + k
		e, ok := byKey[key]
		if !ok {
			t.Errorf("%s: not a registry key", key)
			continue
		}
		if e.Mut != "hot" || e.Tenancy != "global-only" || !e.Strict {
			t.Errorf("%s: mutability %q, tenancy %q, strict %v — want hot, global-only, strict", key, e.Mut, e.Tenancy, e.Strict)
		}
		if want := "CTX_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_")); e.EnvVar != want {
			t.Errorf("%s: env %q, want %q", key, e.EnvVar, want)
		}
	}
}

// TestValidateMeterV47 is V47: every meter int at 0 and the ratio outside
// [0, 1) — 1, a negative value, NaN — is one error on its own field; the
// edges 1 and 0 / 0.999 are legal.
func TestValidateMeterV47(t *testing.T) {
	errorOn := func(vals map[string]string, field string) bool {
		c, issues := cfgFrom(t, vals)
		issues = append(issues, Validate(c)...)
		n := 0
		for _, is := range issues {
			if is.Severity == SeverityError {
				if is.Field != field {
					t.Errorf("%v: unexpected error on %s: %s", vals, is.Field, is.Msg)
					continue
				}
				n++
			}
		}
		return n == 1
	}
	for k := range meterKeyDefaults {
		key := "gateway." + k
		if k == "xu_reserve_ratio" {
			continue
		}
		if !errorOn(map[string]string{key: "0"}, key) {
			t.Errorf("%s = 0: want exactly one error on the field (V47)", key)
		}
		if errorOn(map[string]string{key: "1"}, key) {
			t.Errorf("%s = 1: refused, want legal", key)
		}
	}
	for _, v := range []string{"1", "1.5", "-0.01", "NaN"} {
		if !errorOn(map[string]string{"gateway.xu_reserve_ratio": v}, "gateway.xu_reserve_ratio") {
			t.Errorf("gateway.xu_reserve_ratio = %s: want exactly one error (V47, range [0, 1))", v)
		}
	}
	for _, v := range []string{"0", "0.999"} {
		if errorOn(map[string]string{"gateway.xu_reserve_ratio": v}, "gateway.xu_reserve_ratio") {
			t.Errorf("gateway.xu_reserve_ratio = %s: refused, want legal", v)
		}
	}
}
