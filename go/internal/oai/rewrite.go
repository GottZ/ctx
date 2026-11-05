package oai

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
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

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
