package recblocks

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"unicode/utf8"
)

// defaultStreamRole is the role of a streamed choice whose deltas name none.
const defaultStreamRole = "assistant"

func is2xx(status int) bool { return status >= 200 && status <= 299 }

// response decomposes a present answer body and sets its side's format (and
// a stream's line counts). Invalid UTF-8 is raw whatever the status; an
// answer that is not a 2xx is the error the client got.
func (b *builder) response(in Input, body []byte, side *Side) {
	if !utf8.Valid(body) {
		side.Format = formatNotUTF8
		b.raw(body)
		return
	}
	if !is2xx(in.Status) {
		side.Format = bodyFormat(body)
		b.text(Block{Address: "response/error", Kind: kindError}, body)
		return
	}
	rt := route(in)
	if in.Stream && rt == opChat {
		b.stream(body, side)
		return
	}
	if !json.Valid(body) {
		side.Format = formatNotJSON
		b.raw(body)
		return
	}
	side.Format = formatJSON
	if !b.routedResponse(rt, trimWS(body)) {
		b.jsonBlock(sideResponse, body, true)
	}
}

// routedResponse applies the rule of the route to a valid JSON 2xx answer;
// false when the answer is not what the route reads (nothing added then).
func (b *builder) routedResponse(rt string, body []byte) bool {
	o, ok := parseObject(body)
	if !ok {
		return false
	}
	switch rt {
	case opChat:
		return b.chatResponse(o)
	case opRerank:
		res, ok := o.get("results")
		if ok {
			b.jsonBlock("response/results", res, false)
		}
		return ok
	}
	return false
}

// chatResponse: an object with an array "choices", each choice's message at
// response/choices/{idx}.
func (b *builder) chatResponse(o object) bool {
	v, ok := o.get("choices")
	if !ok {
		return false
	}
	raws, ok := parseArray(v)
	if !ok {
		return false
	}
	choices := make([]object, len(raws))
	isObj := make([]bool, len(raws))
	for c, raw := range raws {
		choices[c], isObj[c] = parseObject(raw)
	}
	idx := choiceIndexes(choices, isObj)
	for c, raw := range raws {
		base := "response/choices/" + strconv.Itoa(idx[c])
		if !isObj[c] {
			b.jsonBlock(base, raw, true)
			continue
		}
		if msg, ok := choices[c].get("message"); ok && !isNull(msg) {
			b.message(base, base+"/content", msg)
		}
	}
	return true
}

// choiceIndexes are the address indices of an answer's choices: their
// "index" values when every choice is an object with a distinct plain
// index, else their positions — an address is unique either way.
func choiceIndexes(choices []object, isObj []bool) []int {
	idx := make([]int, len(choices))
	positions := make([]int, len(choices))
	for c := range positions {
		positions[c] = c
	}
	seen := make(map[int]bool, len(choices))
	for c := range choices {
		if !isObj[c] {
			return positions
		}
		v, ok := choices[c].get("index")
		if !ok {
			return positions
		}
		n, ok := plainIndex(v)
		if !ok || seen[n] {
			return positions
		}
		seen[n], idx[c] = true, n
	}
	return idx
}

// streamCall is one tool call assembled from a stream's deltas.
type streamCall struct {
	id, name string
	args     []byte
}

// streamChoice is one choice assembled from a stream's deltas.
type streamChoice struct {
	role                                    string
	content, reasoning, refusal             []byte
	contentSeen, reasoningSeen, refusalSeen bool
	calls                                   map[int]*streamCall
	// function is the legacy function_call, nil when no delta carried one.
	function *streamCall
}

// stream assembles a 2xx chat stream — the relayed "data:" lines, one JSON
// chunk each — per choice, then appends its error events (a chunk carrying
// an "error" that is not null) as blocks response/error/<n> in line order;
// without a single parsed line it is one raw block. The error events are
// found again in a second walk rather than kept from the first, so a body of
// many of them costs a walk, never a list of its size.
func (b *builder) stream(body []byte, side *Side) {
	choices := map[int]*streamChoice{}
	parsed, events := 0, 0
	walkStream(body, func(_ []byte, chunk object, ok bool) {
		side.StreamLines++
		if !ok {
			side.StreamLinesUnparsed++
			return
		}
		parsed++
		if isErrorEvent(chunk) {
			events++
			return
		}
		assembleChunk(choices, chunk)
	})
	if parsed == 0 {
		side.Format = formatNotJSON
		b.raw(body)
		return
	}
	side.Format = formatJSON
	for _, idx := range sortedKeys(choices) {
		b.streamChoice("response/choices/"+strconv.Itoa(idx), choices[idx])
	}
	if events == 0 {
		return
	}
	n := 0
	walkStream(body, func(payload []byte, chunk object, ok bool) {
		if ok && isErrorEvent(chunk) {
			b.text(Block{Address: "response/error/" + strconv.Itoa(n), Kind: kindError}, payload)
			n++
		}
	})
}

