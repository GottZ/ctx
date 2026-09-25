package chat

import (
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

// The web-chat stream path used to copy only temperature/top_p out of the
// chat role's model_map params; everything else (chat_template_kwargs, the
// thinking switch of vLLM-served reasoning models) never reached the wire.
func TestChatOptionsCarriesModelMapParams(t *testing.T) {
	e := NewEngine(nil, nil, nil, Config{MaxTokens: 2048}, nil)
	b := backends.Backend{ModelMap: map[string]backends.ModelSpec{
		"chat": {Model: "m", Params: map[string]any{
			"temperature":          0.2,
			"top_p":                0.9,
			"chat_template_kwargs": map[string]any{"enable_thinking": false},
			"max_tokens":           float64(32000),
		}},
	}}

	opts := e.chatOptions(b, 512)

	if opts.Temperature != 0.2 || opts.TopP != 0.9 {
		t.Errorf("temperature/top_p = %v/%v, want 0.2/0.9", opts.Temperature, opts.TopP)
	}
	ctk, ok := opts.Extra["chat_template_kwargs"].(map[string]any)
	if !ok || ctk["enable_thinking"] != false {
		t.Errorf("Extra = %v, want chat_template_kwargs carried to the stream wire", opts.Extra)
	}
	if opts.NumPredict != 512 {
		t.Errorf("NumPredict = %d, want the clamped 512 — a role-wide max_tokens must not widen the chat budget", opts.NumPredict)
	}
}

// Without model_map params the defaults stay what they were.
func TestChatOptionsDefaultsWithoutParams(t *testing.T) {
	e := NewEngine(nil, nil, nil, Config{MaxTokens: 2048}, nil)
	opts := e.chatOptions(backends.Backend{Model: "m"}, 300)
	if opts.Temperature != 0.7 || opts.NumPredict != 300 || len(opts.Extra) != 0 {
		t.Errorf("opts = %+v, want temperature 0.7, NumPredict 300, no Extra", opts)
	}
}

func TestLaunderErrorReasoningBudget(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", llm.ErrReasoningExhaustedBudget)
	ev := launderError(err, backends.Backend{Name: "gpu"})
	if ev["code"] != "reasoning_budget_exhausted" || ev["retryable"] != false || ev["backend"] != "gpu" {
		t.Errorf("event = %v, want code reasoning_budget_exhausted, retryable false, backend gpu", ev)
	}
}
