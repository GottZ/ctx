package main

// The day and month budgets through the production adapter (W06-13): the
// meter's budget refusal is 429 insufficient_quota, not retryable, with the
// Retry-After until the window resets; the envelopes are the factors × the
// consumer default; the keys are hot.

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// TestMeterLimiterMapsTheBudgets is gate (a) through the adapter:
// chat_daily_tokens 1000, 800 real tokens, then a reservation of 400 ⇒ 429
// insufficient_quota, x-should-retry false, Retry-After = the time until
// 00:00 UTC (fake clock); a rotated key of the same consumer finds the same
// budget (gate (e), B-06-19); the tenant envelope is the factor × the
// consumer default (gate (f)); a hot change acts on the next admission.
func TestMeterLimiterMapsTheBudgets(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{
		"CTX_GATEWAY_CHAT_DAILY_TOKENS_DEFAULT": "1000", "CTX_GATEWAY_TENANT_ENVELOPE_FACTOR": "2",
	})
	now := time.Date(2026, 11, 7, 18, 30, 0, 0, time.UTC)
	meter := keyquota.New(func() time.Time { return now })
	meter.MarkSeeded()
	l := meterLimiter{meter: meter, settings: gatewaySettingsGetter(cfgStore)}
	ctx := context.Background()
	est := func(n int) oai.Estimate {
		return oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: n, ClientMaxTokens: true}
	}
	key := func(consumer, tenant, modelKey string) *auth.ConsumerAuth {
		return &auth.ConsumerAuth{IsValid: true, ModelKeyID: modelKey, ConsumerID: consumer, TenantID: tenant, Kind: auth.KindApp, AdmissionLane: handler.LaneUrgent}
	}
	res, ref := l.Admit(ctx, key("c1", "t1", "k-old"), modelalias.Alias{}, est(10))
	if ref != nil {
		t.Fatalf("first: %+v", ref)
	}
	res.Settle(dispatch.Usage{PromptTokens: 500, CompletionTokens: 300})
	_, ref = l.Admit(ctx, key("c1", "t1", "k-new"), modelalias.Alias{}, est(400))
	want := 5*time.Hour + 30*time.Minute
	if ref == nil || ref.Code != oai.CodeInsufficientQuota || ref.ShouldRetry || ref.RetryAfter != want ||
		ref.Message != "Daily token quota reached (chat): 1000 tokens per UTC day." {
		t.Fatalf("800 used + 400 over a rotated key: %+v, want 429 insufficient_quota, not retryable, Retry-After %v", ref, want)
	}

	// Tenant envelope 2 × 1000: two consumers of t2 use it up, a third
	// consumer of t2 is refused although its own budget is untouched.
	for _, c := range []string{"c2", "c3"} {
		res, ref := l.Admit(ctx, key(c, "t2", "k-"+c), modelalias.Alias{}, est(10))
		if ref != nil {
			t.Fatalf("%s: %+v", c, ref)
		}
		res.Settle(dispatch.Usage{CompletionTokens: 1000})
	}
	_, ref = l.Admit(ctx, key("c4", "t2", "k-c4"), modelalias.Alias{}, est(10))
	if ref == nil || ref.Code != oai.CodeInsufficientQuota || ref.ShouldRetry || ref.Message != "Daily token quota of your tenant reached (chat)." {
		t.Fatalf("third consumer of a full tenant envelope: %+v", ref)
	}

	next := *cfgStore.Snapshot()
	next.Gateway.ChatDailyTokensDefault = 5000
	if err := cfgStore.Replace(&next); err != nil {
		t.Fatal(err)
	}
	if _, ref := l.Admit(ctx, key("c1", "t1", "k-new"), modelalias.Alias{}, est(400)); ref != nil {
		t.Errorf("after the hot change to 5000: %+v, want admitted", ref)
	}
}
