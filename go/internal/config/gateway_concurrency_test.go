package config

// gateway.consumer_concurrency_default, the meter's L1 (wave W06-11,
// design/06 §4.2, §4.11): a registry key with default 2, parse-strict, hot,
// global-only, in the boot record, and V48 — at least 1 and, while
// dispatch.external_queue_per_consumer caps the queue, at most that cap. The
// tests address the key by name, the form a settings write and the
// environment use.

import (
	"strings"
	"testing"
)

const concurrencyKey = "gateway.consumer_concurrency_default"

// TestConcurrencyKeyIsAStrictHotGlobalKey: a registry key, default 2 (never
// unbounded without a limits row), parse-strict, hot, global-only, env
// CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT, and in the boot record.
func TestConcurrencyKeyIsAStrictHotGlobalKey(t *testing.T) {
	var e *entry
	for _, x := range registry() {
		if x.Key == concurrencyKey {
			e = &x
		}
	}
	if e == nil {
		t.Fatalf("%s: not a registry key", concurrencyKey)
	}
	if e.Mut != "hot" || e.Tenancy != "global-only" || !e.Strict || e.EnvVar != "CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT" {
		t.Errorf("%s: mutability %q, tenancy %q, strict %v, env %q — want hot, global-only, strict, CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT",
			concurrencyKey, e.Mut, e.Tenancy, e.Strict, e.EnvVar)
	}
	c, issues := cfgFrom(t, map[string]string{})
	args := BootDumpArgs(c, issues)
	var group map[string]any
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "gateway" {
			group, _ = args[i+1].(map[string]any)
		}
	}
	if got, ok := group["consumer_concurrency_default"]; !ok || got != 2 {
		t.Errorf("%s in the boot record = %v (present %v), want the default 2", concurrencyKey, got, ok)
	}
}

// TestValidateConcurrencyV48 is V48: 0 is one error on the key; a negative
// value is V35's one error; 1 and the queue cap are legal, one above the cap
// is one error naming both keys; with the per-consumer queue cap off (0)
// any C >= 1 is legal.
func TestValidateConcurrencyV48(t *testing.T) {
	errorsOn := func(vals map[string]string) []Issue {
		t.Helper()
		c, issues := cfgFrom(t, vals)
		issues = append(issues, Validate(c)...)
		var errs []Issue
		for _, is := range issues {
			if is.Severity != SeverityError {
				continue
			}
			if is.Field != concurrencyKey {
				t.Errorf("%v: unexpected error on %s: %s", vals, is.Field, is.Msg)
				continue
			}
			errs = append(errs, is)
		}
		return errs
	}
	for _, tc := range []struct {
		conc, queue string
		errs        int
		mentions    string
	}{
		{"0", "", 1, ">= 1"},
		{"-1", "", 1, ""},
		{"1", "", 0, ""},
		{"2", "", 0, ""},
		{"8", "", 0, ""},
		{"9", "", 1, "dispatch.external_queue_per_consumer (8)"},
		{"5", "4", 1, "dispatch.external_queue_per_consumer (4)"},
		{"4", "4", 0, ""},
		{"1000", "0", 0, ""},
		{"0", "0", 1, ">= 1"},
	} {
		vals := map[string]string{concurrencyKey: tc.conc}
		if tc.queue != "" {
			vals["dispatch.external_queue_per_consumer"] = tc.queue
		}
		errs := errorsOn(vals)
		if len(errs) != tc.errs || (tc.mentions != "" && len(errs) == 1 && !strings.Contains(errs[0].Msg, tc.mentions)) {
			t.Errorf("concurrency %s, queue cap %q: errors %+v, want %d error(s) mentioning %q", tc.conc, tc.queue, errs, tc.errs, tc.mentions)
		}
	}
}
