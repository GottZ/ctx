package oai_test

// Pins of the role carry of the stream rewrite (W02-08 hardening 5, PR #166
// CodeRabbit thread at rewrite.go:330): an engine that sends the assistant
// role only in its reasoning chunks (SGLang: "role":"assistant" in the first
// chunk, "role":null after it) loses it to a reasoning-off alias's strip —
// the emptied chunk is not relayed — and a client that accumulates the
// stream (the openai-node helper: "missing role for choice 0") breaks after
// the text. The rewrite carries a role the strip dropped before it was
// relayed into the next relayed choice of the same index, a finish-only
// choice included, once per choice and attempt. Red (base 1ef1cfae): the
// first relayed choice carries no role.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// roleChunk is an upstream chunk with the given choices (canaries in every
// field the rewrite replaces).
func roleChunk(choices ...string) string {
	return `{"id":"ID-CANARY-role","object":"chat.completion.chunk","created":1700000000,"model":"MODEL-CANARY","choices":[` +
		strings.Join(choices, ",") + `],"usage":null}`
}

// The choices of the role pins in SGLang's form (every delta key, null where
// it carries nothing; the role only in the first reasoning chunk).
const (
	roleThink0   = `{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":"THINK-CANARY","tool_calls":null},"logprobs":null,"finish_reason":null,"matched_stop":null}`
	roleThink1   = `{"index":1,"delta":{"role":"assistant","content":null,"reasoning_content":"THINK-CANARY","tool_calls":null},"logprobs":null,"finish_reason":null,"matched_stop":null}`
	roleThinkN1  = `{"index":1,"delta":{"role":null,"content":null,"reasoning_content":"THINK-CANARY","tool_calls":null},"logprobs":null,"finish_reason":null,"matched_stop":null}`
	roleContentA = `{"index":0,"delta":{"role":null,"content":"A","reasoning_content":null,"tool_calls":null},"logprobs":null,"finish_reason":null,"matched_stop":null}`
	roleContentB = `{"index":1,"delta":{"role":null,"content":"B","reasoning_content":null,"tool_calls":null},"logprobs":null,"finish_reason":null,"matched_stop":null}`
	roleFinish0  = `{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"logprobs":null,"finish_reason":"stop","matched_stop":null}`
	roleFinish1  = `{"index":1,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"logprobs":null,"finish_reason":"stop","matched_stop":null}`
)

// roleSteps renders upstream chunks as a stream ending with [DONE].
func roleSteps(chunks ...string) []sseStep {
	steps := make([]sseStep, 0, len(chunks)+1)
	for _, c := range chunks {
		steps = append(steps, sseStep{data: c})
	}
	return append(steps, sseStep{data: "[DONE]"})
}

// relayedChoice is one choice of a relayed chunk.
type relayedChoice struct {
	chunk int    // the chunk's position among the relayed chunks
	role  string // the delta's role ("" = no role key)
	delta map[string]json.RawMessage
	raw   map[string]json.RawMessage
}

// relayedChoices groups the choices of the relayed chunks by their index.
func relayedChoices(t *testing.T, chunks []map[string]json.RawMessage) map[string][]relayedChoice {
	t.Helper()
	out := map[string][]relayedChoice{}
	for i, m := range chunks {
		for _, c := range pinChoices(t, m) {
			var delta map[string]json.RawMessage
			if err := json.Unmarshal(c["delta"], &delta); err != nil {
				t.Fatalf("chunk %d: delta %s: %v", i, c["delta"], err)
			}
			rc := relayedChoice{chunk: i, delta: delta, raw: c}
			if r, ok := delta["role"]; ok {
				if err := json.Unmarshal(r, &rc.role); err != nil || rc.role == "" {
					t.Errorf("chunk %d: role %s is no role string", i, r)
					rc.role = string(r)
				}
			}
			out[string(c["index"])] = append(out[string(c["index"])], rc)
		}
	}
	return out
}

