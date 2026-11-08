package main

// W06-18 event probe at the meter adapter (RC-D cluster C2): with
// gateway.chat_daily_tokens_default 2000 and gateway.soft_ratio 0.5, the
// adapter notes soft_limit for the admissions over the share (consumer
// level, daily_tokens, chat, the UTC day's start) and hard_limit for the
// budget refusal — and nothing for the admissions below the share. The
// recorder behind the sink dedupes (internal/usageevents); the adapter only
// translates the meter's answer.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/usageevents"
)

type recordingSink struct {
	mu     sync.Mutex
	events []usageevents.Event
}

func (s *recordingSink) Note(e usageevents.Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *recordingSink) of(k usageevents.Kind) []usageevents.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []usageevents.Event
	for _, e := range s.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func TestMeterLimiterNotesUsageEvents(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{
		"CTX_GATEWAY_CHAT_DAILY_TOKENS_DEFAULT": "2000",
		"CTX_GATEWAY_SOFT_RATIO":                "0.5",
	})
	gw := meterTestGateway(t, cfgStore)
	sink := &recordingSink{}
	l := meterLimiter{meter: gw.meter, settings: gatewaySettingsGetter(cfgStore), events: sink}
	ctx := context.Background()
	ca := urgentConsumer("events")
	est := oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 100, ClientMaxTokens: true}
	// 100 tokens each: admissions 1–9 stay below 1000 (the share), from the
	// 10th on every admission is at or over it; the 21st is refused.
	admitted, refused := 0, 0
	for range 25 {
		res, ref := l.Admit(ctx, ca, modelalias.Alias{}, est)
		if ref != nil {
			refused++
			if ref.Code != oai.CodeInsufficientQuota {
				t.Fatalf("refusal %+v, want insufficient_quota", ref)
			}
			continue
		}
		admitted++
		res.Settle(dispatch.Usage{CompletionTokens: 100})
	}
	if admitted != 20 || refused != 5 {
		t.Fatalf("fixture: %d admitted, %d refused — want 20 and 5", admitted, refused)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	soft := sink.of(usageevents.KindSoftLimit)
	if len(soft) == 0 {
		t.Fatalf("W06-18 gate (event probe): no soft_limit note after 20 admissions at ratio 0.5 of 2000 (sink saw %d events)", len(sink.events))
	}
	if len(soft) != 11 {
		t.Errorf("W06-18 gate (event probe): %d soft_limit notes, want 11 (admissions 10–20; the recorder dedupes, the adapter notes each)", len(soft))
	}
	if e := soft[0]; e.ConsumerID != "events" || e.TenantID != "t1" || e.Op != "chat" || e.Dimension != "daily_tokens" ||
		e.Level != "consumer" || !e.WindowStart.Equal(day) || e.Limit != 2000 {
		t.Errorf("W06-18 gate (event probe): soft_limit note %+v, want consumer events/t1, chat, daily_tokens, level consumer, window %v, limit 2000", e, day)
	}
	hard := sink.of(usageevents.KindHardLimit)
	if len(hard) != 5 || hard[0].Dimension != "daily_tokens" || !hard[0].WindowStart.Equal(day) {
		t.Errorf("W06-18 gate (event probe): hard_limit notes %+v, want 5 for daily_tokens of window %v", hard, day)
	}
}
