// Pure unit tests for the chat harness helpers — no DB, no LLM, runs under
// `go test -short`. The loop + tool execution against a real DB and a fake LLM
// server live in chat_integration_test.go.
package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

func TestWindowContent(t *testing.T) {
	body := strings.Repeat("a", 100)
	t.Run("truncates and reports next offset", func(t *testing.T) {
		w, trunc, next := windowContent(body, 0, 50)
		if !trunc || next != 50 {
			t.Fatalf("truncated=%v next=%d; want true/50", trunc, next)
		}
		if !strings.HasPrefix(w, strings.Repeat("a", 50)) || !strings.Contains(w, "offset=50") {
			t.Fatalf("window = %q; want 50 a's + offset marker", w)
		}
	})
	t.Run("offset reads the tail without truncation", func(t *testing.T) {
		w, trunc, next := windowContent(body, 50, 50)
		if trunc || next != 0 || w != strings.Repeat("a", 50) {
			t.Fatalf("window=%q trunc=%v next=%d; want 50 a's/false/0", w, trunc, next)
		}
	})
	t.Run("window larger than content returns all", func(t *testing.T) {
		w, trunc, _ := windowContent(body, 0, 500)
		if trunc || w != body {
			t.Fatalf("trunc=%v; want full untruncated content", trunc)
		}
	})
	t.Run("offset past end is empty", func(t *testing.T) {
		w, trunc, _ := windowContent(body, 200, 50)
		if w != "" || trunc {
			t.Fatalf("window=%q trunc=%v; want empty/false", w, trunc)
		}
	})
	t.Run("rune-safe on multibyte content", func(t *testing.T) {
		mb := strings.Repeat("ä", 100) // 1 rune / 2 bytes each
		w, trunc, next := windowContent(mb, 0, 50)
		if !trunc || next != 50 {
			t.Fatalf("trunc=%v next=%d; want true/50", trunc, next)
		}
		head := strings.TrimSuffix(strings.SplitN(w, "\n", 2)[0], "")
		if !strings.HasPrefix(head, strings.Repeat("ä", 50)) {
			t.Fatalf("multibyte window split a rune: %q", head)
		}
	})
}

func TestBuildHistoryBudget(t *testing.T) {
	e := NewEngine(nil, nil, nil, Config{HistoryBudgetChars: 120}, nil)
	msgs := []store.ChatMessage{
		{Role: "user", Content: strings.Repeat("u", 50)},
		{Role: "assistant", Content: strings.Repeat("a", 50)},
		{Role: "tool", Content: strings.Repeat("t", 1000), ToolCallID: "c1"},
		{Role: "user", Content: strings.Repeat("z", 30)},
	}
	out := e.buildHistory(msgs)
	if len(out) == 0 {
		t.Fatal("history empty")
	}
	if out[0].Role != "user" {
		t.Fatalf("history must start on a user message, got %q (orphaned tool/assistant head)", out[0].Role)
	}
	if last := out[len(out)-1]; last.Content != strings.Repeat("z", 30) {
		t.Fatalf("current user message must survive intact, got %q", last.Content)
	}
	if total := totalChars(out); total > 120 {
		t.Fatalf("history %d chars exceeds budget 120", total)
	}
}

func TestToWireRoundtripsToolCalls(t *testing.T) {
	tcs := []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "ctx_query", Arguments: json.RawMessage(`{"query":"x"}`)}}}
	raw := marshalToolCalls(tcs)
	if raw == nil {
		t.Fatal("marshalToolCalls returned nil")
	}
	w := toWire(store.ChatMessage{Role: "assistant", ToolCalls: raw})
	if len(w.ToolCalls) != 1 || w.ToolCalls[0].Function.Name != "ctx_query" {
		t.Fatalf("roundtrip lost the tool call: %+v", w.ToolCalls)
	}
}

func TestBackendMaxTokens(t *testing.T) {
	cases := []struct {
		name   string
		limits map[string]any
		want   int
	}{
		{"absent", nil, 0},
		{"json float", map[string]any{"chat_max_tokens": float64(512)}, 512},
		{"int", map[string]any{"chat_max_tokens": 512}, 512},
		{"other key ignored", map[string]any{"foo": 1.0}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := backendMaxTokens(backends.Backend{Limits: c.limits}); got != c.want {
				t.Errorf("backendMaxTokens = %d; want %d", got, c.want)
			}
		})
	}
}

func TestTokenClampPrefersSmallest(t *testing.T) {
	e := NewEngine(nil, nil, nil, Config{MaxTokens: 2048}, nil)
	// Backend CPU clamp 512 beats the global 2048; budget 300 beats both.
	cpu := backends.Backend{Limits: map[string]any{"chat_max_tokens": float64(512)}}
	if got := e.tokenClamp(cpu, 8192); got != 512 {
		t.Errorf("clamp with backend 512 = %d; want 512", got)
	}
	if got := e.tokenClamp(cpu, 300); got != 300 {
		t.Errorf("clamp with budget 300 = %d; want 300", got)
	}
	if got := e.tokenClamp(backends.Backend{}, 8192); got != 2048 {
		t.Errorf("clamp with no backend limit = %d; want 2048 (global)", got)
	}
}

func TestNormalizeSensFailClosed(t *testing.T) {
	for _, in := range []backends.Sensitivity{"", "bogus"} {
		if got := normalizeSens(in); got != backends.SensCredentials {
			t.Errorf("normalizeSens(%q) = %q; want credentials (fail-closed)", in, got)
		}
	}
	if got := normalizeSens(backends.SensInternal); got != backends.SensInternal {
		t.Errorf("normalizeSens(internal) = %q; want internal", got)
	}
}

func TestToolDefsAreValidJSON(t *testing.T) {
	names := map[string]bool{}
	for _, d := range toolDefs {
		if d.Type != "function" {
			t.Errorf("tool %q type = %q; want function", d.Function.Name, d.Type)
		}
		var schema map[string]any
		if err := json.Unmarshal(d.Function.Parameters, &schema); err != nil {
			t.Errorf("tool %q parameters not valid JSON: %v", d.Function.Name, err)
		}
		names[d.Function.Name] = true
	}
	for _, want := range []string{"ctx_query", "ctx_search", "ctx_get", "ctx_recent"} {
		if !names[want] {
			t.Errorf("missing tool def %q", want)
		}
	}
}

func TestErrOutcomeIsPublicAndNotOK(t *testing.T) {
	o := errOutcome("block not found")
	if o.OK || o.Sensitivity != backends.SensPublic {
		t.Fatalf("errOutcome ok=%v sens=%q; want false/public (no content to protect)", o.OK, o.Sensitivity)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(o.Content), &m); err != nil || m["error"] == "" {
		t.Fatalf("errOutcome content = %q; want {\"error\":…}", o.Content)
	}
}
