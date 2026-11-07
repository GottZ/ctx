package oai

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/GottZ/ctx/internal/modelalias"
)

// The answer rewrite of a non-streamed chat completion (design/02 §4.6, B4):
// only the spec keys of the upstream body survive — id, created and model
// are ctx's, usage keeps its spec subkeys, service_tier comes from the
// planner's lane, and every other key (the backend's model name,
// system_fingerprint, timings, provider, cost, engine internals) is dropped.
// choices pass verbatim (tool calls, refusal, logprobs: ctx-B reads them,
// L7), except the reasoning traces of a reasoning-off alias and the spec's
// required logprobs key, which an engine that leaves it out gets as null
// (CreateChatCompletionResponse). The non-stream rewrite is still the map
// rewrite below: its walk (FOLGE-S, the stream's writer in rewrite_walk.go)
// waits for an exception at the route's answer write (gosec G705:
// json.Marshal is the HTML escape its taint check knows, the walk's identical
// escape is not). TestRewriteChatTraceIsTheBase pins the bytes.

// usageKeys are the spec subkeys of usage (OpenAI CompletionUsage);
// OpenRouter's cost and every engine extra are dropped.
var usageKeys = map[string]bool{
	"prompt_tokens": true, "completion_tokens": true, "total_tokens": true,
	"prompt_tokens_details": true, "completion_tokens_details": true,
}

// usageKeyOrder are usageKeys in the order json.Marshal writes them.
var usageKeyOrder = sortedNames(usageKeys)

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
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

// rewriteOverhead is what the stream rewrite may add to an upstream chunk
// besides the escapes of id and alias: the keys ctx sets, created, the
// index and delta a choice may lack, the SSE frame. The output buffer starts
// at the upstream length plus this; a chunk that grows past it (HTML
// escapes, many choices) grows the buffer by doubling.
const rewriteOverhead = 256

// rewriteBufferLen is the first size of a rewrite's output buffer.
func rewriteBufferLen(upstream int, id, alias string) int {
	return upstream + rewriteOverhead + 6*(len(id)+len(alias))
}

// topMembers is the last value of choices, usage and timings in the
// upstream object raw (nil: absent) — the values the map rewrite read.
func topMembers(raw []byte) (choices, usage, timings []byte) {
	it, _ := newMemberIter(raw)
	for {
		k, v, more := it.next()
		if !more {
			return choices, usage, timings
		}
		switch {
		case keyIs(k, "choices"):
			choices = v
		case keyIs(k, "usage"):
			usage = v
		case keyIs(k, "timings"):
			timings = v
		}
	}
}

// upstreamObject validates an upstream body as the map rewrite's
// json.Unmarshal did: the trimmed object, or null (true) for a JSON null —
// the map rewrite read no member of it — or the decode's own error.
func upstreamObject(upstream []byte) (obj []byte, null bool, err error) {
	if !json.Valid(upstream) {
		return nil, false, mapError[map[string]json.RawMessage](upstream)
	}
	t := trimJSON(upstream)
	switch {
	case isNull(t):
		return nil, true, nil
	case t[0] != '{':
		return nil, false, mapError[map[string]json.RawMessage](upstream)
	}
	return t, false, nil
}

// checkChoiceKinds refuses a choices array with an element that is neither
// an object nor null, with the error the map rewrite's decode gave.
func checkChoiceKinds(choices []byte) error {
	it, _ := newArrayIter(choices)
	for {
		e, more := it.next()
		if !more {
			return nil
		}
		if !isNull(e) && !hasKind(e, kindObject) {
			return mapError[[]map[string]json.RawMessage](choices)
		}
	}
}

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

// nullValue is the raw JSON null the rewrite adds.
var nullValue = []byte("null")

// appendUsage writes a usage object (valid JSON) with its spec subkeys only;
// a *_details value that is no object (SGLang sends null) is not the spec's
// CompletionUsage and is dropped like an extra.
func appendUsage(dst, usage []byte) []byte {
	var buf [levelMembersMax]member
	um := collectMembers(buf[:0], usage)
	dst = append(growBy(dst, 2), '{')
	first := true
	for _, name := range usageKeyOrder {
		at := findMember(um, name)
		if at < 0 || strings.HasSuffix(name, "_details") && !hasKind(um[at].val, kindObject) {
			continue
		}
		dst = appendMember(dst, first, um[at].key, um[at].val)
		first = false
	}
	return append(growBy(dst, 1), '}')
}

