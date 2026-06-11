package rrf

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// LLM-judge call site: Rerank must hit the endpoint of the chat tuple's
// protocol (/api/chat vs /v1/chat/completions). The judge is fail-open — on a
// wrong wire path the LLM error is swallowed and the pre-rerank order is
// silently kept, so the PATH assertion (not the returned order or error) is
// what catches a call site reading the wrong protocol field (F1 risk 1).
func TestRerankWirePath(t *testing.T) {
	cases := []struct {
		name     string
		protocol backends.Protocol
		wantPath string
	}{
		{"ollama", backends.ProtocolOllama, "/api/chat"},
		{"openai", backends.ProtocolOpenAI, "/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPath = r.URL.Path
				mu.Unlock()
				if r.URL.Path == "/v1/chat/completions" {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"[10, 5, 1]"}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`)
					return
				}
				fmt.Fprint(w, `{"message":{"role":"assistant","content":"[10, 5, 1]"},"eval_count":1,"prompt_eval_count":1}`)
			}))
			defer srv.Close()

			chat := backends.Backend{Host: srv.URL, Protocol: tc.protocol, Model: "m"}
			results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
			out, err := Rerank(context.Background(), chat, "q", results)
			if err != nil {
				t.Fatalf("Rerank: %v", err)
			}
			// Scores [10,5,1] parsed = the judge leg really ran (not fail-open).
			if out[0].RerankScore == nil {
				t.Error("RerankScore not set — judge response was not consumed")
			}
			mu.Lock()
			defer mu.Unlock()
			if gotPath != tc.wantPath {
				t.Errorf("wire path = %q, want %q", gotPath, tc.wantPath)
			}
		})
	}
}
