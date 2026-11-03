package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/ssrfguard"
)

// F-W1 Gate (a), chat wires (design/04 I17, S5): a backend that answers
// 307 with a Location on another origin must not hand the prompt to that
// origin. Every chat wire — ollama, openai, anthropic, both streams — on both
// pool clients (a _global row: open; a tenant row: guarded) returns the 307
// as a StatusError, which Classify maps to ClassBadRequest, and the foreign
// host sees nothing.

const redirectPromptMarker = "PROMPT-MARKER-F-W1"

// foreignWire answers every chat wire like a real backend would and records
// what reached it: in the red run it is the proof of where the prompt went.
type foreignWire struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newForeignWire(t *testing.T) *foreignWire {
	t.Helper()
	f := &foreignWire{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, fmt.Sprintf("%s %s x-api-key=%q authorization=%q body=%s",
			r.Method, r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), b))
		f.mu.Unlock()
		stream := r.Header.Get("Accept") == "text/event-stream"
		switch {
		case r.URL.Path == "/api/chat":
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"done":true}`)
		case r.URL.Path == "/v1/messages" && stream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":3}}}\n\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
				"data: {\"type\":\"message_stop\"}\n\n")
		case r.URL.Path == "/v1/messages":
			_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
		case stream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *foreignWire) hits() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen), strings.Join(f.seen, " | ")
}

// newRedirectingOrigin answers every request with 307 to the same path on
// target — the redirect a misconfigured or hostile backend would send.
func newRedirectingOrigin(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", target+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// admitGuardedLoopback lets this package's guarded client dial exactly the
// given loopback listeners — stand-ins for two PUBLIC hosts, which ssrfguard
// admits by design — and delegates every other address to the real
// ssrfguard.DialControl. Only the transport is swapped: the client's
// CheckRedirect, the code under test, stays the production one.
func admitGuardedLoopback(t *testing.T, srvs ...*httptest.Server) {
	t.Helper()
	g := httpClients.For(true)
	orig := g.Transport
	allow := map[string]bool{}
	for _, s := range srvs {
		allow[s.Listener.Addr().String()] = true
	}
	tr := orig.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(network, address string, c syscall.RawConn) error {
		if allow[address] {
			return nil
		}
		return ssrfguard.DialControl(network, address, c)
	}}
	tr.DialContext = d.DialContext
	g.Transport = tr
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		g.Transport = orig
	})
}

// assertRedirectRefused pins the refusal shape on one wire.
func assertRedirectRefused(t *testing.T, err error, foreign *foreignWire) {
	t.Helper()
	if n, seen := foreign.hits(); n != 0 {
		t.Errorf("the foreign host saw %d request(s): %s", n, seen)
	}
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want StatusError{307}", err)
	}
	if c := backends.Classify(err, ""); c != backends.ClassBadRequest {
		t.Errorf("Classify = %v, want bad_request", c)
	}
}

func TestChatWiresRefuseCrossOriginRedirect(t *testing.T) {
	ctx := context.Background()
	user := "user says " + redirectPromptMarker
	msgs := []ChatMsg{{Role: "user", Content: user}}
	wires := []struct {
		name string
		p    backends.Protocol
		call func(b backends.Backend) error
	}{
		{"chat/ollama", backends.ProtocolOllama, func(b backends.Backend) error {
			_, err := Chat(ctx, b, "s", user, Options{NumPredict: 8}, 5*time.Second)
			return err
		}},
		{"chat/openai", backends.ProtocolOpenAI, func(b backends.Backend) error {
			_, err := Chat(ctx, b, "s", user, Options{NumPredict: 8}, 5*time.Second)
			return err
		}},
		{"chat/anthropic", backends.ProtocolAnthropic, func(b backends.Backend) error {
			_, err := Chat(ctx, b, "s", user, Options{NumPredict: 8}, 5*time.Second)
			return err
		}},
		{"stream/openai", backends.ProtocolOpenAI, func(b backends.Backend) error {
			_, err := ChatStreamBackend(ctx, b, "m", msgs, nil, Options{NumPredict: 8}, 5*time.Second, nil)
			return err
		}},
		{"stream/anthropic", backends.ProtocolAnthropic, func(b backends.Backend) error {
			_, err := ChatStreamBackend(ctx, b, "m", msgs, nil, Options{NumPredict: 8}, 5*time.Second, nil)
			return err
		}},
	}
	for _, scope := range []string{backends.GlobalScope, "acme"} {
		client := "open"
		if scope != backends.GlobalScope {
			client = "guarded"
		}
		for _, w := range wires {
			t.Run(client+"/"+w.name, func(t *testing.T) {
				foreign := newForeignWire(t)
				origin := newRedirectingOrigin(t, foreign.srv.URL)
				if scope != backends.GlobalScope {
					admitGuardedLoopback(t, origin, foreign.srv)
				}
				b := backends.Backend{Host: origin.URL, Scope: scope, Protocol: w.p, Model: "m", APIKey: "row-key"}
				assertRedirectRefused(t, w.call(b), foreign)
			})
		}
	}
}