// checkRoleOnce asserts that every choice index in want gets the assistant
// role exactly once, in its first relayed choice.
func checkRoleOnce(t *testing.T, raw string, byIndex map[string][]relayedChoice, want ...string) {
	t.Helper()
	if len(byIndex) != len(want) {
		t.Errorf("choice indexes %d, want %d (%v): %s", len(byIndex), len(want), want, raw)
	}
	for _, idx := range want {
		cs := byIndex[idx]
		if len(cs) == 0 {
			t.Errorf("choice %s: never relayed: %s", idx, raw)
			continue
		}
		if cs[0].role != "assistant" {
			t.Errorf(`choice %s: the first relayed choice (chunk %d) carries role %q, want "assistant" — a client that accumulates the stream misses the role: %s`,
				idx, cs[0].chunk, cs[0].role, raw)
		}
		for _, c := range cs[1:] {
			if c.role != "" {
				t.Errorf("choice %s: chunk %d carries role %q again, want the role once (in chunk %d): %s", idx, c.chunk, c.role, cs[0].chunk, raw)
			}
		}
	}
}

// postRoleStream serves the upstream chunks through a one-row rig and
// returns the relayed chunks and the raw stream.
func postRoleStream(t *testing.T, model string, chunks ...string) ([]map[string]json.RawMessage, string) {
	t.Helper()
	u := newUpstream(t, "")
	u.setRespond(sseRespond(roleSteps(chunks...), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	rig.reg.reload(t, pinReasoningAliases())
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody(model))
	if model == "chat" {
		return checkRelay(t, a), a.raw
	}
	// checkRelay pins the alias "chat"; another alias gets the framing
	// checks the role pins need.
	if a.status != 200 || a.readErr != nil || a.dones() != 1 || a.events[len(a.events)-1].data != "[DONE]" {
		t.Fatalf("answer %d, read error %v, [DONE] %d: %s", a.status, a.readErr, a.dones(), a.raw)
	}
	var out []map[string]json.RawMessage
	for _, c := range a.chunks() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(c), &m); err != nil {
			t.Fatalf("chunk %q: %v", c, err)
		}
		if string(m["model"]) != `"`+model+`"` {
			t.Errorf("chunk %s: model, want %q", c, model)
		}
		out = append(out, m)
	}
	return out, a.raw
}

// TestPinReasoningOffCarriesTheRole is the CodeRabbit thread on PR #166
// (rewrite.go:330): a reasoning-off alias keeps the assistant role the
// engine sent only in a reasoning chunk — it reaches the client in the
// first relayed choice of its index, exactly once. Red (base 1ef1cfae, and
// the mutant "role not carried"): the first relayed choice has no role.
func TestPinReasoningOffCarriesTheRole(t *testing.T) {
	t.Run("role in the reasoning chunk, then content", func(t *testing.T) {
		// The thread's form: role and reasoning, then the text, then finish.
		chunks, raw := postRoleStream(t, "chat",
			roleChunk(`{"index":0,"delta":{"role":"assistant","reasoning_content":"THINK-CANARY"}}`),
			roleChunk(`{"index":0,"delta":{"content":"Hi"}}`),
			roleChunk(`{"index":0,"delta":{},"finish_reason":"stop"}`))
		if len(chunks) != 2 {
			t.Fatalf("%d chunks relayed, want 2 (the reasoning chunk dropped): %s", len(chunks), raw)
		}
		by := relayedChoices(t, chunks)
		checkRoleOnce(t, raw, by, "0")
		if c := by["0"][0]; string(c.delta["content"]) != `"Hi"` {
			t.Errorf("first relayed delta %s, want the content Hi next to the role", c.raw["delta"])
		}
	})
	t.Run("role in the reasoning chunk, then only finish_reason", func(t *testing.T) {
		// The stream ends after the reasoning: the role lands in the
		// finish-only choice.
		chunks, raw := postRoleStream(t, "chat",
			roleChunk(roleThink0), roleChunk(roleFinish0))
		if len(chunks) != 1 {
			t.Fatalf("%d chunks relayed, want 1 (the finish chunk): %s", len(chunks), raw)
		}
		by := relayedChoices(t, chunks)
		checkRoleOnce(t, raw, by, "0")
		if c := by["0"][0]; string(c.raw["finish_reason"]) != `"stop"` {
			t.Errorf("relayed choice %v, want finish_reason stop", c.raw)
		}
	})
	t.Run("two choices, each with its own role", func(t *testing.T) {
		// n = 2 in SGLang's form: both roles in the first reasoning chunk;
		// choice 0's text comes next to choice 1's further reasoning, choice
		// 1's text one chunk later. Each index gets its role in its own
		// first relayed choice — never the other index's.
		chunks, raw := postRoleStream(t, "chat",
			roleChunk(roleThink0, roleThink1),
			roleChunk(roleContentA, roleThinkN1),
			roleChunk(roleContentB),
			roleChunk(roleFinish0, roleFinish1))
		if len(chunks) != 3 {
			t.Fatalf("%d chunks relayed, want 3: %s", len(chunks), raw)
		}
		by := relayedChoices(t, chunks)
		checkRoleOnce(t, raw, by, "0", "1")
		if by["0"][0].chunk != 0 || by["1"][0].chunk != 1 {
			t.Errorf("first relayed choice of index 0 in chunk %d, of index 1 in chunk %d, want 0 and 1: %s", by["0"][0].chunk, by["1"][0].chunk, raw)
		}
		if got := len(pinChoices(t, chunks[0])); got != 1 {
			t.Errorf("chunk 0 relays %d choices, want 1 (choice 1's reasoning dropped): %s", got, raw)
		}
	})
	t.Run("a role already relayed is not carried again", func(t *testing.T) {
		// Control: the role went out with the first text; a later
		// reasoning-only chunk repeats it (an engine that sends the role in
		// every chunk) and is dropped — the next text carries no role.
		chunks, raw := postRoleStream(t, "chat",
			roleChunk(`{"index":0,"delta":{"role":"assistant","content":"A"}}`),
			roleChunk(`{"index":0,"delta":{"role":"assistant","reasoning_content":"THINK-CANARY"}}`),
			roleChunk(`{"index":0,"delta":{"content":"B"},"finish_reason":"stop"}`))
		if len(chunks) != 2 {
			t.Fatalf("%d chunks relayed, want 2: %s", len(chunks), raw)
		}
		checkRoleOnce(t, raw, relayedChoices(t, chunks), "0")
	})
}

