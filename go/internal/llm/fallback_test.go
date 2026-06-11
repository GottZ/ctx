package llm

import (
	"context"
	"fmt"
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

// deadBackend returns a primary chat tuple pointing at a closed port.
func deadBackend(t *testing.T) backends.Backend {
	t.Helper()
	return backends.Backend{Host: deadURL(t), Protocol: backends.ProtocolOllama, Model: "m"}
}

func TestChatWithFallbackUsesFallbackWhenPrimaryDown(t *testing.T) {
	var fbHits atomic.Int64
	fb := httptest.NewServer(ollamaOK("from-fallback", &fbHits))
	defer fb.Close()
	fallback := &backends.Backend{Host: fb.URL, Protocol: backends.ProtocolOllama, Timeout: 5 * time.Second}

	resp, used, err := chatWithFallback(context.Background(), deadBackend(t), fallback, "sys", "user", Options{}, 2*time.Second)
	if err != nil {
		t.Fatalf("want fallback success, got %v", err)
	}
	if !used {
		t.Error("usedFallback = false, want true")
	}
	if resp.Message.Content != "from-fallback" {
		t.Errorf("content = %q, want from-fallback", resp.Message.Content)
	}
	if fbHits.Load() != 1 {
		t.Errorf("fallback hits = %d, want 1", fbHits.Load())
	}
}

func TestChatWithFallbackErrorsWhenNoFallbackConfigured(t *testing.T) {
	// nil = no fallback (config.ChatFallbackBackend with empty host) and an
	// empty-Host tuple must behave identically: primary error passes through.
	for name, fallback := range map[string]*backends.Backend{"nil": nil, "empty-host": {}} {
		_, used, err := chatWithFallback(context.Background(), deadBackend(t), fallback, "sys", "user", Options{}, 2*time.Second)
		if err == nil {
			t.Fatalf("%s: want error when primary down and no fallback configured", name)
		}
		if used {
			t.Errorf("%s: usedFallback = true, want false", name)
		}
	}
}

func TestChatWithFallbackSkipsFallbackOnHTTPError(t *testing.T) {
	// Primary is alive but answers 500 — the server made a statement, a slower
	// fallback must not second-guess it.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer primary.Close()

	var fbHits atomic.Int64
	fb := httptest.NewServer(ollamaOK("nope", &fbHits))
	defer fb.Close()
	fallback := &backends.Backend{Host: fb.URL, Protocol: backends.ProtocolOllama, Timeout: 5 * time.Second}

	primaryB := backends.Backend{Host: primary.URL, Protocol: backends.ProtocolOllama, Model: "m"}
	_, used, err := chatWithFallback(context.Background(), primaryB, fallback, "sys", "user", Options{}, 2*time.Second)
	if err == nil {
		t.Fatal("want HTTP 500 error passed through")
	}
	if used || fbHits.Load() != 0 {
		t.Errorf("fallback consulted on HTTP error (used=%v hits=%d), want untouched", used, fbHits.Load())
	}
}
