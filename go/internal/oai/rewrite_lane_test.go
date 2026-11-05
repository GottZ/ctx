package oai_test

import (
	"encoding/json"
	"testing"

	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

// TestRewriteServiceTierFromTheLane (Fremd-Review 2 H-6, design/02 §4.6):
// the answer's service_tier comes from the planner's lane — bulk ⇒ "flex",
// urgent ⇒ "default" — never from the upstream. The lane is an input of the
// pure rewrite: the pin holds before A03-W9 (bulk refused in the planner,
// the branch unreachable through the route) and after it (bulk served).
func TestRewriteServiceTierFromTheLane(t *testing.T) {
	up := []byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","service_tier":"UPSTREAM-TIER",` +
		`"choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	for lane, want := range map[string]string{handler.LaneBulk: "flex", handler.LaneUrgent: "default"} {
		body, err := oai.RewriteChatLane(up, "chatcmpl-1", 1700000000, "chat", lane, true)
		if err != nil {
			t.Fatalf("lane %q: %v", lane, err)
		}
		var out struct {
			ServiceTier string `json:"service_tier"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("lane %q: %v", lane, err)
		}
		if out.ServiceTier != want {
			t.Errorf("lane %q: service_tier %q, want %q", lane, out.ServiceTier, want)
		}
	}
}
