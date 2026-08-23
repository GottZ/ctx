package dream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
// (dreamChat with the chatJSON seam uninstalled) maps the tuple's Protocol to
// the wire path.
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
	if _, err := dreamChat(context.Background(), b, "sys", "user", llm.Options{}, 5*time.Second, true); err != nil {
		t.Fatalf("dreamChat openai: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("Protocol=openai: got path %q, want /v1/chat/completions", gotPath)
	}

	b.Protocol = backends.ProtocolOllama
	if _, err := dreamChat(context.Background(), b, "sys", "user", llm.Options{}, 5*time.Second, true); err != nil {
		t.Fatalf("dreamChat ollama: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Errorf("Protocol=ollama: got path %q, want /api/chat", gotPath)
	}
}

// recordWireBody starts a dual-protocol backend that answers both dialects and
// hands back the raw request BODY of the last call. The seam stays
// UNINSTALLED for every test using it — an installed chatJSON never reaches a
// wire, so it can say nothing about what the request carried.
func recordWireBody(t *testing.T, body *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		*body = string(raw)
		if r.URL.Path == "/v1/chat/completions" {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"prose"}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`)
			return
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"prose"},"eval_count":1,"prompt_eval_count":1}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jsonModeMarker is the dialect's JSON-mode field on the request body:
// Ollama's top-level "format":"json", OpenAI's response_format object.
func jsonModeMarker(p backends.Protocol) string {
	if p == backends.ProtocolOpenAI {
		return `"response_format"`
	}
	return `"format":"json"`
}

// TestDreamChatJSONModeOnTheWire pins the per-call jsonMode parameter in both
// dialects: it is the only observable difference between the two call classes
// of the pipeline, and it is invisible to every seam-based test in the package.
// Hardwiring dreamChat back to llm.ChatJSON turns the two "plain" rows red.
func TestDreamChatJSONModeOnTheWire(t *testing.T) {
	var body string
	srv := recordWireBody(t, &body)

	for _, proto := range []backends.Protocol{backends.ProtocolOllama, backends.ProtocolOpenAI} {
		for _, jsonMode := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/jsonMode=%v", proto, jsonMode), func(t *testing.T) {
				body = ""
				b := backends.Backend{Host: srv.URL, Model: "m", Protocol: proto}
				if _, err := dreamChat(context.Background(), b, "sys", "user", llm.Options{}, 5*time.Second, jsonMode); err != nil {
					t.Fatalf("dreamChat: %v", err)
				}
				marker := jsonModeMarker(proto)
				if got := strings.Contains(body, marker); got != jsonMode {
					t.Errorf("request body carries %s = %v, want %v\nbody: %s", marker, got, jsonMode, body)
				}
			})
		}
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
	if _, err := dreamChat(context.Background(), b, "sys", "user", llm.Options{}, time.Second, true); err != nil {
		t.Fatalf("dreamChat via seam: %v", err)
	}
	if gotHost != b.Host || gotModel != b.Model {
		t.Errorf("seam got (host=%q, model=%q), want (%q, %q)", gotHost, gotModel, b.Host, b.Model)
	}
}