// TestPinReasoningOnRelaysTheRoleAsSent is the negative form: a
// reasoning-passthrough alias strips nothing, so the reasoning chunk with
// its role is relayed as the engine sent it and no role is added anywhere.
func TestPinReasoningOnRelaysTheRoleAsSent(t *testing.T) {
	chunks, raw := postRoleStream(t, "think",
		roleChunk(`{"index":0,"delta":{"role":"assistant","reasoning_content":"pondering"}}`),
		roleChunk(`{"index":0,"delta":{"content":"Hi"}}`),
		roleChunk(`{"index":0,"delta":{},"finish_reason":"stop"}`))
	if len(chunks) != 3 {
		t.Fatalf("%d chunks relayed, want 3 (nothing stripped): %s", len(chunks), raw)
	}
	by := relayedChoices(t, chunks)
	checkRoleOnce(t, raw, by, "0")
	if c := by["0"][0]; string(c.delta["reasoning_content"]) != `"pondering"` || c.chunk != 0 {
		t.Errorf("first relayed choice %v in chunk %d, want the reasoning chunk with its role as sent", c.raw, c.chunk)
	}
}

// TestPinRoleCarryEndsWithTheAttempt pins that the carry belongs to one
// attempt: row one drops a reasoning chunk with the role and then fails
// before any frame reached the client (failover); row two streams chunks
// without a role, and none is added — the role of an attempt that relayed
// nothing is not the next attempt's. Red: the carry state outlives the
// attempt (one state per request).
func TestPinRoleCarryEndsWithTheAttempt(t *testing.T) {
	u1, u2 := newUpstream(t, ""), newUpstream(t, "")
	u1.setRespond(sseRespond([]sseStep{{data: roleChunk(roleThink0)}, {drop: true}}, nil, nil))
	u2.setRespond(sseRespond(streamShapes[1].steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	chunks := checkRelay(t, a)
	if h1, h2 := u1.hits.Load(), u2.hits.Load(); h1 != 1 || h2 != 1 {
		t.Fatalf("hits %d/%d, want one attempt on each row (failover before the commit): %s", h1, h2, a.raw)
	}
	if got := streamContent(chunks); got != streamText {
		t.Errorf("content %q, want row two's %q", got, streamText)
	}
	for _, cs := range relayedChoices(t, chunks) {
		for _, c := range cs {
			if c.role != "" {
				t.Errorf("chunk %d carries role %q: row two sent none, the first attempt's role crossed the failover: %s", c.chunk, c.role, a.raw)
			}
		}
	}
}
