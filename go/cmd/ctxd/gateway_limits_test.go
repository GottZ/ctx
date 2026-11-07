package main

// The limits rows of context_gateway_limits through the production adapter
// (wave W06-15, design/06 §4.3; W06-11 and W06-13/14 handovers): the rows
// reach keyquota.Limits per level, max_concurrency of a row replaces the
// registry default (L1), the egress permissions and the cost budget come from
// the rows, and a reload applies from the next request on while running
// requests keep what they were admitted under.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

func limitsI64(v int64) *int64     { return &v }
func limitsF64(v float64) *float64 { return &v }
func limitsBool(v bool) *bool      { return &v }

// limitsSource is a registry over a row set the test swaps; reload publishes
// the current set the way a manage write does (synchronous Load).
type limitsSource struct {
	mu   sync.Mutex
	rows []gatewaylimits.Row
	reg  *gatewaylimits.Registry
}

func newLimitsSource(t *testing.T, rows ...gatewaylimits.Row) *limitsSource {
	t.Helper()
	s := &limitsSource{rows: rows}
	s.reg = gatewaylimits.NewRegistry(func(context.Context) ([]gatewaylimits.Row, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]gatewaylimits.Row(nil), s.rows...), nil
	})
	if err := s.reg.Load(context.Background()); err != nil {
		t.Errorf("limits registry Load: %v", err)
	}
	return s
}

func (s *limitsSource) reload(t *testing.T, rows ...gatewaylimits.Row) {
	t.Helper()
	s.mu.Lock()
	s.rows = rows
	s.mu.Unlock()
	if err := s.reg.Load(context.Background()); err != nil {
		t.Errorf("limits registry reload: %v", err)
	}
}

func limitsConsumerRow(consumer string, op keyquota.Op, v gatewaylimits.Values) gatewaylimits.Row {
	return gatewaylimits.Row{ID: "row-" + consumer + "-" + string(op), Kind: gatewaylimits.SubjectConsumer, ConsumerID: consumer,
		Tier: gatewaylimits.TierServer, Op: op, Values: v}
}

// TestMeterLimitsRowsReachEveryLevel: a consumer row, a tenant row, an
// instance row and a tenant envelope row land in the meter's limits — the
// minute buckets, L1, the consumer budget and both envelopes.
func TestMeterLimitsRowsReachEveryLevel(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_TENANT_ENVELOPE_FACTOR": "5", "CTX_GATEWAY_INSTANCE_ENVELOPE_FACTOR": "10"})
	src := newLimitsSource(t,
		limitsConsumerRow("c1", keyquota.OpChat, gatewaylimits.Values{RPM: limitsI64(3)}),
		gatewaylimits.Row{ID: "t", Kind: gatewaylimits.SubjectTenant, TenantID: "t1", Tier: gatewaylimits.TierServer, Op: keyquota.OpChat,
			Values: gatewaylimits.Values{TPM: limitsI64(4000)}},
		gatewaylimits.Row{ID: "i", Kind: gatewaylimits.SubjectInstance, Tier: gatewaylimits.TierServer, Op: keyquota.OpChat,
			Values: gatewaylimits.Values{DailyTokens: limitsI64(1000), MaxConcurrency: limitsI64(1)}},
		gatewaylimits.Row{ID: "e", Kind: gatewaylimits.SubjectTenantEnvelope, TenantID: "t1", Tier: gatewaylimits.TierServer, Op: keyquota.OpChat,
			Values: gatewaylimits.Values{DailyTokens: limitsI64(1500)}},
	)
	_, lim, _ := meterLimits(cfgStore.Snapshot().GatewaySettings().Meter, src.reg.Snapshot(), urgentConsumer("c1"), llmlog.GatewayOpChat) //nolint:forbidigo // test: gateway.* is global-only
	if lim.RPM != 3 || lim.TPM != 4000 || lim.Concurrency != 1 {
		t.Errorf("minute/L1 limits = rpm %d tpm %d C %d, want the consumer row 3, the tenant row 4000, the instance row 1", lim.RPM, lim.TPM, lim.Concurrency)
	}
	if lim.Consumer.DailyTokens != 1000 || lim.Tenant.DailyTokens != 1500 || lim.Instance.DailyTokens != 10000 {
		t.Errorf("budgets = consumer %d tenant %d instance %d, want the instance row 1000, the envelope row 1500, factor 10 × 1000",
			lim.Consumer.DailyTokens, lim.Tenant.DailyTokens, lim.Instance.DailyTokens)
	}
}

