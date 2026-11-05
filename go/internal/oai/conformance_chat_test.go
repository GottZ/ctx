package oai_test

// Conformance harness, Go half, for POST /v1/chat/completions (wave W02-07,
// gate G1; design/02 §4.6, §7): the official OpenAI Go SDK
// (client.Chat.Completions.New) against the chat route of the harness
// (newChatRig) over each of the three answer shapes the live chat chain
// serves — SGLang, llama.cpp, OpenRouter. The wire bytes of every answer are
// checked against the vendored CreateChatCompletionResponse (openai-openapi
// @ c2aecc2f, the same commit as the other excerpts); the SDK's typed answer
// against the rewrite contract: id = chatcmpl-<server-side UUIDv7> (never
// the client's X-Request-ID), model = the alias name, usage > 0, no
// reasoning trace in the message of a reasoning-off alias.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// chatResponseExcerpt is the vendored excerpt of the chat answer
// (CreateChatCompletionResponse and its transitive $ref closure, same commit
// as schemaExcerpt). The Python harness reads the same file.
const chatResponseExcerpt = "../../conformance/oai/testdata/openapi-c2aecc2f-create-chat-completion-response.json"

// cfClientRequestID is the client's own request id of the conformance
// calls: it must reach neither the answer's id nor the upstream (B16).
const cfClientRequestID = "deadbeef"

// cfChatShapes are the answer shapes of the three engine families
// (chat_harness_test.go).
var cfChatShapes = []struct{ name, answer string }{
	{"sglang", answerSGLang},
	{"llamacpp", answerLlamaCpp},
	{"openrouter", answerOpenRouter},
}

// cfWire serves h and keeps the bytes of the last answer to a chat
// completion, so the schema check runs on the wire.
type cfWire struct {
	mu   sync.Mutex
	last []byte
}

func (c *cfWire) body() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.last...)
}

func cfServe(t testing.TB, h http.Handler) (*httptest.Server, *cfWire) {
	t.Helper()
	wire := &cfWire{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &captureWriter{ResponseWriter: w}
		h.ServeHTTP(cw, r)
		if r.URL.Path == "/v1/chat/completions" {
			wire.mu.Lock()
			wire.last = cw.buf.Bytes()
			wire.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, wire
}

// cfChatViolations calls the route once through the Go SDK over a rig whose
// only row answers in shape answer and returns what a client would trip
// over — empty means conformant.
func cfChatViolations(t *testing.T, answer string) []string {
	t.Helper()
	schema := loadSchema(t, chatResponseExcerpt, "CreateChatCompletionResponse")
	up := newUpstream(t, answer)
	rig := newChatRig(t, []backends.Backend{up.row("conf", backends.TrustFull, 1)}, 4, 1)
	srv, wire := cfServe(t, rig.router)
	client := openai.NewClient(option.WithBaseURL(srv.URL+"/v1/"), option.WithAPIKey("ctxm_conformance"), option.WithMaxRetries(0))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "chat",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	}, option.WithHeader("X-Request-ID", cfClientRequestID))
	if err != nil {
		return append(out, fmt.Sprintf("Chat.Completions.New: %v", err))
	}
	if n := up.hits.Load(); n != 1 {
		bad("the upstream saw %d request(s), want 1", n)
	}
	suffix, ok := strings.CutPrefix(resp.ID, "chatcmpl-")
	switch u, perr := uuid.Parse(suffix); {
	case !ok:
		bad("id %q lacks the chatcmpl- prefix", resp.ID)
	case resp.ID == "chatcmpl-"+cfClientRequestID || strings.Contains(resp.ID, cfClientRequestID):
		bad("id %q carries the client's X-Request-ID", resp.ID)
	case perr != nil || u.Version() != 7:
		bad("id %q: suffix is no UUIDv7 (%v)", resp.ID, perr)
	}
	if resp.Model != "chat" {
		bad("model %q, want the alias name %q", resp.Model, "chat")
	}
	if resp.Object != "chat.completion" {
		bad("object %q, want chat.completion", resp.Object)
	}
	if resp.Usage.PromptTokens <= 0 || resp.Usage.CompletionTokens <= 0 || resp.Usage.TotalTokens <= 0 {
		bad("usage %+v, want every count > 0", resp.Usage)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from the engine" {
		bad("choices %+v, want the engine's one message", resp.Choices)
	} else if _, ok := resp.Choices[0].Message.JSON.ExtraFields["reasoning_content"]; ok {
		bad("the message carries reasoning_content (reasoning-off alias)")
	}
	body := wire.body()
	if bytes.Contains(body, []byte("reasoning_content")) || bytes.Contains(body, []byte("THINK-CANARY")) {
		bad("the wire body carries the reasoning trace: %s", body)
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return append(out, fmt.Sprintf("wire body is not JSON: %v (%q)", err, body))
	}
	if err := schema.Validate(v); err != nil {
		bad("wire body fails CreateChatCompletionResponse: %v (%s)", err, body)
	}
	return out
}

// TestConformanceGoSDKChat is G1 (Go half): every shape reaches the SDK as a
// conformant chat completion.
func TestConformanceGoSDKChat(t *testing.T) {
	for _, sh := range cfChatShapes {
		t.Run(sh.name, func(t *testing.T) {
			for _, v := range cfChatViolations(t, sh.answer) {
				t.Error(v)
			}
		})
	}
}

// TestConformanceChatSchemaHasTeeth is G1's control: the vendored schema
// refuses what is no chat completion — the route's own error envelope and
// a completion without the required id.
func TestConformanceChatSchemaHasTeeth(t *testing.T) {
	schema := loadSchema(t, chatResponseExcerpt, "CreateChatCompletionResponse")
	w := httptest.NewRecorder()
	oai.WriteInternal(w)
	noID := `{"object":"chat.completion","created":1,"model":"chat","choices":[{"index":0,"finish_reason":"stop","logprobs":null,` +
		`"message":{"role":"assistant","content":"x"}}]}`
	for name, raw := range map[string][]byte{"error envelope": w.Body.Bytes(), "no id": []byte(noID)} {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if schema.Validate(v) == nil {
			t.Errorf("%s passes CreateChatCompletionResponse — the schema check has no teeth", name)
		}
	}
}
