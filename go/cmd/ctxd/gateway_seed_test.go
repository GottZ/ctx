package main

// The boot seed of the meter (wave W06-13 (c), (h); design/06 §4.2
// "Boot-Seed", §4.1 step 7): until a seed read succeeded every /v1 admission
// is 429 warming with Retry-After 5 and x-should-retry true; the read is
// retried, and once it succeeds the meter opens with the rollup's figures.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// TestMeterSeedFailureAnswersWarming is gate (h) at the gateway: a seed read
// that fails twice leaves the meter closed — 429 rate_limit_exceeded,
// Retry-After 5 s, retryable — and the loop retries until the third read
// succeeds; then the meter is open and the consumer's rollup day counts.
func TestMeterSeedFailureAnswersWarming(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CHAT_DAILY_TOKENS_DEFAULT": "1000"})
	meter := keyquota.New(time.Now)
	var reads atomic.Int64
	release := make(chan struct{})
	read := func(context.Context, time.Time) ([]llmusage.ConsumerUsage, error) {
		if reads.Add(1) <= 2 {
			return nil, errors.New("fake: connection refused")
		}
		<-release
		return []llmusage.ConsumerUsage{{ConsumerID: "c1", TenantID: "t1", Pipeline: llmlog.PipelineGatewayChat, DayTokens: 1000, DayCalls: 1, MonthTokens: 1000}}, nil
	}
	l := meterLimiter{meter: meter, settings: gatewaySettingsGetter(cfgStore)}
	est := oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 1, ClientMaxTokens: true, PromptRunes: 9}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		seedMeter(ctx, meter, read, time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for reads.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, ref := l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, est)
	if ref == nil || ref.Code != oai.CodeRateLimitExceeded || ref.RetryAfter != 5*time.Second || !ref.ShouldRetry {
		t.Fatalf("while the seed fails: %+v, want 429 warming, Retry-After 5s, retryable", ref)
	}
	close(release)
	<-done
	if !meter.Seeded() || reads.Load() != 3 {
		t.Fatalf("after the third read: seeded %v, reads %d", meter.Seeded(), reads.Load())
	}
	_, ref = l.Admit(ctx, urgentConsumer("c1"), modelalias.Alias{}, est)
	if ref == nil || ref.Code != oai.CodeInsufficientQuota {
		t.Fatalf("seeded consumer with 1000 of 1000 used today: %+v, want insufficient_quota", ref)
	}
}

// TestSeedRowsMapThePipelines: a rollup row's pipeline becomes the meter's
// op through llmlog.GatewayOp; a pipeline that is no gateway op is dropped.
func TestSeedRowsMapThePipelines(t *testing.T) {
	rows := meterSeedRows([]llmusage.ConsumerUsage{
		{ConsumerID: "c", TenantID: "t", Pipeline: llmlog.PipelineGatewayChat, DayTokens: 1, DayCalls: 2, MonthTokens: 3},
		{ConsumerID: "c", TenantID: "t", Pipeline: llmlog.PipelineGatewayEmbeddings, DayTokens: 4},
		{ConsumerID: "c", TenantID: "t", Pipeline: llmlog.PipelineGatewayRerank, DayTokens: 5},
		{ConsumerID: "c", TenantID: "t", Pipeline: "query", DayTokens: 6},
	})
	if len(rows) != 3 || rows[0].Op != keyquota.OpChat || rows[0].DayTokens != 1 || rows[0].DayRequests != 2 || rows[0].MonthTokens != 3 ||
		rows[1].Op != keyquota.OpEmbed || rows[2].Op != keyquota.OpRerank {
		t.Errorf("seed rows %+v", rows)
	}
}
