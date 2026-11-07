package oai

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/modelalias"
)

// The answer rewrite of a non-streamed chat completion (design/02 §4.6, B4):
// the upstream body is read as a map of raw values and only the spec keys
// survive — id, created and model are ctx's, usage keeps its spec subkeys,
// service_tier comes from the planner's lane, and every other key (the
// backend's model name, system_fingerprint, timings, provider, cost, engine
// internals) is dropped. choices pass verbatim (tool calls, refusal,
// logprobs: ctx-B reads them, L7), except the reasoning traces of a
// reasoning-off alias and the spec's required logprobs key, which an engine
// that leaves it out gets as null (CreateChatCompletionResponse).

// usageKeys are the spec subkeys of usage (OpenAI CompletionUsage);
// OpenRouter's cost and every engine extra are dropped.
var usageKeys = map[string]bool{
	"prompt_tokens": true, "completion_tokens": true, "total_tokens": true,
	"prompt_tokens_details": true, "completion_tokens_details": true,
}

// reasoningMessageKeys are the reasoning traces engines put next to a
// message's content (SGLang and vLLM reasoning_content, OpenRouter reasoning
// and reasoning_details): a reasoning-off alias answers without them.
var reasoningMessageKeys = []string{"reasoning_content", "reasoning", "reasoning_details"}

// The service tiers of the lanes (design/02 §4.6, design/03 §4.4): urgent is
// the default tier, bulk the flex tier.
const (
	serviceTierDefault = "default"
	serviceTierFlex    = "flex"
)

func serviceTierOf(lane string) string {
	if lane == "bulk" {
		return serviceTierFlex
	}
	return serviceTierDefault
}

// errNoChoices refuses an upstream body without a choices array.
var errNoChoices = errors.New("oai: upstream answer without choices")

// rewriteChat renders the client body of an upstream answer: id, created and
// model replaced, object and choices kept, usage filtered, service_tier set.
func rewriteChat(upstream []byte, id string, created int64, alias, lane string, reasoningOff bool) ([]byte, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(upstream, &in); err != nil {
		return nil, err
	}
	choices, ok := in["choices"]
	if !ok || !hasKind(choices, kindArray) {
		return nil, errNoChoices
	}
	choices, err := normalizeChoices(choices, reasoningOff)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{
		"id":           mustJSON(id),
		"object":       json.RawMessage(`"chat.completion"`),
		"created":      json.RawMessage(strconv.FormatInt(created, 10)),
		"model":        mustJSON(alias),
		"choices":      choices,
		"service_tier": mustJSON(serviceTierOf(lane)),
	}
	if u, ok := in["usage"]; ok && hasKind(u, kindObject) {
		var usage map[string]json.RawMessage
		if err := json.Unmarshal(u, &usage); err == nil {
			for k, v := range usage {
				// A *_details value that is no object (SGLang sends null)
				// is not the spec's CompletionUsage: dropped like an extra.
				if !usageKeys[k] || (strings.HasSuffix(k, "_details") && !hasKind(v, kindObject)) {
					delete(usage, k)
				}
			}
			out["usage"] = mustJSON(usage)
		}
	}
	return json.Marshal(out)
}

// normalizeChoices brings the choices into the spec shape with one decode:
// a choice without logprobs (llama.cpp leaves the key out; the spec requires
// it, nullable) gets logprobs: null, and for a reasoning-off alias every
// message loses its reasoning traces. Everything else of a choice stays as
// the engine sent it — values are never decoded.
func normalizeChoices(choices json.RawMessage, reasoningOff bool) (json.RawMessage, error) {
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(choices, &arr); err != nil {
		return nil, err
	}
	for _, c := range arr {
		if c == nil {
			continue
		}
		if _, ok := c["logprobs"]; !ok {
			c["logprobs"] = json.RawMessage("null")
		}
		if reasoningOff {
			stripReasoning(c)
		}
	}
	return json.Marshal(arr)
}

