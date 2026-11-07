package main

// The production binding of the per-consumer meter (W06-10): the process's
// model gateway owns one seeded meter, gatewayDeps wires it as the chat
// route's Limiter (a nil meter leaves the Limiter nil — the fail-closed
// case), and meterLimiter maps the route's estimate, the lane and the
// meter's decision onto the oai contract.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

func meterTestStore(t *testing.T, env map[string]string) *config.Store {
	t.Helper()
	applyEnv(t, env)
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env: %v", issues)
	}
	return config.NewStore(cc)
}

// meterTestGateway is the process's model gateway over a pool that never
// connects (the meter needs no database; the door's constructor wants a
// pool). Its boot seed read fails, so the meter is closed (warming, W06-13);
// the helper opens it as a meter that has been running for a while
// (MarkSeeded: no rollup figures, no burst damping) — the minute buckets and
// L1 under test need no history.
func meterTestGateway(t *testing.T, cfgStore *config.Store) *modelGateway {
	t.Helper()
	gw := meterTestGatewayUnseeded(t, cfgStore)
	gw.meter.MarkSeeded()
	return gw
}

// meterTestGatewayUnseeded is meterTestGateway without opening the meter.
func meterTestGatewayUnseeded(t *testing.T, cfgStore *config.Store) *modelGateway {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://ctx:ctx@127.0.0.1:1/ctx?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return newModelGateway(pool, cfgStore)
}

// TestGatewayDepsWiresTheMeter is the wiring pin of gate Z1: the model
// gateway holds one meter, its seed read wired, and gatewayDeps hands it to
// the chat route as its Limiter; without a meter the Limiter stays nil (L11
// fail-closed). A boot seed read that fails leaves the meter closed
// (warming, W06-13 (h)) — fail-closed, never open without the rollup.
func TestGatewayDepsWiresTheMeter(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{})
	gw := meterTestGatewayUnseeded(t, cfgStore)
	if gw.meter == nil || gw.meter.Seeded() || gw.seedRead == nil {
		t.Fatalf("model gateway meter %v (seeded %v, seed read %v), want one closed meter with its seed read after a failed seed",
			gw.meter, gw.meter != nil && gw.meter.Seeded(), gw.seedRead != nil)
	}
	if d := gatewayDeps(context.Background(), nil, cfgStore, nil, nil, nil, nil, gw.meter); d.Limiter == nil {
		t.Error("gatewayDeps with the process meter: Limiter nil, want the meter")
	}
	if d := gatewayDeps(context.Background(), nil, cfgStore, nil, nil, nil, nil, nil); d.Limiter != nil {
		t.Errorf("gatewayDeps without a meter: Limiter %T, want nil (no typed nil behind the interface)", d.Limiter)
	}
}

func urgentConsumer(id string) *auth.ConsumerAuth {
	return &auth.ConsumerAuth{IsValid: true, ConsumerID: id, TenantID: "t1", Kind: auth.KindApp, AdmissionLane: handler.LaneUrgent}
}

// TestMeterLimiterMapsTheContract: the limits come from the settings
// snapshot per op, service_tier flex lowers the lane to bulk (one lane rule
// with the planner), the refusal is 429 rate_limit_exceeded with the exact
// Retry-After, the reservation settles dispatch.Usage and carries the
// x-ratelimit-* headers.
func TestMeterLimiterMapsTheContract(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CHAT_RPM_DEFAULT": "4", "CTX_GATEWAY_CHAT_TPM_DEFAULT": "60000"})
	gw := meterTestGateway(t, cfgStore)
	l := gatewayDeps(context.Background(), nil, cfgStore, nil, nil, nil, nil, gw.meter).Limiter
	ctx := context.Background()
	est := func(tier string) oai.Estimate {
		return oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 100, ClientMaxTokens: true, PromptRunes: 9, ServiceTier: tier}
	}

	// Bulk via service_tier flex: three of four (r = 0.25), headers in the
	// bulk lane's view.
	ca := urgentConsumer("flex")
	for i := range 3 {
		res, ref := l.Admit(ctx, ca, modelalias.Alias{}, est("flex"))
		if ref != nil {
			t.Fatalf("flex %d refused: %+v", i+1, ref)
		}
		if got := res.Headers().Get("x-ratelimit-limit-requests"); got != "3" {
			t.Errorf("flex %d: x-ratelimit-limit-requests %q, want 3 (the bulk lane's share of 4)", i+1, got)
		}
		res.Settle(dispatch.Usage{PromptTokens: 1})
	}
	_, ref := l.Admit(ctx, ca, modelalias.Alias{}, est("flex"))
	// The real clock moved a little since the first admission: the wait is
	// the emission interval 15 s minus that.
	if ref == nil || ref.Code != oai.CodeRateLimitExceeded || !ref.ShouldRetry || ref.RetryAfter > 15*time.Second || ref.RetryAfter < 14*time.Second {
		t.Fatalf("flex 4: %+v, want 429 rate_limit_exceeded, retryable, Retry-After just under 15s", ref)
	}
	// The same consumer on its urgent lane passes.
	res, ref := l.Admit(ctx, ca, modelalias.Alias{}, est(""))
	if ref != nil {
		t.Fatalf("urgent after three bulk: %+v", ref)
	}
	if got := res.Headers().Get("x-ratelimit-limit-requests"); got != "4" {
		t.Errorf("urgent: x-ratelimit-limit-requests %q, want 4", got)
	}

	// Settle books dispatch.Usage: 5000 real tokens leave 55000 minus the
	// next reservation.
	ca2 := urgentConsumer("settle")
	res, _ = l.Admit(ctx, ca2, modelalias.Alias{}, est(""))
	res.Settle(dispatch.Usage{PromptTokens: 4000, CompletionTokens: 1000})
	res, _ = l.Admit(ctx, ca2, modelalias.Alias{}, oai.Estimate{Op: llmlog.GatewayOpChat, ClientMaxTokens: true})
	if got, _ := strconv.Atoi(res.Headers().Get("x-ratelimit-remaining-tokens")); got > 55000 || got < 54990 {
		t.Errorf("remaining tokens %d after settling 5000, want ≈55000", got)
	}

	// A bulk-ceiling consumer is bulk whatever the tier says.
	bulk := urgentConsumer("bulk")
	bulk.AdmissionLane = handler.LaneBulk
	res, _ = l.Admit(ctx, bulk, modelalias.Alias{}, est("priority"))
	if got := res.Headers().Get("x-ratelimit-limit-requests"); got != "3" {
		t.Errorf("bulk consumer: x-ratelimit-limit-requests %q, want 3", got)
	}
}

