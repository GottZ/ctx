package dream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
)

// Regression: dream.chatJSON must follow dream.Protocol, not
// llm.DefaultProtocol. Before the fix, a split chat/dream protocol
// configuration silently sent dream traffic over the chat protocol (W49).
func TestChatJSONFollowsDreamProtocol(t *testing.T) {
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

	origProto := Protocol
	origDefault := llm.DefaultProtocol
	defer func() {
		Protocol = origProto
		llm.DefaultProtocol = origDefault
	}()

	// Adversarial setup: DefaultProtocol deliberately contradicts
	// dream.Protocol so following the wrong one is visible.
	llm.DefaultProtocol = "ollama"
	Protocol = "openai"
	if _, err := chatJSON(context.Background(), srv.URL, "", "m", nil, "sys", "user", llm.Options{}, 5*time.Second); err != nil {
		t.Fatalf("chatJSON openai: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("Protocol=openai: got path %q, want /v1/chat/completions (followed DefaultProtocol?)", gotPath)
	}

	llm.DefaultProtocol = "openai"
	Protocol = "ollama"
	if _, err := chatJSON(context.Background(), srv.URL, "", "m", nil, "sys", "user", llm.Options{}, 5*time.Second); err != nil {
		t.Fatalf("chatJSON ollama: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Errorf("Protocol=ollama: got path %q, want /api/chat (followed DefaultProtocol?)", gotPath)
	}
}