// walkStream calls fn for every "data:" line of a stream body with its
// trimmed payload and, when the payload is one JSON object, that object
// (ok=false otherwise).
func walkStream(body []byte, fn func(payload []byte, chunk object, ok bool)) {
	for rest := body; len(rest) > 0; {
		var line []byte
		line, rest, _ = bytes.Cut(rest, []byte{'\n'})
		payload, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		payload = trimWS(payload)
		if !json.Valid(payload) {
			fn(payload, nil, false)
			continue
		}
		chunk, ok := parseObject(payload)
		fn(payload, chunk, ok)
	}
}

// isErrorEvent reports whether a stream chunk is an error event: it carries
// an "error" that is not null. Its choices, if any, are not assembled — the
// event's block holds the whole chunk.
func isErrorEvent(chunk object) bool {
	e, ok := chunk.get("error")
	return ok && !isNull(e)
}

// assembleChunk adds one chunk's deltas to their choices.
func assembleChunk(choices map[int]*streamChoice, chunk object) {
	v, ok := chunk.get("choices")
	if !ok {
		return
	}
	raws, _ := parseArray(v)
	for _, raw := range raws {
		c, ok := parseObject(raw)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := c.get("index"); ok {
			if n, ok := plainIndex(v); ok {
				idx = n
			}
		}
		delta, ok := c.obj("delta")
		if !ok {
			continue
		}
		sc := choices[idx]
		if sc == nil {
			sc = &streamChoice{calls: map[int]*streamCall{}}
			choices[idx] = sc
		}
		sc.addDelta(delta)
	}
}

// addDelta appends a delta's strings, byte for byte.
func (sc *streamChoice) addDelta(d object) {
	if sc.role == "" {
		sc.role = d.text("role")
	}
	if s, ok := d.str("content"); ok {
		sc.content, sc.contentSeen = append(sc.content, s...), true
	}
	if s, ok := reasoningOf(d); ok {
		sc.reasoning, sc.reasoningSeen = append(sc.reasoning, s...), true
	}
	if s, ok := d.str("refusal"); ok {
		sc.refusal, sc.refusalSeen = append(sc.refusal, s...), true
	}
	if fc, ok := d.obj("function_call"); ok {
		if sc.function == nil {
			sc.function = &streamCall{}
		}
		sc.function.addFunction(fc)
	}
	v, ok := d.get("tool_calls")
	if !ok {
		return
	}
	raws, _ := parseArray(v)
	for p, raw := range raws {
		if tc, ok := parseObject(raw); ok {
			sc.addCallDelta(p, tc)
		}
	}
}

// addCallDelta adds one tool call delta at position p of its chunk: keyed by
// its "index" (its position without one); id and name from the first delta
// that carries them, the arguments concatenated (a non-string value as
// written).
func (sc *streamChoice) addCallDelta(p int, tc object) {
	k := p
	if v, ok := tc.get("index"); ok {
		if n, ok := plainIndex(v); ok {
			k = n
		}
	}
	call := sc.calls[k]
	if call == nil {
		call = &streamCall{}
		sc.calls[k] = call
	}
	if call.id == "" {
		call.id = tc.text("id")
	}
	if fn, ok := tc.obj("function"); ok {
		call.addFunction(fn)
	}
}

// addFunction adds one function delta ({name, arguments}) — of a tool call,
// or a legacy function_call: the name from the first delta that carries one,
// the arguments concatenated (a non-string value as written).
func (call *streamCall) addFunction(fn object) {
	if call.name == "" {
		call.name = fn.text("name")
	}
	args, ok := fn.get("arguments")
	if !ok || isNull(args) {
		return
	}
	if s, ok := decodeString(args); ok {
		call.args = append(call.args, s...)
		return
	}
	call.args = append(call.args, args...)
}

// streamChoice appends one assembled choice in a message's order:
// reasoning, content, refusal, the tool calls by index ascending, the legacy
// function_call.
func (b *builder) streamChoice(base string, sc *streamChoice) {
	role := sc.role
	if role == "" {
		role = defaultStreamRole
	}
	if sc.reasoningSeen {
		b.text(Block{Address: base + "/reasoning", Kind: kindText, Role: role, Name: "reasoning"}, nonNil(sc.reasoning))
	}
	if sc.contentSeen {
		b.text(Block{Address: base + "/content", Kind: kindText, Role: role}, nonNil(sc.content))
	}
	if sc.refusalSeen {
		b.text(Block{Address: base + "/refusal", Kind: kindText, Role: role, Name: "refusal"}, nonNil(sc.refusal))
	}
	for _, k := range sortedKeys(sc.calls) {
		call := sc.calls[k]
		b.text(Block{Address: base + "/tool_calls/" + strconv.Itoa(k), Kind: kindToolCall, Role: role,
			Name: call.name, ToolCallID: call.id}, nonNil(call.args))
	}
	if sc.function != nil {
		b.text(Block{Address: base + "/function_call", Kind: kindToolCall, Role: role, Name: sc.function.name}, nonNil(sc.function.args))
	}
}

// nonNil is b, or an empty slice for nil: an assembled block is never nil.
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
