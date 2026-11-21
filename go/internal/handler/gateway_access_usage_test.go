package handler

// W06-16/W06-18 at the access-log stage (RC-D S2 hardening — no S1 gate
// covered the hook): a successful request of a consumer notes first_use of
// its model key and counts the consumer in the top-consumers ring; a refused
// request and a request without a consumer do neither.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
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

// TestGatewayAccessLogClientGone — issue #453: a /v1 client that leaves
// while the handler runs got nothing; the access line says 499 with the
// handler's status, as the request log does. The booking stays as for the
// same request answered: first_use and the top-consumers ring see it, no
// refusal is counted.
func TestGatewayAccessLogClientGone(t *testing.T) {
	sink, events, ring := &gwLogSink{}, &gwEventSink{}, NewConsumerRing(nil)
	settings := config.Defaults().GatewaySettings()
	access := NewGatewayAccessLog(GatewayAccessLogConfig{
		Settings: func() oai.Settings { return settings },
		Logger:   slog.New(sink),
		Events:   events,
		Ring:     ring,
	})
	started := make(chan struct{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ar := oai.AccessRecordFrom(r.Context()); ar != nil {
			ar.ConsumerID, ar.TenantID, ar.ModelKeyID, ar.Op = "c1", "t1", "k1", "chat"
			ar.PromptTokens, ar.CompletionTokens = 30, 12
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	})
	srv, done := abortServer(t, access.Handler(inner), nil)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: want its own cancellation, got %v", err)
	}
	waitDone(t, done)

	lines := sink.lines("gateway request")
	if len(lines) != 1 {
		t.Fatalf("%d gateway request lines, want 1", len(lines))
	}
	if lines[0]["status"] != int64(clientClosedStatus) || lines[0]["handler_status"] != int64(http.StatusOK) {
		t.Errorf("status %v handler_status %v, want %d and 200 (the client left)", lines[0]["status"], lines[0]["handler_status"], clientClosedStatus)
	}
	if n := len(sink.lines("gateway rejections")); n != 0 {
		t.Errorf("a client that left was counted as a refusal (%d lines)", n)
	}
	if len(events.events) != 1 || events.events[0].ConsumerID != "c1" {
		t.Errorf("first_use notes %+v, want the one of c1 as for an answered request", events.events)
	}
	if top := ring.Top(10, ""); len(top) != 1 || top[0] != (TopConsumer{ConsumerID: "c1", Requests: 1, Tokens: 42}) {
		t.Errorf("ring %+v, want c1 with 1 request and 42 tokens as for an answered request", top)
	}
}
