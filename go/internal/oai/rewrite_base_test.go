package oai

// The base of the FOLGE-S rewrite (RC-D S2, cluster C8): the map rewrite of
// 437a9f49 (rewrite.go before the token walk), kept verbatim under base
// names as the differential oracle of rewriteChat and rewriteChunkInto —
// TestRewriteChunkIntoIsTheBase, TestRewriteChatIsTheBase and their fuzz
// targets compare the walk with it byte for byte, beyond the two goldens.

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// rewriteChat renders the client body of an upstream answer: id, created and
// model replaced, object and choices kept, usage filtered, service_tier set.
func baseRewriteChat(upstream []byte, id string, created int64, alias, lane string, reasoningOff bool) ([]byte, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(upstream, &in); err != nil {
		return nil, err
	}
	choices, ok := in["choices"]
	if !ok || !hasKind(choices, kindArray) {
		return nil, errNoChoices
	}
	choices, err := baseNormalizeChoices(choices, reasoningOff)
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
func baseNormalizeChoices(choices json.RawMessage, reasoningOff bool) (json.RawMessage, error) {
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
			baseStripReasoning(c)
		}
	}
	return json.Marshal(arr)
}

// stripReasoning removes the reasoning traces from a choice's message.
func baseStripReasoning(c map[string]json.RawMessage) {
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

// rewriteChunk renders the client form of one upstream stream chunk; roles
// is the role carry of the attempt the chunk belongs to.
func baseRewriteChunk(upstream []byte, id string, created int64, alias, lane string, reasoningOff, includeUsage bool, roles *roleCarry) (streamChunk, error) {
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
	choices, n, err := baseNormalizeChunkChoices(choices, reasoningOff, &sc, roles)
	if err != nil {
		return streamChunk{}, err
	}
	baseReadStreamUsage(in, &sc)
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
			out["usage"] = baseFilterUsage(u)
		}
	}
	sc.out, err = json.Marshal(out)
	return sc, err
}

// filterUsage keeps the spec subkeys of a usage object (as rewriteChat).
func baseFilterUsage(u json.RawMessage) json.RawMessage {
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
func baseReadStreamUsage(in map[string]json.RawMessage, sc *streamChunk) {
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
func baseNormalizeChunkChoices(choices json.RawMessage, reasoningOff bool, sc *streamChunk, roles *roleCarry) (json.RawMessage, int, error) {
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
		sc.runes += baseDeltaRunes(delta)
		baseDropNullDeltaKeys(delta)
		if reasoningOff && baseStripDelta(delta) && baseEmptyChoice(c, delta) {
			baseDropped(roles, c["index"], delta)
			continue
		}
		baseRelaying(roles, c["index"], delta)
		c["delta"] = mustJSON(delta)
		kept = append(kept, c)
	}
	out, err := json.Marshal(kept)
	return out, len(kept), err
}

// dropNullDeltaKeys removes the null values the schema does not allow.
func baseDropNullDeltaKeys(delta map[string]json.RawMessage) {
	for k, v := range delta {
		if isNull(v) && !nullableDeltaKeys[k] {
			delete(delta, k)
		}
	}
}

// stripDelta removes the reasoning traces from a delta; true when one was
// there.
func baseStripDelta(delta map[string]json.RawMessage) bool {
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
func baseEmptyChoice(c, delta map[string]json.RawMessage) bool {
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

// deltaRunes counts the generated runes of a delta.
func baseDeltaRunes(delta map[string]json.RawMessage) int {
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

// dropped keeps the role of a choice the strip dropped, unless its index
// relayed one.
func baseDropped(r *roleCarry, index json.RawMessage, delta map[string]json.RawMessage) {
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
func baseRelaying(r *roleCarry, index json.RawMessage, delta map[string]json.RawMessage) {
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
