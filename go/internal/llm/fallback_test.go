package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

func ollamaOK(marker string, hits *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprintf(w, `{"message":{"role":"assistant","content":"%s"},"eval_count":1,"prompt_eval_count":1}`, marker)
	}
}

// deadURL returns a URL whose port is guaranteed closed (dial refused).
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

func chainBackend(id, name, url string) backends.Backend {
	return backends.Backend{
		ID: id, Name: name, Host: url, Protocol: backends.ProtocolOllama,
		Model: "m", Trust: backends.TrustFull, Enabled: true,
		Roles: []string{backends.RoleSynthesis},
	}
}

// TestChatChainFailsOverOnTransport replays the historical fallback
// semantics against the chain: primary dial-refused ⇒ next backend serves.
// Provenance: the SERVED backend comes back (the pre-pool code logged
// host=primary even on fallback — red against that).
func TestChatChainFailsOverOnTransport(t *testing.T) {
	var fbHits atomic.Int64
	fb := httptest.NewServer(ollamaOK("from-fallback", &fbHits))
	defer fb.Close()

	chain := []backends.Backend{
		chainBackend("p", "primary", deadURL(t)),
		chainBackend("f", "fallback", fb.URL),
	}
	resp, served, attempts, err := ChatChain(context.Background(), chain,
		backends.RoleSynthesis, "sys", "user", Options{}, "", nil)
	if err != nil {
		t.Fatalf("want fallback success, got %v", err)
	}
	if served == nil || served.Name != "fallback" {
		t.Fatalf("provenance = %v, want fallback", served)
	}
	if resp.Message.Content != "from-fallback" {
		t.Errorf("content = %q", resp.Message.Content)
	}
	if len(attempts) != 2 || attempts[0].Class != "transport" || attempts[1].Class != "ok" {
		t.Errorf("attempts = %+v, want [transport ok]", attempts)
	}
}

// TestChatChainStopsOn500 is the doctrine anchor (pre-pool negative test
// preserved): the server RAN the request and failed at it — a slower next
// backend must not second-guess a deterministic per-prompt failure.
func TestChatChainStopsOn500(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer primary.Close()

	var fbHits atomic.Int64
	fb := httptest.NewServer(ollamaOK("nope", &fbHits))
	defer fb.Close()

	chain := []backends.Backend{
		chainBackend("p", "primary", primary.URL),
		chainBackend("f", "fallback", fb.URL),
	}
	_, served, attempts, err := ChatChain(context.Background(), chain,
		backends.RoleSynthesis, "sys", "user", Options{}, "", nil)
	if err == nil {
		t.Fatal("want HTTP 500 error passed through")
	}
	if served != nil || fbHits.Load() != 0 {
		t.Errorf("next backend consulted on HTTP 500 (served=%v hits=%d), want untouched", served, fbHits.Load())
	}
	if len(attempts) != 1 || attempts[0].Class != "server_fault" {
		t.Errorf("attempts = %+v, want [server_fault]", attempts)
	}
}

// TestChatChain502GoesNext documents the DELIBERATE semantic extension over
// the old chatWithFallback (which never escalated any HTTP status):
// 502/503/504 mean "infrastructure said no" — transient, try the next.
func TestChatChain502GoesNext(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer primary.Close()

	var fbHits atomic.Int64
	fb := httptest.NewServer(ollamaOK("served", &fbHits))
	defer fb.Close()

	chain := []backends.Backend{
		chainBackend("p", "primary", primary.URL),
		chainBackend("f", "fallback", fb.URL),
	}
	resp, served, _, err := ChatChain(context.Background(), chain,
		backends.RoleSynthesis, "sys", "user", Options{}, "", nil)
	if err != nil {
		t.Fatalf("want 502 escalation to succeed, got %v", err)
	}
	if served.Name != "fallback" || resp.Message.Content != "served" || fbHits.Load() != 1 {
		t.Errorf("502 did not escalate: served=%v hits=%d", served, fbHits.Load())
	}
}

// TestChatChainExhausted: every backend down ⇒ the last error surfaces,
// attempts carry the full walk, every failure is reported into health.
func TestChatChainExhausted(t *testing.T) {
	chain := []backends.Backend{
		chainBackend("a", "a", deadURL(t)),
		chainBackend("b", "b", deadURL(t)),
	}
	var reports atomic.Int64
	report := func(id string, class backends.ErrClass, _ time.Duration) {
		if class != backends.ClassOK {
			reports.Add(1)
		}
	}
	_, served, attempts, err := ChatChain(context.Background(), chain,
		backends.RoleSynthesis, "sys", "user", Options{}, "", report)
	if err == nil || served != nil {
		t.Fatal("want exhaustion error")
	}
	if len(attempts) != 2 || reports.Load() != 2 {
		t.Errorf("attempts=%d reports=%d, want 2/2", len(attempts), reports.Load())
	}
}

// TestChatChainParamsMerge: ModelSpec.Params override the code defaults and
// reach the ollama wire (options.top_p), think travels as params.think, the
// mapped model name wins over the F1 Model field.
func TestChatChainParamsMerge(t *testing.T) {
	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		gotBody.Store(string(buf))
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"ok"},"eval_count":1}`)
	}))
	defer srv.Close()

	b := chainBackend("x", "x", srv.URL)
	b.ModelMap = map[string]backends.ModelSpec{
		"default": {Model: "mapped-model", Params: map[string]any{"top_p": 0.8, "think": false}},
	}
	_, _, _, err := ChatChain(context.Background(), []backends.Backend{b},
		backends.RoleSynthesis, "sys", "user", Options{Temperature: 0.1}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := gotBody.Load().(string)
	for _, want := range []string{`"model":"mapped-model"`, `"top_p":0.8`, `"think":false`} {
		if !hasSub(body, want) {
			t.Errorf("wire body lacks %s: %s", want, body)
		}
	}
}

func hasSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