// TestMeterLimiterRowConcurrencyReplacesRegistry is the W06-11 handover
// (L1): a consumer's max_concurrency row replaces gateway.consumer_concurrency_default
// for that consumer and op, in both directions; another consumer keeps the default.
func TestMeterLimiterRowConcurrencyReplacesRegistry(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT": "2"})
	meter := keyquota.New(time.Now)
	meter.MarkSeeded()
	src := newLimitsSource(t,
		limitsConsumerRow("c1", keyquota.OpChat, gatewaylimits.Values{MaxConcurrency: limitsI64(1)}),
		limitsConsumerRow("c2", keyquota.OpChat, gatewaylimits.Values{MaxConcurrency: limitsI64(3)}))
	l := meterLimiter{meter: meter, settings: gatewaySettingsGetter(cfgStore), limits: src.reg}
	ctx := context.Background()
	est := oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 1, ClientMaxTokens: true}
	admitted := func(consumer string, n int) int {
		got := 0
		for range n {
			if _, ref := l.Admit(ctx, urgentConsumer(consumer), modelalias.Alias{}, est); ref == nil {
				got++
			} else if ref.Code != oai.CodeConcurrencyLimitExceeded {
				t.Fatalf("%s: refusal %q, want only %q", consumer, ref.Code, oai.CodeConcurrencyLimitExceeded)
			}
		}
		return got
	}
	if got := admitted("c1", 3); got != 1 {
		t.Errorf("c1 with max_concurrency row 1: %d of 3 open at once, want 1 (the row replaces the registry default 2)", got)
	}
	if got := admitted("c2", 4); got != 3 {
		t.Errorf("c2 with max_concurrency row 3: %d of 4 open at once, want 3", got)
	}
	if got := admitted("c3", 4); got != 2 {
		t.Errorf("c3 without a row: %d of 4 open at once, want the registry default 2", got)
	}
}