// stripReasoning removes the reasoning traces from a choice's message.
func stripReasoning(c map[string]json.RawMessage) {
	raw, ok := c["message"]
	if !ok {
		return
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil || msg == nil {
		return
	}
	changed := false
	for _, k := range reasoningMessageKeys {
		if _, ok := msg[k]; ok {
			delete(msg, k)
			changed = true
		}
	}
	if changed {
		c["message"] = mustJSON(msg)
	}
}

// The stream rewrite (design/02 §4.6 "Stream", W02-08): every upstream
// chunk is read as a map of raw values once and leaves with the key
// allowlist {id, object, created, model, choices, usage, service_tier} — id,
// created and model the same in every chunk (ctx's, as non-stream), object
// chat.completion.chunk, service_tier from the lane. choices pass verbatim
// except what the non-stream rewrite normalizes too (lead decision O-08-3
// (b)): a reasoning-off alias's deltas lose every reasoning trace (a choice
// left empty is dropped, its role carried — roleCarry), and a
// choice without the required delta/index gets them
// (CreateChatCompletionStreamResponse). The usage chunk reaches only a
// client that asked for it (stream_options.include_usage); for every other
// client it is swallowed and usage is removed from every chunk — ctx counts
// it either way (upstream include_usage is always on, llm.putFrame).

// streamChunk is what the relay learns from one upstream chunk besides its
// client form.
type streamChunk struct {
	out  []byte // the client chunk; nil when skip
	skip bool   // nothing for the client: the usage chunk of a client without include_usage, or a chunk emptied by the reasoning strip
	// runes are the runes the engine generated in this chunk (content,
	// reasoning, refusal, tool-call names and arguments) — the completion
	// estimate of a stream without usage (design/06 §4.2 B-06-8).
	runes int
	// usage is the chunk's usage object (prompt, completion), nil without.
	usage *[2]int
	// promptN is llama.cpp's timings.prompt_n (0 = absent).
	promptN int
}

// streamDeltaKeys are the generated text fields of a delta the estimate
// counts.
var streamDeltaKeys = []string{"content", "reasoning_content", "reasoning", "refusal"}

// rewriteChunk renders the client form of one upstream stream chunk; roles
// is the role carry of the attempt the chunk belongs to.
func rewriteChunk(upstream []byte, id string, created int64, alias, lane string, reasoningOff, includeUsage bool, roles *roleCarry) (streamChunk, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(upstream, &in); err != nil {
		return streamChunk{}, err
	}
	var sc streamChunk
	choices, ok := in["choices"]
	switch {
	case !ok || isNull(choices):
		choices = json.RawMessage("[]")
	case !hasKind(choices, kindArray):
		return streamChunk{}, errNoChoices
	}
	choices, n, err := normalizeChunkChoices(choices, reasoningOff, &sc, roles)
	if err != nil {
		return streamChunk{}, err
	}
	readStreamUsage(in, &sc)
	usageObj := false
	if u, ok := in["usage"]; ok && hasKind(u, kindObject) {
		usageObj = true
	}
	// Nothing for the client: the usage chunk without include_usage, an
	// empty chunk that is no usage chunk (client code reads choices[0] of
	// every chunk), and a chunk the reasoning strip emptied.
	if n == 0 && (!usageObj || !includeUsage) {
		sc.skip = true
		return sc, nil
	}
	out := map[string]json.RawMessage{
		"id":           mustJSON(id),
		"object":       json.RawMessage(`"chat.completion.chunk"`),
		"created":      json.RawMessage(strconv.FormatInt(created, 10)),
		"model":        mustJSON(alias),
		"choices":      choices,
		"service_tier": mustJSON(serviceTierOf(lane)),
	}
	if u, ok := in["usage"]; ok && includeUsage {
		out["usage"] = json.RawMessage("null")
		if usageObj {
			out["usage"] = filterUsage(u)
		}
	}
	sc.out, err = json.Marshal(out)
	return sc, err
}

// filterUsage keeps the spec subkeys of a usage object (as rewriteChat).
func filterUsage(u json.RawMessage) json.RawMessage {
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(u, &usage); err != nil {
		return json.RawMessage("null")
	}
	for k, v := range usage {
		if !usageKeys[k] || (strings.HasSuffix(k, "_details") && !hasKind(v, kindObject)) {
			delete(usage, k)
		}
	}
	return mustJSON(usage)
}

// readStreamUsage reads the token counts of a chunk: its usage object and
// llama.cpp's timings.prompt_n.
func readStreamUsage(in map[string]json.RawMessage, sc *streamChunk) {
	if u, ok := in["usage"]; ok && hasKind(u, kindObject) {
		var v struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		}
		if json.Unmarshal(u, &v) == nil {
			sc.usage = &[2]int{v.Prompt, v.Completion}
		}
	}
	if t, ok := in["timings"]; ok && hasKind(t, kindObject) {
		var v struct {
			PromptN int `json:"prompt_n"`
		}
		if json.Unmarshal(t, &v) == nil {
			sc.promptN = v.PromptN
		}
	}
}

// normalizeChunkChoices brings a chunk's choices into the stream schema and
// counts the generated runes. It returns the choices left: a choice the
// reasoning strip emptied is dropped, its role carried (roleCarry).
func normalizeChunkChoices(choices json.RawMessage, reasoningOff bool, sc *streamChunk, roles *roleCarry) (json.RawMessage, int, error) {
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(choices, &arr); err != nil {
		return nil, 0, err
	}
	kept := arr[:0]
	for i, c := range arr {
		if c == nil {
			continue
		}
		if _, ok := c["index"]; !ok {
			c["index"] = json.RawMessage(strconv.Itoa(i))
		}
		delta := map[string]json.RawMessage{}
		if raw, ok := c["delta"]; ok && hasKind(raw, kindObject) {
			if err := json.Unmarshal(raw, &delta); err != nil {
				return nil, 0, err
			}
		}
		sc.runes += deltaRunes(delta)
		dropNullDeltaKeys(delta)
		if reasoningOff && stripDelta(delta) && emptyChoice(c, delta) {
			roles.dropped(c["index"], delta)
			continue
		}
		roles.relaying(c["index"], delta)
		c["delta"] = mustJSON(delta)
		kept = append(kept, c)
	}
	out, err := json.Marshal(kept)
	return out, len(kept), err
}

