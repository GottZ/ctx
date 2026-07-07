package rrf

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
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

			// F3-P3: the judge resolves over Chain("synthesis", …) — seed a
			// single-backend pool with the test server as the synthesis row.
			bpool := backends.NewPool(nil, nil)
			bpool.SeedSnapshotForTest([]backends.Backend{{
				ID: "judge", Name: "judge", Host: srv.URL, Protocol: tc.protocol,
				Trust: backends.TrustFull, Enabled: true, Priority: 1,
				Roles:    []string{backends.RoleSynthesis},
				ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
			}})
			results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
			out, err := Rerank(context.Background(), nil, bpool, backends.SensPublic, "q", results, "", rerankTestAdmission(t))
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

// rerankTestAdmission is the MW3 pass-through admission for the judge call
// site (interactive, design/01 §4.6 N1 rerank-judge). The principal is
// ctx-derived since MW4 (design/03 §4.1.1): the test installs the hook with
// a fixed caller so the interactive class never runs into the B8 downgrade
// (pattern: blocktype.SetRequestScopeHook in registry_t12_test.go).
func rerankTestAdmission(t *testing.T) llm.Admission {
	t.Helper()
	dispatch.SetPrincipalHook(func(context.Context) dispatch.Principal {
		return dispatch.Principal{ApiKeyID: "test-key", HomeScope: "private"}
	})
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return llm.Admission{Admitter: d, Class: dispatch.ClassInteractive}
}
