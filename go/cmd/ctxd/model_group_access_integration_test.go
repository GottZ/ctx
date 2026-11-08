//go:build integration

// W06-9 gate (d), B-06-22: the probe router — handler.RequestID +
// handler.Logger globally, mountModelGroup with the access-log stage, the
// real enabled gate and the real model-key door over a real database —
// counted through a slog handler.
//
//	go test -tags=integration -p 1 -count=1 -run TestModelGroupGatewayAccessLog -v ./cmd/ctxd/
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// probeSink counts the records it sees, by message, with their attributes.
type probeSink struct {
	mu   sync.Mutex
	recs []map[string]any
}

func (s *probeSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *probeSink) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Resolve().Any()
		return true
	})
	s.mu.Lock()
	s.recs = append(s.recs, m)
	s.mu.Unlock()
	return nil
}
func (s *probeSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *probeSink) WithGroup(string) slog.Handler      { return s }

// take returns and clears the records seen so far.
func (s *probeSink) take() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.recs
	s.recs = nil
	return out
}

func countMsg(recs []map[string]any, msg string) int {
	n := 0
	for _, r := range recs {
		if r["msg"] == msg {
			n++
		}
	}
	return n
}

func linesOf(recs []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func TestModelGroupGatewayAccessLog_Integration(t *testing.T) {
	seed := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, seed, "access-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	consumer, mk, model, err := store.CreateConsumerWithKey(ctx, seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "access-probe"}, nil, store.APIStamp(admin.ID, "access"))
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	forged, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("forged key: %v", err)
	}

	sink := &probeSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(sink))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var enabled, accessLog atomic.Bool
	enabled.Store(true)
	accessLog.Store(true)
	base := config.Defaults().GatewaySettings()
	get := func() oai.Settings {
		s := base
		s.Enabled, s.AccessLog = enabled.Load(), accessLog.Load()
		return s
	}
	door := handler.ModelAuth(handler.ModelAuthConfig{
		Pool: seed, Settings: get, Usage: groupUsage{}, Logger: slog.New(sink),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
			w.WriteHeader(rej.Status)
		},
	})
	access := handler.NewGatewayAccessLog(handler.GatewayAccessLogConfig{Settings: get, Logger: slog.New(sink)})

	r := chi.NewRouter()
	r.Use(handler.RequestID, handler.Logger)
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mountModelGroup(r, "/v1", modelGroupDeps{access: access, gate: oai.EnabledGate(get), door: door, ingress: &fedhop.Ingress{}, maxDepth: func() int { return 3 }, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop, routes: []modelRoute{
		{http.MethodGet, "/models", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })},
		// The probe route of the 429 flood: authenticated, then refused —
		// the shape of a meter rejection (W06-10) without the meter.
		{http.MethodPost, "/chat/completions", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})},
	}})
	send := func(n int, method, path, bearer string, want int) {
		t.Helper()
		for i := range n {
			req := httptest.NewRequest(method, path, nil)
			req.TLS = &tls.ConnectionState{} // the consumer does not allow plaintext
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if want != 0 && rec.Code != want {
				t.Fatalf("%s %s request %d: status %d, want %d", method, path, i, rec.Code, want)
			}
		}
	}
	wantOneLine := func(phase string, recs []map[string]any, consumerID, reason string, count int64) {
		t.Helper()
		lines := linesOf(recs, "gateway rejections")
		if len(lines) != 1 || lines[0]["consumer_id"] != consumerID || lines[0]["reason"] != reason ||
			lines[0]["count"] != count || lines[0]["window"] != "1m" {
			t.Fatalf("%s: rejection lines %v, want one (%q, %s, count=%d, window=1m)", phase, lines, consumerID, reason, count)
		}
		if n := countMsg(recs, "http request"); n != 0 {
			t.Fatalf("%s: %d http request line(s) under /v1, want 0", phase, n)
		}
		if n := len(recs); n != 1 {
			t.Fatalf("%s: %d line(s) in total, want exactly the one rejection line: %v", phase, n, recs)
		}
	}

	// 1 — 1 000 requests with a forged (malformed) key: 401 at stage 4,
	// no database. Ist: 1 000 http request lines.
	send(1000, http.MethodPost, "/v1/chat/completions", "sk-not-a-model-key", http.StatusUnauthorized)
	if recs := sink.take(); len(recs) != 0 {
		t.Fatalf("forged keys: %d line(s) before the window ended (%d http request), want 0", len(recs), countMsg(recs, "http request"))
	}
	access.Flush()
	wantOneLine("forged keys", sink.take(), "", oai.CodeInvalidAPIKey, 1000)

	// 2 — 1 000 requests with a well-formed forged key: refused by the
	// database, then by the failure lock. At most one line per reason.
	send(1000, http.MethodPost, "/v1/chat/completions", forged.Plaintext(), 0)
	access.Flush()
	recs := sink.take()
	seen := map[string]bool{}
	var sum int64
	for _, l := range linesOf(recs, "gateway rejections") {
		reason, _ := l["reason"].(string)
		if seen[reason] || l["consumer_id"] != "" {
			t.Fatalf("well-formed forgeries: line %v repeats a (consumer, reason) pair or names a consumer", l)
		}
		seen[reason] = true
		n, _ := l["count"].(int64)
		sum += n
	}
	if sum != 1000 || countMsg(recs, "http request") != 0 || len(recs) != len(seen) {
		t.Fatalf("well-formed forgeries: %d line(s) %v, counts sum %d, want ≤ 1 line per reason summing to 1000", len(recs), recs, sum)
	}
	t.Logf("well-formed forgeries: %d line(s) for 1000 refusals: %v", len(recs), recs)

	// 3 — 1 000 requests with a valid key on a route that answers 429.
	// Ist: 2 000 lines (http request + model auth: accepted).
	send(1000, http.MethodPost, "/v1/chat/completions", model.Plaintext(), http.StatusTooManyRequests)
	if recs := sink.take(); len(recs) != 0 {
		t.Fatalf("429 flood: %d line(s) before the window ended (%d http request, %d model auth: accepted), want 0",
			len(recs), countMsg(recs, "http request"), countMsg(recs, "model auth: accepted"))
	}
	access.Flush()
	wantOneLine("429 flood", sink.take(), consumer.ID, "http_429", 1000)

	// 4 — successes: exactly one gateway request line each, with the
	// identity the door put into the request holder.
	send(50, http.MethodGet, "/v1/models", model.Plaintext(), http.StatusOK)
	recs = sink.take()
	lines := linesOf(recs, "gateway request")
	if len(lines) != 50 || len(recs) != 50 {
		t.Fatalf("successes: %d gateway request line(s) of %d line(s), want 50 of 50", len(lines), len(recs))
	}
	for _, l := range lines {
		if l["consumer_id"] != consumer.ID || l["model_key_id"] != mk.ID || l["tenant_id"] != store.DefaultTenantID ||
			l["status"] != int64(http.StatusOK) || l["request_id"] == "" {
			t.Fatalf("success line %v, want consumer %s, key %s, tenant %s, status 200, a request id", l, consumer.ID, mk.ID, store.DefaultTenantID)
		}
		for _, v := range l {
			if s, ok := v.(string); ok && strings.Contains(s, model.Plaintext()) {
				t.Fatalf("a success line carries the key: %v", l)
			}
		}
	}

	// 5 — gateway.access_log off: no success line at all.
	accessLog.Store(false)
	send(50, http.MethodGet, "/v1/models", model.Plaintext(), http.StatusOK)
	if recs := sink.take(); len(recs) != 0 {
		t.Fatalf("access_log=false: %d line(s), want 0: %v", len(recs), recs)
	}

	// 6 — gateway.enabled off: the stage sits before the gate, so the
	// flag's 404 is counted too, without a consumer.
	enabled.Store(false)
	send(1000, http.MethodGet, "/v1/models", model.Plaintext(), http.StatusNotFound)
	access.Flush()
	wantOneLine("flag off", sink.take(), "", "http_404", 1000)

	// 7 — a route outside the group keeps its http request line.
	send(1, http.MethodGet, "/health", "", http.StatusOK)
	if recs := sink.take(); len(recs) != 1 || countMsg(recs, "http request") != 1 {
		t.Fatalf("/health: %v, want one http request line", recs)
	}
	if n := door.Stats().Accepted; n != 1100 {
		t.Errorf("door accepted %d, want 1100 (1000 + 50 + 50)", n)
	}
}