// The stream rewrite (design/02 §4.6 "Stream", W02-08): every upstream
// chunk leaves with the key allowlist {id, object, created, model, choices,
// usage, service_tier} — id, created and model the same in every chunk
// (ctx's, as non-stream), object chat.completion.chunk, service_tier from the
// lane. choices pass verbatim except what the non-stream rewrite normalizes
// too (lead decision O-08-3 (b)): a reasoning-off alias's deltas lose every
// reasoning trace (a choice left empty is dropped, its role carried —
// roleCarry), and a choice without the required delta/index gets them
// (CreateChatCompletionStreamResponse). The usage chunk reaches only a
// client that asked for it (stream_options.include_usage); for every other
// client it is swallowed and usage is removed from every chunk — ctx counts
// it either way (upstream include_usage is always on, llm.putFrame). The
// walk is rewriteChunkInto (rewrite_into.go).

// streamChunk is what the relay learns from one upstream chunk besides its
// client form.
type streamChunk struct {
	out  []byte // the client chunk's SSE line; nil when skip
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

// readStreamUsage reads the token counts of a chunk: its usage object and
// llama.cpp's timings.prompt_n (small values, decoded as the map rewrite
// decoded them).
func readStreamUsage(usage, timings []byte, sc *streamChunk) {
	if usage != nil && hasKind(usage, kindObject) {
		var v struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		}
		if json.Unmarshal(usage, &v) == nil {
			sc.usage = &[2]int{v.Prompt, v.Completion}
		}
	}
	if timings != nil && hasKind(timings, kindObject) {
		var v struct {
			PromptN int `json:"prompt_n"`
		}
		if json.Unmarshal(timings, &v) == nil {
			sc.promptN = v.PromptN
		}
	}
}

// appendChunkChoices writes the choices of a chunk (a valid array of objects
// and nulls) in the stream schema and counts the generated runes. It
// returns the choices kept: a null is dropped, and a choice the reasoning
// strip emptied is dropped with its role carried (roleCarry).
func appendChunkChoices(dst, choices []byte, reasoningOff bool, sc *streamChunk, roles *roleCarry) ([]byte, int) {
	dst = append(growBy(dst, 2), '[')
	it, _ := newArrayIter(choices)
	kept := 0
	for i := 0; ; i++ {
		e, more := it.next()
		if !more {
			break
		}
		if isNull(e) {
			continue
		}
		var cbuf, dbuf [levelMembersMax]member
		var ibuf [20]byte
		cm := collectMembers(cbuf[:0], e)
		at := findMember(cm, "index")
		if at < 0 {
			cm = insertMember(cm, "index", strconv.AppendInt(ibuf[:0], int64(i), 10))
			at = findMember(cm, "index")
		}
		index := cm[at].val
		dm := dbuf[:0]
		if d := findMember(cm, "delta"); d >= 0 && hasKind(cm[d].val, kindObject) {
			dm = collectMembers(dm, cm[d].val)
		}
		sc.runes += deltaRunes(dm)
		dropNullDeltaKeys(dm)
		if reasoningOff && stripDelta(dm) && emptyChoice(cm, dm) {
			roles.dropped(index, dm)
			continue
		}
		dm = roles.relaying(index, dm)
		if findMember(cm, "delta") < 0 {
			cm = insertMember(cm, "delta", nil)
		}
		if kept > 0 {
			dst = append(dst, ',')
		}
		dst = appendChoice(dst, cm, dm)
		kept++
	}
	return append(growBy(dst, 1), ']'), kept
}

// appendChoice writes a stream choice: its members in key order, delta as
// the rewritten delta dm.
func appendChoice(dst []byte, cm, dm []member) []byte {
	dst = append(growBy(dst, 2), '{')
	for j, m := range cm {
		if !keyIs(m.key, "delta") {
			dst = appendMember(dst, j == 0, m.key, m.val)
			continue
		}
		if j > 0 {
			dst = append(dst, ',')
		}
		dst = appendKey(dst, m.key)
		dst = append(dst, ':')
		dst = appendLevel(dst, dm)
	}
	return append(growBy(dst, 1), '}')
}