// TestMeterLimiterEgressFromRows is the W06-14 handover: allow_external of
// a row keeps an openrouter row in the chain (WithoutBillable),
// allow_unmetered_external keeps a non-reporting external row, and a row's
// monthly_cost_usd used up drops the metered row again (degrade, no 429).
func TestMeterLimiterEgressFromRows(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CONSUMER_MONTHLY_COST_USD_DEFAULT": "100"})
	meter := keyquota.New(time.Now)
	meter.MarkSeeded()
	src := newLimitsSource(t,
		limitsConsumerRow("c1", keyquota.OpChat, gatewaylimits.Values{AllowExternal: limitsBool(true), MonthlyCostUSD: limitsF64(0.5)}),
		limitsConsumerRow("c2", keyquota.OpChat, gatewaylimits.Values{AllowExternal: limitsBool(true), AllowUnmeteredExternal: limitsBool(true)}))
	l := meterLimiter{meter: meter, settings: gatewaySettingsGetter(cfgStore), limits: src.reg}
	ctx := context.Background()
	chain := []backends.Backend{
		{Name: "local", Locality: backends.LocalityLocal, ProviderClass: "llamacpp"},
		{Name: "openrouter", Locality: backends.LocalityExternal, ProviderClass: backends.ProviderOpenRouter},
		{Name: "generic-ext", Locality: backends.LocalityExternal, ProviderClass: "generic"},
	}
	names := func(bs []backends.Backend) []string {
		out := make([]string, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.Name)
		}
		return out
	}
	if got := names(backends.WithoutBillable(chain, l.Egress(ctx, urgentConsumer("c1"), llmlog.GatewayOpChat))); len(got) != 2 || got[1] != "openrouter" {
		t.Errorf("c1 allow_external row: chain %v, want [local openrouter]", got)
	}
	if got := names(backends.WithoutBillable(chain, l.Egress(ctx, urgentConsumer("c2"), llmlog.GatewayOpChat))); len(got) != 3 {
		t.Errorf("c2 allow_external + allow_unmetered_external rows: chain %v, want all three", got)
	}
	if got := names(backends.WithoutBillable(chain, l.Egress(ctx, urgentConsumer("c3"), llmlog.GatewayOpChat))); len(got) != 1 {
		t.Errorf("c3 without a row: chain %v, want [local] (default no permission)", got)
	}
	res, ref := l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 1, ClientMaxTokens: true})
	if ref != nil {
		t.Fatal(ref)
	}
	res.(oai.CostAdder).AddCost(0.6)
	res.Settle(dispatch.Usage{CompletionTokens: 1})
	p := l.Egress(ctx, urgentConsumer("c1"), llmlog.GatewayOpChat)
	if !p.CostExhausted {
		t.Errorf("c1 booked 0.6 USD against its row's 0.5: CostExhausted false, want true (the row's budget, not the setting's 100)")
	}
	if got := names(backends.WithoutBillable(chain, p)); len(got) != 1 {
		t.Errorf("c1 after the row budget is used up: chain %v, want [local] (degrade)", got)
	}
}

// TestMeterLimiterRowReloadNextRequestNotRunning: a reloaded row applies to
// the next request; a request admitted before keeps running and ends
// normally — lowering max_concurrency under the open count refuses the next
// one without touching the open ones, raising it admits more at once.
func TestMeterLimiterRowReloadNextRequestNotRunning(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT": "4"})
	meter := keyquota.New(time.Now)
	meter.MarkSeeded()
	src := newLimitsSource(t, limitsConsumerRow("c1", keyquota.OpChat, gatewaylimits.Values{MaxConcurrency: limitsI64(2)}))
	l := meterLimiter{meter: meter, settings: gatewaySettingsGetter(cfgStore), limits: src.reg}
	ctx := context.Background()
	est := oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 1, ClientMaxTokens: true}
	var open []oai.LimitReservation
	for range 3 {
		if res, ref := l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, est); ref == nil {
			open = append(open, res)
		}
	}
	if len(open) != 2 {
		t.Fatalf("row max_concurrency 2: %d open, want 2", len(open))
	}
	src.reload(t, limitsConsumerRow("c1", keyquota.OpChat, gatewaylimits.Values{MaxConcurrency: limitsI64(1)}))
	if _, ref := l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, est); ref == nil || ref.Code != oai.CodeConcurrencyLimitExceeded {
		t.Errorf("after lowering the row to 1 with 2 open: next request %v, want %s", ref, oai.CodeConcurrencyLimitExceeded)
	}
	for _, res := range open {
		res.Settle(dispatch.Usage{CompletionTokens: 1}) // the running requests end as they were admitted
	}
	if _, ref := l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, est); ref != nil {
		t.Errorf("after the running requests ended: next request refused %v, want admitted under the new row 1", ref)
	}
	if _, ref := l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, est); ref == nil {
		t.Error("second request with one open under the reloaded row 1: admitted, want refused (the next request sees the reload)")
	}
	src.reload(t, limitsConsumerRow("c1", keyquota.OpChat, gatewaylimits.Values{MaxConcurrency: limitsI64(3)}))
	got := 0
	for range 3 {
		if _, ref := l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, est); ref == nil {
			got++
		}
	}
	if got != 2 {
		t.Errorf("after raising the row to 3 with 1 open: %d of 3 more admitted, want 2", got)
	}
}