// nullableDeltaKeys are the delta keys the stream schema allows as null
// (ChatCompletionStreamResponseDelta: content, refusal). A null anywhere else
// — SGLang sends "role":null and "tool_calls":null in every chunk — breaks
// the schema (role is a string enum, tool_calls an array); the null carries
// nothing, so it is dropped (O-08-3 (b), CreateChatCompletionStreamResponse).
var nullableDeltaKeys = map[string]bool{"content": true, "refusal": true}

// dropNullDeltaKeys removes the null values the schema does not allow.
func dropNullDeltaKeys(delta map[string]json.RawMessage) {
	for k, v := range delta {
		if isNull(v) && !nullableDeltaKeys[k] {
			delete(delta, k)
		}
	}
}

// stripDelta removes the reasoning traces from a delta; true when one was
// there.
func stripDelta(delta map[string]json.RawMessage) bool {
	changed := false
	for _, k := range reasoningMessageKeys {
		if _, ok := delta[k]; ok {
			delete(delta, k)
			changed = true
		}
	}
	return changed
}

// emptyChoice reports whether a stripped choice carries nothing a client
// reads: no delta value but nulls and the role, no finish_reason, no
// logprobs. Such a chunk is the reasoning trace itself (its timing) and is
// not relayed; its role moves to the next relayed choice (roleCarry).
func emptyChoice(c, delta map[string]json.RawMessage) bool {
	for k, v := range delta {
		if k != "role" && !isNull(v) {
			return false
		}
	}
	for _, k := range []string{"finish_reason", "logprobs"} {
		if v, ok := c[k]; ok && !isNull(v) {
			return false
		}
	}
	return true
}

// roleCarry is the role state of one stream attempt (W02-08 hardening 5,
// CodeRabbit on PR #166): SGLang sends "role":"assistant" only in its first
// chunk — for a reasoning-off alias a reasoning chunk the strip drops — and
// a client that accumulates the stream needs it (openai-node's helper:
// "missing role for choice 0"). Per choice index it holds the role a dropped
// choice carried before the index relayed one, set into the index's next
// relayed choice (a finish-only one too), and whether a role was relayed (a
// later dropped role is not set again). Fixed size: the indexes below
// modelalias.CeilingMaxN, the most choices any alias lets a request ask
// for; a choice beyond them, and a role outside the stream schema's enum,
// is relayed as before — an engine's stream cannot grow the state.
type roleCarry struct {
	pending [modelalias.CeilingMaxN]string
	relayed [modelalias.CeilingMaxN]bool
}

// streamRoles are the roles of ChatCompletionStreamResponseDelta.
var streamRoles = map[string]bool{"developer": true, "system": true, "user": true, "assistant": true, "tool": true}

// roleSlot is the carry slot of a choice index (false: not tracked).
func roleSlot(index json.RawMessage) (int, bool) {
	i, err := strconv.Atoi(string(index))
	return i, err == nil && i >= 0 && i < modelalias.CeilingMaxN
}

// dropped keeps the role of a choice the strip dropped, unless its index
// relayed one.
func (r *roleCarry) dropped(index json.RawMessage, delta map[string]json.RawMessage) {
	i, ok := roleSlot(index)
	raw, has := delta["role"]
	var role string
	if !ok || !has || r.relayed[i] || json.Unmarshal(raw, &role) != nil || !streamRoles[role] {
		return
	}
	r.pending[i] = role
}

// relaying sets a kept role into a relayed choice without one and marks its
// index's role relayed.
func (r *roleCarry) relaying(index json.RawMessage, delta map[string]json.RawMessage) {
	i, ok := roleSlot(index)
	if !ok {
		return
	}
	if _, has := delta["role"]; !has && r.pending[i] != "" {
		delta["role"] = mustJSON(r.pending[i])
	}
	if _, has := delta["role"]; has {
		r.relayed[i], r.pending[i] = true, ""
	}
}

// deltaRunes counts the generated runes of a delta.
func deltaRunes(delta map[string]json.RawMessage) int {
	n := 0
	for _, k := range streamDeltaKeys {
		var s string
		if raw, ok := delta[k]; ok && json.Unmarshal(raw, &s) == nil {
			n += utf8.RuneCountInString(s)
		}
	}
	if raw, ok := delta["tool_calls"]; ok {
		var calls []struct {
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &calls) == nil {
			for _, tc := range calls {
				n += utf8.RuneCountInString(tc.Function.Name) + utf8.RuneCountInString(tc.Function.Arguments)
			}
		}
	}
	return n
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
