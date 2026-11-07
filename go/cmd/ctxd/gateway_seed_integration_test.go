//go:build integration

package main

// The meter's boot seed against a real database (wave W06-13 gates (b) and
// (c)): a restarted process — a new model gateway over the same database —
// finds the day budget a consumer used before the restart, including the
// estimate a stream cut short without a usage chunk booked in the token
// columns (B-06-8).
//
//	go test -tags=integration -p 1 ./cmd/ctxd/ -run 'GatewayBootSeed' -count=1 -v

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestGatewayBootSeedRestoresTheBudget is gate (b) (Finding 1) and the seed
// half of gate (c): the consumer's rows of today — one with the backend's
// usage, one a stream cut short whose estimate stands in the columns with
// metadata.gw.usage "estimated" — add up to the day budget of 1000; a model
// gateway built afterwards (the restart) refuses the consumer's next request
// with insufficient_quota, while a consumer without rows passes. Red against
// MarkSeeded at start (the meter of W06-10): the budget is free again.
func TestGatewayBootSeedRestoresTheBudget(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CHAT_DAILY_TOKENS_DEFAULT": "1000"})
	var consumer, fresh string
	for _, p := range []*string{&consumer, &fresh} {
		if err := pool.QueryRow(ctx, `INSERT INTO context_model_consumers (tenant_id, label) VALUES ($1::uuid, $2) RETURNING id::text`,
			store.DefaultTenantID, "w0613-seed-"+time.Now().Format("150405.000000000")).Scan(p); err != nil {
			t.Fatal(err)
		}
	}
	llmlog.RecordGateway(pool, llmlog.GatewayRow{Pipeline: llmlog.PipelineGatewayChat, ConsumerID: consumer,
		Gateway:   llmlog.Gateway{Alias: "w0613", Usage: llmlog.UsageBackend, Finish: llmlog.FinishStop},
		Telemetry: llmlog.Entry{Model: "m", Host: "h", PromptTokens: 400, CompletionTokens: 200}})
	// The stream cut short: the route's estimate (W02-08 streamTokens) in
	// the columns, the true tokens unknown to the backend's usage.
	llmlog.RecordGateway(pool, llmlog.GatewayRow{Pipeline: llmlog.PipelineGatewayChat, ConsumerID: consumer,
		Gateway:   llmlog.Gateway{Alias: "w0613", Usage: llmlog.UsageEstimated, Finish: llmlog.FinishAborted, Stream: true},
		Telemetry: llmlog.Entry{Model: "m", Host: "h", PromptTokens: 300, CompletionTokens: 100}})
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := llmlog.Settle(sctx, pool); err != nil {
		t.Fatal(err)
	}
	var estimated, prompt, completion int64
	if err := pool.QueryRow(ctx, `SELECT estimated_calls, prompt_tokens, completion_tokens FROM context_llm_usage_hourly WHERE consumer_id = $1::uuid`,
		consumer).Scan(&estimated, &prompt, &completion); err != nil {
		t.Fatal(err)
	}
	if estimated != 1 || prompt != 700 || completion != 300 {
		t.Fatalf("rollup: estimated %d, prompt %d, completion %d — want 1, 700, 300 (the estimate in the columns)", estimated, prompt, completion)
	}

	gw := newModelGateway(pool, cfgStore) // the restart
	if !gw.meter.Seeded() {
		t.Fatal("the restarted gateway's meter is not seeded")
	}
	l := gatewayDeps(ctx, pool, cfgStore, nil, nil, nil, nil, gw.meter).Limiter
	est := oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 1, ClientMaxTokens: true, PromptRunes: 9}
	_, ref := l.Admit(ctx, urgentConsumerOf(consumer, store.DefaultTenantID), modelalias.Alias{}, est)
	if ref == nil || ref.Code != oai.CodeInsufficientQuota || ref.ShouldRetry {
		t.Fatalf("consumer with 1000 of 1000 used today, after the restart: %+v, want 429 insufficient_quota", ref)
	}
	if _, ref := l.Admit(ctx, urgentConsumerOf(fresh, store.DefaultTenantID), modelalias.Alias{}, est); ref != nil {
		t.Errorf("consumer without rows after the restart: %+v, want admitted", ref)
	}
}

