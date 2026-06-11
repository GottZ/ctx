package dream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

// Regression: a dream LLM call follows the Protocol of the backends.Backend
// passed to the entry points — openai selects /v1/chat/completions, ollama
// selects /api/chat. Historically (W49) dream traffic silently followed the
// chat protocol via llm.DefaultProtocol under a split chat/dream
// configuration; F1-W3 deleted that var and F1-W6 deleted the dream.Protocol
// package var, so both ambient-state halves of the old confusion class are
// structurally gone. What remains to pin is that the production path
// (dreamChatJSON with the chatJSON seam uninstalled) maps the tuple's
// Protocol to the wire path.
func TestDreamChatJSONFollowsBackendProtocol(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path == "/v1/chat/completions" {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{}"}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`)
			return
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"{}"},"eval_count":1,"prompt_eval_count":1}`)
	}))
	defer srv.Close()

	b := backends.Backend{Host: srv.URL, Model: "m", Protocol: backends.ProtocolOpenAI}
	if _, err := dreamChatJSON(context.Background(), b, "sys", "user", llm.Options{}, 5*time.Second); err != nil {
		t.Fatalf("dreamChatJSON openai: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("Protocol=openai: got path %q, want /v1/chat/completions", gotPath)
	}

	b.Protocol = backends.ProtocolOllama
	if _, err := dreamChatJSON(context.Background(), b, "sys", "user", llm.Options{}, 5*time.Second); err != nil {
		t.Fatalf("dreamChatJSON ollama: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Errorf("Protocol=ollama: got path %q, want /api/chat", gotPath)
	}
}

// Regression: an installed chatJSON test seam intercepts the call (no wire
// traffic) and receives the loose tuple of the SAME backend the entry point
// got — the seam contract every dream test file builds its overrides on.
func TestDreamChatJSONSeamReceivesBackendTuple(t *testing.T) {
	var gotHost, gotModel string
	saved := chatJSON
	chatJSON = func(_ context.Context, host, _, model string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		gotHost, gotModel = host, model
		return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: "{}"}}, nil
	}
	t.Cleanup(func() { chatJSON = saved })

	b := backends.Backend{Host: "http://backend.example", Model: "seam-model", Protocol: backends.ProtocolOpenAI}
	if _, err := dreamChatJSON(context.Background(), b, "sys", "user", llm.Options{}, time.Second); err != nil {
		t.Fatalf("dreamChatJSON via seam: %v", err)
	}
	if gotHost != b.Host || gotModel != b.Model {
		t.Errorf("seam got (host=%q, model=%q), want (%q, %q)", gotHost, gotModel, b.Host, b.Model)
	}
}
