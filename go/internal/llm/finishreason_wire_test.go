// Stop-reason decode on the NON-streaming chat path (issue #26 commit 1).
// Both wire formats report one, under different names, and both may omit it.
// The probes run over the real decode path against httptest servers — a
// struct-literal assertion would pass even if the json tag were wrong, which
// is the only failure mode that can actually happen here.
package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// finishSrv answers every request with respJSON, whatever the path.
func finishSrv(t *testing.T, respJSON string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(respJSON))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestChatDecodesFinishReason walks both protocols with and without the stop
// reason on the wire. The negative rows are the load-bearing half: a provider
// that reports nothing must yield "" (unknown), never a fabricated "stop", and
// the sibling fields must survive the added decode.
func TestChatDecodesFinishReason(t *testing.T) {
	const (
		ollamaWithReason = `{"message":{"role":"assistant","content":"hi"},
			"eval_count":600,"prompt_eval_count":120,"done_reason":"length"}`
		ollamaNoReason = `{"message":{"role":"assistant","content":"hi"},
			"eval_count":600,"prompt_eval_count":120}`
		openAIWithReason = `{"choices":[{"message":{"role":"assistant","content":"hi"},
			"finish_reason":"length"}],
			"usage":{"completion_tokens":600,"prompt_tokens":120}}`
		openAINoReason = `{"choices":[{"message":{"role":"assistant","content":"hi"}}],
			"usage":{"completion_tokens":600,"prompt_tokens":120}}`
	)

	tests := []struct {
		name     string
		protocol backends.Protocol
		respJSON string
		want     string
	}{
		{"ollama-length", backends.ProtocolOllama, ollamaWithReason, "length"},
		{"ollama-absent", backends.ProtocolOllama, ollamaNoReason, ""},
		{"openai-length", backends.ProtocolOpenAI, openAIWithReason, "length"},
		{"openai-absent", backends.ProtocolOpenAI, openAINoReason, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := backends.Backend{
				Host: finishSrv(t, tt.respJSON), Protocol: tt.protocol,
				Model: "m", Trust: backends.TrustFull,
			}
			resp, err := ChatJSON(context.Background(), b, "sys", "usr",
				Options{NumPredict: 600}, 5*time.Second)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.FinishReason != tt.want {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, tt.want)
			}
			// The decode must not disturb what the path already carried.
			if resp.Message.Content != "hi" {
				t.Errorf("content = %q, want %q", resp.Message.Content, "hi")
			}
			if resp.Message.Role != "assistant" {
				t.Errorf("role = %q, want %q", resp.Message.Role, "assistant")
			}
			if resp.EvalCount != 600 {
				t.Errorf("EvalCount = %d, want 600", resp.EvalCount)
			}
			if resp.PromptTokens != 120 {
				t.Errorf("PromptTokens = %d, want 120", resp.PromptTokens)
			}
		})
	}
}

// TestChatFinishReasonStaysRaw pins the deliberate non-normalisation: an
// OpenAI-dialect provider that answers with its own vocabulary (here
// OpenRouter's tool_calls, and llama.cpp's eos flavour of a natural end) is
// handed through verbatim. Folding vocabularies into one another here would
// destroy the only evidence of which provider said what.
func TestChatFinishReasonStaysRaw(t *testing.T) {
	for _, want := range []string{"tool_calls", "content_filter", "eos"} {
		t.Run(want, func(t *testing.T) {
			body := `{"choices":[{"message":{"role":"assistant","content":"x"},
				"finish_reason":"` + want + `"}],"usage":{"completion_tokens":3}}`
			b := backends.Backend{
				Host: finishSrv(t, body), Protocol: backends.ProtocolOpenAI,
				Model: "m", Trust: backends.TrustFull,
			}
			resp, err := Chat(context.Background(), b, "sys", "usr", Options{}, 5*time.Second)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.FinishReason != want {
				t.Errorf("FinishReason = %q, want the raw %q", resp.FinishReason, want)
			}
		})
	}
}
