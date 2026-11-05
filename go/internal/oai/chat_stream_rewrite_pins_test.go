package oai_test

// Pins of the stream chunk rewrite over the real route (W02-08, Härtung 2):
// the surviving rewrite mutants of Fremd-Review 2 (L-6) made permanent
// gates. Each test names what it pins and the mutant that turns it red.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/promptguard"
)

// pinChoices decodes the choices of a client chunk.
func pinChoices(t *testing.T, m map[string]json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var ch []map[string]json.RawMessage
	if err := json.Unmarshal(m["choices"], &ch); err != nil {
		t.Fatalf("choices %s: %v", m["choices"], err)
	}
	return ch
}

// pinSGLangThinkEnd is a reasoning chunk of SGLang that carries the
// finish_reason: the model ran out of tokens while it was still reasoning.
const pinSGLangThinkEnd = `{"id":"ID-CANARY-sglang","object":"chat.completion.chunk","created":1700000000,"model":"MODEL-CANARY",` +
	`"choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":"THINK-CANARY","tool_calls":null},"logprobs":null,` +
	`"finish_reason":"length","matched_stop":null}],"usage":null}`

// TestPinReasoningOffKeepsTheFinishReason pins that a reasoning-off alias's
// strip never swallows a chunk's finish_reason: a stream that ends inside
// the reasoning reaches the client as one emptied chunk with finish_reason
// length and no trace. Red: Fremd-Review 2 S02 (emptyChoice ignores
// finish_reason — the chunk is dropped, the client never sees a finish).
func TestPinReasoningOffKeepsTheFinishReason(t *testing.T) {
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{data: sglangThink}, {data: sglangThink}, {data: pinSGLangThinkEnd}, {data: "[DONE]"}}, nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	chunks := checkRelay(t, a)
	if strings.Contains(a.raw, "reasoning") {
		t.Errorf("a reasoning trace reached the client of a reasoning-off alias: %s", a.raw)
	}
	if len(chunks) != 1 {
		t.Fatalf("%d chunks relayed, want 1 (the finish chunk, emptied): %s", len(chunks), a.raw)
	}
	ch := pinChoices(t, chunks[0])
	if len(ch) != 1 || string(ch[0]["finish_reason"]) != `"length"` {
		t.Fatalf("finish chunk %s, want one choice with finish_reason length", chunks[0]["choices"])
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(ch[0]["delta"], &delta); err != nil {
		t.Fatalf("delta %s: %v", ch[0]["delta"], err)
	}
	for k, v := range delta {
		if string(v) != "null" {
			t.Errorf("finish chunk delta %s: key %s carries %s, want an emptied delta", ch[0]["delta"], k, v)
		}
	}
}

// pinOpenRouterThink is an OpenRouter chunk with its reasoning form:
// reasoning and reasoning_details, not null.
func pinOpenRouterThink(content string) string {
	return `{"id":"gen-ID-CANARY","provider":"PROVIDER-CANARY","model":"MODEL-CANARY","object":"chat.completion.chunk","created":1700000000,` +
		`"choices":[{"index":0,"delta":{"role":"assistant","content":` + jsonOrNull(content) + `,"reasoning":"THINK-CANARY",` +
		`"reasoning_details":[{"type":"reasoning.text","text":"THINK-CANARY","format":"unknown","index":0}]},"finish_reason":null,` +
		`"native_finish_reason":null,"logprobs":null}]}`
}

