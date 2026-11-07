package main

// The consumer's egress policy through the production adapter (wave
// W06-14): meterLimiter answers oai.EgressSource from the settings
// (gateway.consumer_allow_external_default, hot) and the meter's cost
// budgets (gateway.consumer_monthly_cost_usd_default per consumer, × the
// tenant envelope factor per tenant); the cost a route adds books on Settle;
// gatewayDeps wires the source; the boot seed carries the month's cost.

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

func TestMeterLimiterEgress(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CONSUMER_MONTHLY_COST_USD_DEFAULT": "1.0", "CTX_GATEWAY_TENANT_ENVELOPE_FACTOR": "2"})
	meter := keyquota.New(time.Now)
	meter.MarkSeeded()
	l := meterLimiter{meter: meter, settings: gatewaySettingsGetter(cfgStore)}
	ctx := context.Background()
	if p := l.Egress(ctx, urgentConsumer("c1"), llmlog.GatewayOpChat); p.AllowExternal || p.AllowUnmetered || p.CostExhausted {
		t.Fatalf("defaults: %+v, want no permission, nothing exhausted", p)
	}
	next := *cfgStore.Snapshot()
	next.Gateway.ConsumerAllowExternalDefault = true
	if err := cfgStore.Replace(&next); err != nil {
		t.Fatal(err)
	}
	if p := l.Egress(ctx, urgentConsumer("c1"), llmlog.GatewayOpChat); !p.AllowExternal || p.AllowUnmetered {
		t.Fatalf("after the hot permission: %+v, want AllowExternal, never AllowUnmetered (no source before W06-15)", p)
	}
	est := oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 1, ClientMaxTokens: true}
	for i, cost := range []float64{0.7, 0.4} {
		res, ref := l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, est)
		if ref != nil {
			t.Fatal(ref)
		}
		res.(oai.CostAdder).AddCost(cost)
		res.Settle(dispatch.Usage{CompletionTokens: 1})
		if got := l.Egress(ctx, urgentConsumer("c1"), llmlog.GatewayOpChat).CostExhausted; got != (i == 1) {
			t.Fatalf("after %v USD: exhausted %v", []float64{0.7, 1.1}[i], got)
		}
	}
	if p := l.Egress(ctx, urgentConsumer("c1"), llmlog.GatewayOpEmbed); p.CostExhausted || !p.CostUntracked {
		t.Errorf("embed: %+v — chat's cost must not exhaust embed, and embed books no provider cost (CostUntracked)", p)
	}
	if p := l.Egress(ctx, urgentConsumer("c1"), llmlog.GatewayOpChat); p.CostUntracked {
		t.Error("chat books the provider cost: CostUntracked must be false")
	}
	// Tenant envelope 2 × 1.0: c1 booked 1.1, c2 of the same tenant 0.9.
	res, _ := l.Admit(ctx, urgentConsumer("c2"), modelalias.Alias{}, est)
	res.(oai.CostAdder).AddCost(0.9)
	res.Settle(dispatch.Usage{CompletionTokens: 1})
	if !l.Egress(ctx, urgentConsumer("c3"), llmlog.GatewayOpChat).CostExhausted {
		t.Error("tenant envelope 2.0 with 2.0 booked: c3 not exhausted")
	}
	if d := gatewayDeps(ctx, nil, cfgStore, nil, nil, nil, nil, meter, nil); d.Egress == nil {
		t.Error("gatewayDeps with the meter: Egress nil")
	}
	if d := gatewayDeps(ctx, nil, cfgStore, nil, nil, nil, nil, nil, nil); d.Egress != nil {
		t.Errorf("gatewayDeps without a meter: Egress %T, want nil (fail-closed: free rows only)", d.Egress)
	}
	if rows := meterSeedRows([]llmusage.ConsumerUsage{{ConsumerID: "c", TenantID: "t", Pipeline: llmlog.PipelineGatewayChat, MonthExternalCost: 0.25}}); len(rows) != 1 || rows[0].MonthCostUSD != 0.25 {
		t.Errorf("seed rows %+v, want the month's external cost", rows)
	}
}