// urgentConsumerOf is an urgent app consumer of tenant.
func urgentConsumerOf(id, tenant string) *auth.ConsumerAuth {
	return &auth.ConsumerAuth{IsValid: true, ConsumerID: id, TenantID: tenant, Kind: auth.KindApp, AdmissionLane: handler.LaneUrgent}
}

// TestGatewayRestartDuringOpenReservations is gate (d) (B-06-9) on the
// meter and the row writer: five reservations are open when the process
// stops (streams that relayed text, no usage chunk), one request settled
// before; ForceSettleAll ends the five with their estimate and their writers
// put the rows into context_llm_log, the llmlog settle lands them, and the
// next process's seed sees at least what the meter had used before the
// restart. Red against the shutdown without ForceSettleAll: the open
// requests' tokens are lost and the budget is free again.
func TestGatewayRestartDuringOpenReservations(t *testing.T) {
	for _, force := range []bool{true, false} {
		t.Run(map[bool]string{true: "with ForceSettleAll", false: "without (the old shutdown)"}[force], func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			ctx := context.Background()
			var consumer string
			if err := pool.QueryRow(ctx, `INSERT INTO context_model_consumers (tenant_id, label) VALUES ($1::uuid, 'w0613-restart') RETURNING id::text`,
				store.DefaultTenantID).Scan(&consumer); err != nil {
				t.Fatal(err)
			}
			cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CHAT_DAILY_TOKENS_DEFAULT": "1000000", "CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT": "8"})
			gw := newModelGateway(pool, cfgStore)
			l := gatewayDeps(ctx, pool, cfgStore, nil, nil, nil, nil, gw.meter).Limiter
			ca := urgentConsumerOf(consumer, store.DefaultTenantID)
			est := oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 4000, ClientMaxTokens: true, PromptRunes: 900}
			row := func(u dispatch.Usage, usage string) llmlog.GatewayRow {
				return llmlog.GatewayRow{Pipeline: llmlog.PipelineGatewayChat, ConsumerID: consumer,
					Gateway:   llmlog.Gateway{Alias: "w0613", Usage: usage, Finish: llmlog.FinishStop},
					Telemetry: llmlog.Entry{Model: "m", Host: "h", PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens}}
			}
			res, ref := l.Admit(ctx, ca, modelalias.Alias{}, est)
			if ref != nil {
				t.Fatal(ref)
			}
			done := dispatch.Usage{PromptTokens: 300, CompletionTokens: 200}
			res.Settle(done)
			llmlog.RecordGateway(pool, row(done, llmlog.UsageBackend))
			usedBefore := int64(500)
			for i := range 5 {
				res, ref := l.Admit(ctx, ca, modelalias.Alias{}, est)
				if ref != nil {
					t.Fatal(ref)
				}
				fs := res.(oai.ForcedSettler)
				fs.AddStreamed(180 * (i + 1))
				fs.OnForcedSettle(func(u dispatch.Usage) { llmlog.RecordGateway(pool, row(u, llmlog.UsageEstimated)) })
				usedBefore += int64(promptguard.TokenEstimate(900) + promptguard.TokenEstimate(180*(i+1)))
			}
			if force {
				if n := gw.meter.ForceSettleAll(ctx); n != 5 {
					t.Fatalf("ForceSettleAll ended %d, want 5", n)
				}
			}
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := llmlog.Settle(sctx, pool); err != nil {
				t.Fatal(err)
			}
			next := *cfgStore.Snapshot()
			next.Gateway.ChatDailyTokensDefault = int(usedBefore)
			if err := cfgStore.Replace(&next); err != nil {
				t.Fatal(err)
			}
			gw2 := newModelGateway(pool, cfgStore) // the restart
			l2 := gatewayDeps(ctx, pool, cfgStore, nil, nil, nil, nil, gw2.meter).Limiter
			_, ref = l2.Admit(ctx, ca, modelalias.Alias{}, oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 1, ClientMaxTokens: true})
			if exhausted := ref != nil && ref.Code == oai.CodeInsufficientQuota; exhausted != force {
				t.Fatalf("after the restart with a budget of the %d tokens used before: refusal %+v — want exhausted=%v", usedBefore, ref, force)
			}
		})
	}
}