// nullableDeltaKeys are the delta keys the stream schema allows as null
// (ChatCompletionStreamResponseDelta: content, refusal). A null anywhere else
// — SGLang sends "role":null and "tool_calls":null in every chunk — breaks
// the schema (role is a string enum, tool_calls an array); the null carries
// nothing, so it is dropped (O-08-3 (b), CreateChatCompletionStreamResponse).
var nullableDeltaKeys = map[string]bool{"content": true, "refusal": true}

// dropNullDeltaKeys drops the null values the schema does not allow.
func dropNullDeltaKeys(dm []member) {
	for j := range dm {
		if dm[j].gone || !isNull(dm[j].val) {
			continue
		}
		nullable := false
		for k := range nullableDeltaKeys {
			nullable = nullable || keyIs(dm[j].key, k)
		}
		dm[j].gone = !nullable
	}
}

// stripDelta drops the reasoning traces of a delta; true when one was
// there.
func stripDelta(dm []member) bool {
	changed := false
	for _, k := range reasoningMessageKeys {
		if at := findMember(dm, k); at >= 0 {
			dm[at].gone, changed = true, true
		}
	}
	return changed
}

// emptyChoice reports whether a stripped choice carries nothing a client
// reads: no delta value but nulls and the role, no finish_reason, no
// logprobs. Such a chunk is the reasoning trace itself (its timing) and is
// not relayed; its role moves to the next relayed choice (roleCarry).
func emptyChoice(cm, dm []member) bool {
	for _, m := range dm {
		if !m.gone && !keyIs(m.key, "role") && !isNull(m.val) {
			return false
		}
	}
	for _, k := range []string{"finish_reason", "logprobs"} {
		if at := findMember(cm, k); at >= 0 && !isNull(cm[at].val) {
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

// quotedRoles are the roles as the rewrite writes a carried one.
var quotedRoles = func() map[string][]byte {
	q := make(map[string][]byte, len(streamRoles))
	for r := range streamRoles {
		q[r] = appendString(nil, r)
	}
	return q
}()

// roleSlot is the carry slot of a choice index (false: not tracked).
func roleSlot(index []byte) (int, bool) {
	i, ok := rawInt(index)
	return i, ok && i >= 0 && i < modelalias.CeilingMaxN
}

// dropped keeps the role of a choice the strip dropped, unless its index
// relayed one.
func (r *roleCarry) dropped(index []byte, dm []member) {
	i, ok := roleSlot(index)
	at := findMember(dm, "role")
	if !ok || at < 0 || r.relayed[i] {
		return
	}
	if role, ok := rawStringIn(dm[at].val, streamRoles); ok {
		r.pending[i] = role
	}
}

// relaying sets a kept role into a relayed choice's delta without one and
// marks its index's role relayed; it returns the delta.
func (r *roleCarry) relaying(index []byte, dm []member) []member {
	i, ok := roleSlot(index)
	if !ok {
		return dm
	}
	if findMember(dm, "role") < 0 && r.pending[i] != "" {
		dm = insertMember(dm, "role", quotedRoles[r.pending[i]])
	}
	if findMember(dm, "role") >= 0 {
		r.relayed[i], r.pending[i] = true, ""
	}
	return dm
}

// errNotText refuses a value that is no JSON string where a tool call's
// name or arguments are read (deltaRunes): the map rewrite's decode failed
// there and counted none of the tool calls.
var errNotText = errors.New("oai: not a JSON string")

// runeText is a string field the estimate counts: the runes of the decoded
// JSON string, read in place (a tool call's arguments can be megabytes);
// null keeps the count, any other value is refused.
type runeText int

// UnmarshalJSON counts the runes of b.
func (n *runeText) UnmarshalJSON(b []byte) error {
	c, ok := stringRunes(b)
	if !ok {
		return errNotText
	}
	if !isNull(b) {
		*n = runeText(c)
	}
	return nil
}

// deltaRunes counts the generated runes of a delta (its members before the
// null drop).
func deltaRunes(dm []member) int {
	n := 0
	for _, k := range streamDeltaKeys {
		if at := findMember(dm, k); at >= 0 {
			if c, ok := stringRunes(dm[at].val); ok {
				n += c
			}
		}
	}
	if at := findMember(dm, "tool_calls"); at >= 0 {
		var calls []struct {
			Function struct {
				Name      runeText `json:"name"`
				Arguments runeText `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(dm[at].val, &calls) == nil {
			for _, tc := range calls {
				n += int(tc.Function.Name) + int(tc.Function.Arguments)
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