// TestPinReasoningOffStripsTheOpenRouterForm pins the strip of OpenRouter's
// reasoning keys for a reasoning-off alias: neither reasoning nor
// reasoning_details reaches the client, in a reasoning-only chunk or next to
// content, and the content arrives whole. Red: Fremd-Review 2 S11
// (stripDelta removes reasoning_content only, the SGLang form).
func TestPinReasoningOffStripsTheOpenRouterForm(t *testing.T) {
	steps := []sseStep{{raw: ": OPENROUTER PROCESSING\n\n"}, {data: pinOpenRouterThink("")}, {data: pinOpenRouterThink("hello")}}
	for i, d := range streamDeltas[1:] {
		fin := ""
		if i == len(streamDeltas)-2 {
			fin = "stop"
		}
		steps = append(steps, sseStep{data: openrouterChunk(d, fin)})
	}
	u := newUpstream(t, "")
	u.setRespond(sseRespond(append(steps, sseStep{data: openrouterUsage}, sseStep{data: "[DONE]"}), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	chunks := checkRelay(t, a)
	if strings.Contains(a.raw, "reasoning") {
		t.Errorf("an OpenRouter reasoning key reached the client of a reasoning-off alias: %s", a.raw)
	}
	if got := streamContent(chunks); got != streamText {
		t.Errorf("content %q, want %q", got, streamText)
	}
}

// TestPinStreamServiceTierFollowsTheLane pins the stream chunks'
// service_tier: a consumer on the bulk lane gets "flex" in every chunk, the
// usage chunk included. Red: Fremd-Review 2 S12 (service_tier fixed to
// "default" in the stream rewrite).
func TestPinStreamServiceTierFollowsTheLane(t *testing.T) {
	u := newUpstream(t, "")
	u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withLane(handler.LaneBulk))
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat", `"stream_options":{"include_usage":true}`))
	chunks := checkRelay(t, a)
	if len(chunks) != len(streamDeltas)+1 {
		t.Fatalf("%d chunks, want %d (the deltas and the usage chunk): %s", len(chunks), len(streamDeltas)+1, a.raw)
	}
	for i, m := range chunks {
		if string(m["service_tier"]) != `"flex"` {
			t.Errorf("chunk %d: service_tier %s, want \"flex\" for the bulk lane", i, m["service_tier"])
		}
	}
}

// TestPinStreamChoiceGetsItsIndex pins the schema default of a choice's
// index (CreateChatCompletionStreamResponse requires it): an engine chunk
// whose choice has no index reaches the client with index 0. Red:
// Fremd-Review 2 S14 (no index default — the choice goes out without it).
func TestPinStreamChoiceGetsItsIndex(t *testing.T) {
	var steps []sseStep
	for i, d := range streamDeltas {
		fin := ""
		if i == len(streamDeltas)-1 {
			fin = "stop"
		}
		c := llamaChunk(d, fin)
		if !strings.Contains(c, `"index":0,`) {
			t.Fatalf("fixture %s has no index to remove", c)
		}
		steps = append(steps, sseStep{data: strings.Replace(c, `"index":0,`, "", 1)})
	}
	u := newUpstream(t, "")
	u.setRespond(sseRespond(append(steps, sseStep{data: llamaUsage}, sseStep{data: "[DONE]"}), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	chunks := checkRelay(t, a)
	if len(chunks) != len(streamDeltas) {
		t.Fatalf("%d chunks, want %d: %s", len(chunks), len(streamDeltas), a.raw)
	}
	for i, m := range chunks {
		ch := pinChoices(t, m)
		if len(ch) != 1 || string(ch[0]["index"]) != "0" {
			t.Errorf("chunk %d: choices %s, want one choice with index 0", i, m["choices"])
		}
	}
	if got := streamContent(chunks); got != streamText {
		t.Errorf("content %q, want %q", got, streamText)
	}
}

// TestPinStreamUsageNullStays pins design/02 §4.6 ("usage: null in den
// anderen Chunks bleibt"): for an include_usage client, an engine chunk that
// carries usage: null keeps it; the usage chunk carries the object. Red:
// Fremd-Review 2 S15 (usage: null not relayed — the key disappears).
func TestPinStreamUsageNullStays(t *testing.T) {
	u := newUpstream(t, "")
	u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil)) // SGLang: "usage":null in every content chunk
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat", `"stream_options":{"include_usage":true}`))
	chunks := checkRelay(t, a)
	if len(chunks) != len(streamDeltas)+1 {
		t.Fatalf("%d chunks, want %d (the deltas and the usage chunk): %s", len(chunks), len(streamDeltas)+1, a.raw)
	}
	for i, m := range chunks[:len(streamDeltas)] {
		if v, ok := m["usage"]; !ok || string(v) != "null" {
			t.Errorf("content chunk %d: usage %q (present %v), want usage: null kept", i, v, ok)
		}
	}
	if v := string(chunks[len(chunks)-1]["usage"]); !strings.HasPrefix(v, "{") {
		t.Errorf("usage chunk: usage %s, want the object", v)
	}
}

// TestPinStreamEstimateCountsToolCalls pins the completion estimate of a
// stream without usage that answers with a tool call: the runes of the
// call's name and arguments count (design/06 §4.2 B-06-8), in the row's
// token column and on the access record. Red: Fremd-Review 2 S10
// (deltaRunes without tool_calls — the estimate books 0).
func TestPinStreamEstimateCountsToolCalls(t *testing.T) {
	const name, args = "write_file", `{"path":"a","body":"0123456789"}`
	argsJSON, _ := json.Marshal(args)
	head := `{"id":"chatcmpl-ID-CANARY","object":"chat.completion.chunk","created":1700000000,"model":"MODEL-CANARY","choices":[{"index":0,"delta":`
	steps := []sseStep{
		{data: head + `{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"` + name + `","arguments":""}}]},"logprobs":null,"finish_reason":null}]}`},
		{data: head + `{"tool_calls":[{"index":0,"function":{"arguments":` + string(argsJSON) + `}}]},"logprobs":null,"finish_reason":null}]}`},
		{data: head + `{},"logprobs":null,"finish_reason":"tool_calls"}]}`},
		{data: "[DONE]"},
	}
	u := newUpstream(t, "")
	u.setRespond(sseRespond(steps, nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	checkRelay(t, a)
	want := promptguard.TokenEstimate(len([]rune(name)) + len([]rune(args)))
	if want <= 0 {
		t.Fatalf("estimate of %d runes is %d", len(name)+len(args), want)
	}
	rows := rig.rows.all()
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	if r := rows[0]; r.Gateway.Usage != llmlog.UsageEstimated || r.Telemetry.CompletionTokens != want || r.Gateway.Finish != llmlog.FinishToolCalls {
		t.Errorf("row usage %q completion %d finish %q, want estimated %d (the tool call's %d runes), finish tool_calls",
			r.Gateway.Usage, r.Telemetry.CompletionTokens, r.Gateway.Finish, want, len(name)+len(args))
	}
	if rec := <-rig.recs; rec.Usage != llmlog.UsageEstimated || rec.CompletionTokens != want {
		t.Errorf("access record usage %q completion %d, want estimated %d", rec.Usage, rec.CompletionTokens, want)
	}
}

// TestPinStreamCreatedIsTheFirstAdmission pins created after a failover
// before the commit: every chunk carries the admission second of the
// request's FIRST attempt, as the non-stream answer does (attempt: the
// IsZero guard) — row 1 refuses after 2.1 s, so a later admission lies in a
// later second. Red: Fremd-Review 2 MD5 (admitted set on every attempt —
// the chunks carry the failover's second).
func TestPinStreamCreatedIsTheFirstAdmission(t *testing.T) {
	u1, u2 := newUpstream(t, ""), newUpstream(t, "")
	var failedAt atomic.Int64
	u1.setRespond(status502(2100*time.Millisecond, &failedAt))
	u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	if a.status != http.StatusOK || a.dones() != 1 || failedAt.Load() == 0 {
		t.Fatalf("answer %d dones %d (row 1 failed: %v): %q", a.status, a.dones(), failedAt.Load() != 0, a.raw)
	}
	lo, hi := a.start.Unix(), a.start.Add(500*time.Millisecond).Unix()
	n := 0
	for _, e := range a.events {
		if e.data == "" || e.data == "[DONE]" {
			continue
		}
		var c struct {
			Created int64 `json:"created"`
		}
		if err := json.Unmarshal([]byte(e.data), &c); err != nil {
			t.Fatalf("chunk %q: %v", e.data, err)
		}
		n++
		if c.Created < lo || c.Created > hi {
			t.Errorf("chunk created %d, want the first admission's second (%d…%d; the failover came at +%v)", c.Created, lo, hi,
				time.Unix(0, failedAt.Load()).Sub(a.start).Round(time.Millisecond))
		}
	}
	if n == 0 {
		t.Fatalf("no chunk: %q", a.raw)
	}
}
