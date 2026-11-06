package main

// L1 through the production adapter (W06-11): meterLimiter hands
// gateway.consumer_concurrency_default to the meter per request (hot) and maps
// its refusal onto 429 concurrency_limit_exceeded with Retry-After 1 s; with
// no limits rows (W06-15) the registry default 2 holds for every consumer.

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// TestMeterLimiterMapsL1: under the registry defaults (C = 2, r = 0.25) a
// consumer's third open chat request is refused — 429
// concurrency_limit_exceeded, retryable, Retry-After 1 s, the message naming
// 2 — and an ended one frees its slot; service_tier flex holds the bulk
// lane's one slot, the urgent lane keeps the other, and an open urgent
// request leaves the bulk slot free; a hot change of the key acts on the next
// admission.
func TestMeterLimiterMapsL1(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{})
	gw := meterTestGateway(t, cfgStore)
	l := gatewayDeps(context.Background(), nil, cfgStore, nil, nil, nil, nil, gw.meter).Limiter
	ctx := context.Background()
	est := func(tier string) oai.Estimate {
		return oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 1, ClientMaxTokens: true, PromptRunes: 9, ServiceTier: tier}
	}
	refusedL1 := func(what string, ref *oai.LimitRefusal, message string) {
		t.Helper()
		if ref == nil || ref.Code != oai.CodeConcurrencyLimitExceeded || !ref.ShouldRetry || ref.RetryAfter != time.Second || ref.Message != message {
			t.Errorf("%s: %+v, want 429 concurrency_limit_exceeded, retryable, Retry-After 1s, %q", what, ref, message)
		}
	}

	ca := urgentConsumer("l1")
	var open []oai.LimitReservation
	for i := range 2 {
		res, ref := l.Admit(ctx, ca, modelalias.Alias{}, est(""))
		if ref != nil {
			t.Fatalf("request %d of the default 2 refused: %+v", i+1, ref)
		}
		open = append(open, res)
	}
	_, ref := l.Admit(ctx, ca, modelalias.Alias{}, est(""))
	refusedL1("third open request", ref, "Concurrency limit reached for requests (chat): 2 at a time.")
	open[0].Settle(dispatch.Usage{PromptTokens: 1})
	if _, ref := l.Admit(ctx, ca, modelalias.Alias{}, est("")); ref != nil {
		t.Errorf("after one ended: %+v, want admitted", ref)
	}

	lane := urgentConsumer("l1-lane")
	if _, ref := l.Admit(ctx, lane, modelalias.Alias{}, est("flex")); ref != nil {
		t.Fatalf("first flex: %+v", ref)
	}
	_, ref = l.Admit(ctx, lane, modelalias.Alias{}, est("flex"))
	refusedL1("second flex", ref, "Concurrency limit reached for requests (chat, bulk lane): 1 at a time.")
	if _, ref := l.Admit(ctx, lane, modelalias.Alias{}, est("")); ref != nil {
		t.Errorf("urgent next to one flex: %+v, want admitted (the urgent lane keeps its slot)", ref)
	}

	// The bulk share counts the bulk lane's own requests: next to an open
	// urgent request a flex request still gets the bulk slot.
	mixed := urgentConsumer("l1-mixed")
	if _, ref := l.Admit(ctx, mixed, modelalias.Alias{}, est("")); ref != nil {
		t.Fatalf("urgent first: %+v", ref)
	}
	if _, ref := l.Admit(ctx, mixed, modelalias.Alias{}, est("flex")); ref != nil {
		t.Errorf("flex next to one open urgent request: %+v, want admitted (the bulk lane holds none yet)", ref)
	}

	next := *cfgStore.Snapshot()
	next.Gateway.ConsumerConcurrencyDefault = 3
	if err := cfgStore.Replace(&next); err != nil {
		t.Fatal(err)
	}
	if _, ref := l.Admit(ctx, lane, modelalias.Alias{}, est("")); ref != nil {
		t.Errorf("after the hot change to 3: %+v, want a third slot", ref)
	}
}
