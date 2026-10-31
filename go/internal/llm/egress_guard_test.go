package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// newLoopbackWire answers every chat wire on 127.0.0.1 and counts the
// requests that reached it.
func newLoopbackWire(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case r.URL.Path == "/api/chat":
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"done":true}`)
		case r.Header.Get("Accept") == "text/event-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// assertGuardRefusal pins the refusal shape: the request never left the
// process, the error names the SSRF guard, and the failover classifier sees
// a transport failure (next backend, cooldown) — not a server fault.
func assertGuardRefusal(t *testing.T, name string, err error, hits *atomic.Int64) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: tenant row reached loopback, want SSRF guard refusal", name)
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Errorf("%s: error is not the guard's: %v", name, err)
	}
	if c := backends.Classify(err, ""); c != backends.ClassTransport {
		t.Errorf("%s: Classify = %v, want transport", name, c)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%s: loopback server saw %d request(s), want 0", name, n)
	}
}

// TestTenantRowNeverDialsLoopback (GHSA 2026-10-31 §3): every chat wire —
// openai, ollama, anthropic, both streams — refuses to dial a loopback
// base_url for a tenant row, while a _global row on the same server works.
func TestTenantRowNeverDialsLoopback(t *testing.T) {
	srv, hits := newLoopbackWire(t)
	ctx := context.Background()
	row := func(scope string, p backends.Protocol) backends.Backend {
		return backends.Backend{Host: srv.URL, Scope: scope, Protocol: p, Model: "m", APIKey: "k"}
	}

	for _, p := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama, backends.ProtocolAnthropic} {
		_, err := Chat(ctx, row("acme", p), "s", "u", Options{}, 5*time.Second)
		assertGuardRefusal(t, "chat/"+string(p), err, hits)
	}
	for _, p := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolAnthropic} {
		_, err := ChatStreamBackend(ctx, row("acme", p), "m", []ChatMsg{{Role: "user", Content: "u"}}, nil, Options{}, 5*time.Second, nil)
		assertGuardRefusal(t, "stream/"+string(p), err, hits)
	}

	// Positive control: the same loopback server answers a _global row on
	// every wire that has a stub answer — the refusal above is the guard, not
	// the environment.
	for _, p := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama} {
		before := hits.Load()
		if _, err := Chat(ctx, row(backends.GlobalScope, p), "s", "u", Options{}, 5*time.Second); err != nil {
			t.Errorf("_global chat/%s: %v", p, err)
		}
		if hits.Load() != before+1 {
			t.Errorf("_global chat/%s did not reach the server", p)
		}
	}
	if _, err := ChatStreamBackend(ctx, row(backends.GlobalScope, backends.ProtocolOpenAI), "m",
		[]ChatMsg{{Role: "user", Content: "u"}}, nil, Options{}, 5*time.Second, nil); err != nil {
		t.Errorf("_global stream: %v", err)
	}
}