// TestMeterLimiterFailsClosed: a consumer without an id or tenant, an
// unknown op — a wiring defect — is 503 internal_error, not retryable; an
// unseeded meter is the warming 429 (Retry-After 5 s, retryable).
func TestMeterLimiterFailsClosed(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{})
	gw := meterTestGateway(t, cfgStore)
	l := gatewayDeps(context.Background(), nil, cfgStore, nil, nil, nil, nil, gw.meter).Limiter
	ctx := context.Background()
	for name, tc := range map[string]struct {
		ca *auth.ConsumerAuth
		op string
	}{
		"no consumer id": {&auth.ConsumerAuth{IsValid: true, TenantID: "t1", AdmissionLane: handler.LaneUrgent}, llmlog.GatewayOpChat},
		"no tenant":      {&auth.ConsumerAuth{IsValid: true, ConsumerID: "c", AdmissionLane: handler.LaneUrgent}, llmlog.GatewayOpChat},
		"nil consumer":   {nil, llmlog.GatewayOpChat},
		"unknown op":     {urgentConsumer("c"), "images"},
	} {
		res, ref := l.Admit(ctx, tc.ca, modelalias.Alias{}, oai.Estimate{Op: tc.op})
		if res != nil || ref == nil || ref.Code != oai.CodeInternalError || ref.ShouldRetry {
			t.Errorf("%s: %v %+v, want 503 internal_error, not retryable", name, res, ref)
		}
	}
	cold := meterLimiter{meter: keyquota.New(time.Now), settings: gatewaySettingsGetter(cfgStore)}
	res, ref := cold.Admit(ctx, urgentConsumer("c"), modelalias.Alias{}, oai.Estimate{Op: llmlog.GatewayOpChat})
	if res != nil || ref == nil || ref.Code != oai.CodeRateLimitExceeded || !ref.ShouldRetry || ref.RetryAfter != 5*time.Second {
		t.Errorf("unseeded: %v %+v, want 429 rate_limit_exceeded, retryable, Retry-After 5s", res, ref)
	}
}

// TestMeterLimiterChargesTheClientMaximum is H-5 (review 2 R12): the adapter
// hands the route's effective max_tokens to the meter. A request with a
// client maximum of 5 000 (above the reserve 1 024) reserves its prompt
// estimate (5 tokens for 9 runes) plus the 5 000 — the oai route gates run
// over a stand-in and do not see this mapping.
func TestMeterLimiterChargesTheClientMaximum(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CHAT_TPM_DEFAULT": "60000"})
	gw := meterTestGateway(t, cfgStore)
	l := gatewayDeps(context.Background(), nil, cfgStore, nil, nil, nil, nil, gw.meter).Limiter
	res, ref := l.Admit(context.Background(), urgentConsumer("max"), modelalias.Alias{},
		oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 5000, ClientMaxTokens: true, PromptRunes: 9})
	if ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	if got := res.Headers().Get("x-ratelimit-remaining-tokens"); got != "54995" {
		t.Errorf("x-ratelimit-remaining-tokens %q, want 54995 (60000 − 5 prompt − 5000 client maximum)", got)
	}
}
