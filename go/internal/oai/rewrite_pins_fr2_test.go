package oai_test

// Pins of Fremd-Review 2 F2-4 (Härtung W02-07) on the answer rewrite
// (design/02 §4.6): created and object are ctx's (W3, W5), and a
// reasoning-off alias drops every reasoning trace that carries text (W8, W9;
// the shapes of chat_harness_test.go carry reasoning and reasoning_details
// only as null).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// answerForeignHeader is an engine answer whose created and object are not
// what the client gets.
const answerForeignHeader = `{"id":"ID-CANARY","object":"OBJECT-CANARY","created":42,"model":"MODEL-CANARY",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from the engine"},"logprobs":null,"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`

// answerReasoningText carries the three reasoning traces WITH text: SGLang/
// vLLM reasoning_content, OpenRouter reasoning and reasoning_details.
const answerReasoningText = `{"id":"ID-CANARY","object":"chat.completion","created":1700000000,"model":"MODEL-CANARY",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from the engine",` +
	`"reasoning_content":"RC-TEXT-CANARY","reasoning":"R-TEXT-CANARY",` +
	`"reasoning_details":[{"type":"reasoning.text","text":"RD-TEXT-CANARY","format":"unknown","index":0}]},` +
	`"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`

// F2-4 W3/W5: created is the admission time (Unix seconds), never the
// upstream's value; object is chat.completion, never the upstream's.
func TestAnswerCreatedAndObjectAreCtxs(t *testing.T) {
	u := newUpstream(t, answerForeignHeader)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	before := time.Now().Unix()
	a := r.post(t, chatBody("chat"))
	after := time.Now().Unix()
	if a.status != http.StatusOK {
		t.Fatalf("status %d: %s", a.status, a.body)
	}
	var got struct {
		Object  string `json:"object"`
		Created int64  `json:"created"`
	}
	if err := json.Unmarshal(a.body, &got); err != nil {
		t.Fatalf("answer: %v: %s", err, a.body)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion (the upstream sent OBJECT-CANARY)", got.Object)
	}
	if got.Created < before || got.Created > after {
		t.Errorf("created = %d, want the admission time in [%d, %d] (the upstream sent 42)", got.Created, before, after)
	}
}

// pinReasoningAliases adds a _global chat alias "think" with reasoning
// passthrough to the default fixture.
func pinReasoningAliases() []chatAlias {
	return append(chatAliases(), chatAlias{"00000000-0000-4000-8000-0000000c4a7e", backends.GlobalScope, "think", "chat", "chat",
		backends.SensCredentials, `{"reasoning":"passthrough"}`, true})
}

// pinMessage is the message of the answer's first choice.
func pinMessage(t *testing.T, a chatAnswer) map[string]json.RawMessage {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("status %d: %s", a.status, a.body)
	}
	var got struct {
		Choices []struct {
			Message map[string]json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(a.body, &got); err != nil || len(got.Choices) != 1 {
		t.Fatalf("answer %s: %v, want one choice", a.body, err)
	}
	return got.Choices[0].Message
}

// F2-4 W8/W9: reasoning off (the default) drops reasoning_content,
// reasoning and reasoning_details with their text; reasoning passthrough
// keeps all three verbatim (control).
func TestReasoningTracesWithTextFollowThePolicy(t *testing.T) {
	keys := []string{"reasoning_content", "reasoning", "reasoning_details"}
	u := newUpstream(t, answerReasoningText)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	r.reg.reload(t, pinReasoningAliases())
	t.Run("off", func(t *testing.T) {
		a := r.post(t, chatBody("chat"))
		msg := pinMessage(t, a)
		for _, k := range keys {
			if v, ok := msg[k]; ok {
				t.Errorf("reasoning off: message carries %s = %s", k, v)
			}
		}
		for _, c := range []string{"RC-TEXT-CANARY", "R-TEXT-CANARY", "RD-TEXT-CANARY"} {
			if strings.Contains(string(a.body), c) {
				t.Errorf("reasoning off: the answer carries %q: %s", c, a.body)
			}
		}
		if string(msg["content"]) != `"hello from the engine"` {
			t.Errorf("content = %s, want the engine's", msg["content"])
		}
	})
	t.Run("passthrough", func(t *testing.T) {
		msg := pinMessage(t, r.post(t, chatBody("think")))
		want := map[string]string{
			"reasoning_content": `"RC-TEXT-CANARY"`,
			"reasoning":         `"R-TEXT-CANARY"`,
			"reasoning_details": `[{"type":"reasoning.text","text":"RD-TEXT-CANARY","format":"unknown","index":0}]`,
		}
		for _, k := range keys {
			if got := string(msg[k]); got != want[k] {
				t.Errorf("reasoning passthrough: %s = %s, want %s", k, got, want[k])
			}
		}
	})
}
