package handler

// W06-16/W06-18 at the access-log stage (RC-D S2 hardening — no S1 gate
// covered the hook): a successful request of a consumer notes first_use of
// its model key and counts the consumer in the top-consumers ring; a refused
// request and a request without a consumer do neither.

import (
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/usageevents"
)

type gwEventSink struct {
	mu     sync.Mutex
	events []usageevents.Event
}

func (s *gwEventSink) Note(e usageevents.Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func TestGatewayAccessLogNotesFirstUseAndRing(t *testing.T) {
	events := &gwEventSink{}
	ring := NewConsumerRing(nil)
	settings := config.Defaults().GatewaySettings()
	log := NewGatewayAccessLog(GatewayAccessLogConfig{
		Settings: func() oai.Settings { return settings },
		Logger:   slog.New(&gwLogSink{}),
		Events:   events,
		Ring:     ring,
	})
	e := &gwAccessEnv{log: log}
	answer := func(consumer, key string, status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if ar := oai.AccessRecordFrom(r.Context()); ar != nil {
				ar.ConsumerID, ar.TenantID, ar.ModelKeyID, ar.Op = consumer, "t1", key, "chat"
				ar.PromptTokens, ar.CompletionTokens = 30, 12
			}
			w.WriteHeader(status)
		}
	}
	e.serve(answer("c1", "k1", http.StatusOK))
	e.serve(answer("c1", "k1", http.StatusOK))
	e.serve(answer("c2", "k2", http.StatusTooManyRequests))
	e.serve(answer("", "", http.StatusOK))
	if len(events.events) != 2 {
		t.Fatalf("W06-18 gate (first_use hook): %d notes, want 2 (two successes of c1; the recorder dedupes per key) — %+v", len(events.events), events.events)
	}
	if got := events.events[0]; got.Kind != usageevents.KindFirstUse || got.ConsumerID != "c1" || got.ModelKeyID != "k1" || got.TenantID != "t1" || got.Op != "chat" {
		t.Errorf("W06-18 gate (first_use hook): note %+v, want first_use of c1/k1/t1/chat", got)
	}
	top := ring.Top(10, "")
	if len(top) != 1 || top[0] != (TopConsumer{ConsumerID: "c1", Requests: 2, Tokens: 84}) {
		t.Errorf("W06-16 gate (ring hook): top %+v, want c1 with 2 requests and 84 tokens only", top)
	}
}
