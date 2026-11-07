package config

// The day and month budgets of the meter (wave W06-13, design/06 §4.11):
// gateway.{chat,embed,rerank}_daily_tokens_default, consumer_monthly_tokens_default,
// consumer_daily_requests_default and the two envelope factors — registry
// keys, parse-strict, hot, global-only, in the boot record, and V50: the
// daily token defaults at least 1, the month and request budgets at least 0
// (0 = unbounded), the factors at least 1.

import (
	"strings"
	"testing"
)

var budgetKeys = map[string]any{
	"gateway.chat_daily_tokens_default":       2000000,
	"gateway.embed_daily_tokens_default":      20000000,
	"gateway.rerank_daily_tokens_default":     5000000,
	"gateway.consumer_monthly_tokens_default": 0,
	"gateway.consumer_daily_requests_default": 0,
	"gateway.tenant_envelope_factor":          5.0,
	"gateway.instance_envelope_factor":        10.0,
}

// TestBudgetKeysAreStrictHotGlobalKeys: each key is a registry key,
// parse-strict, hot, global-only, env CTX_GATEWAY_<KEY>, with its design
// default in the boot record.
func TestBudgetKeysAreStrictHotGlobalKeys(t *testing.T) {
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
	for key, def := range budgetKeys {
		e, ok := byKey[key]
		if !ok {
			t.Errorf("%s: not a registry key", key)
			continue
		}
		env := "CTX_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
		if e.Mut != "hot" || e.Tenancy != "global-only" || !e.Strict || e.EnvVar != env {
			t.Errorf("%s: mutability %q, tenancy %q, strict %v, env %q — want hot, global-only, strict, %s", key, e.Mut, e.Tenancy, e.Strict, e.EnvVar, env)
		}
		if got, ok := group[strings.TrimPrefix(key, "gateway.")]; !ok || got != def {
			t.Errorf("%s in the boot record = %v (%T, present %v), want %v", key, got, got, ok, def)
		}
	}
}

// TestValidateBudgetsV50 is V50: a daily token default of 0 and a factor
// below 1 are one error on their key; 0 for the month and request budgets is
// legal (unbounded); negative ints stay V35's one error.
func TestValidateBudgetsV50(t *testing.T) {
	for _, tc := range []struct {
		key, val string
		errs     int
		mentions string
	}{
		{"gateway.chat_daily_tokens_default", "0", 1, ">= 1"},
		{"gateway.embed_daily_tokens_default", "0", 1, ">= 1"},
		{"gateway.rerank_daily_tokens_default", "0", 1, ">= 1"},
		{"gateway.chat_daily_tokens_default", "1", 0, ""},
		{"gateway.rerank_daily_tokens_default", "-1", 1, ""},
		{"gateway.consumer_monthly_tokens_default", "0", 0, ""},
		{"gateway.consumer_monthly_tokens_default", "100", 0, ""},
		{"gateway.consumer_daily_requests_default", "0", 0, ""},
		{"gateway.consumer_daily_requests_default", "-1", 1, ""},
		{"gateway.tenant_envelope_factor", "1", 0, ""},
		{"gateway.tenant_envelope_factor", "0.99", 1, ">= 1"},
		{"gateway.instance_envelope_factor", "0", 1, ">= 1"},
		{"gateway.instance_envelope_factor", "2.5", 0, ""},
	} {
		c, issues := cfgFrom(t, map[string]string{tc.key: tc.val})
		issues = append(issues, Validate(c)...)
		var errs []Issue
		for _, is := range issues {
			if is.Severity != SeverityError {
				continue
			}
			if is.Field != tc.key {
				t.Errorf("%s=%s: unexpected error on %s: %s", tc.key, tc.val, is.Field, is.Msg)
				continue
			}
			errs = append(errs, is)
		}
		if len(errs) != tc.errs || (tc.mentions != "" && len(errs) == 1 && !strings.Contains(errs[0].Msg, tc.mentions)) {
			t.Errorf("%s=%s: errors %+v, want %d error(s) mentioning %q", tc.key, tc.val, errs, tc.errs, tc.mentions)
		}
	}
}

// TestBudgetKeysReachTheMeterSettings: the keys land in oai.MeterSettings —
// the daily token default per op, the month and request budgets and both
// factors.
func TestBudgetKeysReachTheMeterSettings(t *testing.T) {
	c, _ := cfgFrom(t, map[string]string{
		"gateway.chat_daily_tokens_default": "11", "gateway.embed_daily_tokens_default": "12", "gateway.rerank_daily_tokens_default": "13",
		"gateway.consumer_monthly_tokens_default": "14", "gateway.consumer_daily_requests_default": "15",
		"gateway.tenant_envelope_factor": "1.5", "gateway.instance_envelope_factor": "2.5",
	})
	m := c.GatewaySettings().Meter
	if m.Chat.DailyTokens != 11 || m.Embed.DailyTokens != 12 || m.Rerank.DailyTokens != 13 || m.MonthlyTokens != 14 || m.DailyRequests != 15 ||
		m.TenantEnvelopeFactor != 1.5 || m.InstanceEnvelopeFactor != 2.5 {
		t.Errorf("meter settings %+v", m)
	}
}
