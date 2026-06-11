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

// Regression: dream.chatJSON must follow dream.Protocol — openai selects
// /v1/chat/completions, ollama selects /api/chat. Historically (W49) dream
// traffic silently followed the chat protocol via llm.DefaultProtocol under a
// split chat/dream configuration; F1-W3 deleted that var, so the ambient-state
// half of the old adversarial setup is structurally gone. What remains to pin
// is the Protocol→wire-path mapping until F1-W6 moves the protocol into the
// backends.Backend parameter of the dream entry points (this test then
// asserts against that tuple instead).
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
	defer func() { Protocol = origProto }()

	Protocol = "openai"
	if _, err := chatJSON(context.Background(), srv.URL, "", "m", nil, "sys", "user", llm.Options{}, 5*time.Second); err != nil {
		t.Fatalf("chatJSON openai: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("Protocol=openai: got path %q, want /v1/chat/completions", gotPath)
	}

	Protocol = "ollama"
	if _, err := chatJSON(context.Background(), srv.URL, "", "m", nil, "sys", "user", llm.Options{}, 5*time.Second); err != nil {
		t.Fatalf("chatJSON ollama: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Errorf("Protocol=ollama: got path %q, want /api/chat", gotPath)
	}
}
