package recblocks

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxMediaTypeLen bounds the media type a block names from a data URL or an
// audio format field: RFC 6838 allows 127 characters each for type and
// subtype, so a longer one is no media type and the block names none. A
// fixed bound of the format, not a policy.
const maxMediaTypeLen = 255

// request decomposes a present request body and returns its format.
func (b *builder) request(in Input, body []byte) string {
	if !utf8.Valid(body) {
		b.raw(body)
		return formatNotUTF8
	}
	if !json.Valid(body) {
		b.raw(body)
		return formatNotJSON
	}
	if !b.routedRequest(route(in), trimWS(body)) {
		b.jsonBlock(sideRequest, body, true)
	}
	return formatJSON
}

// routedRequest applies the rule of the route to a valid JSON body; false
// when the body is not what the route reads (nothing added then).
func (b *builder) routedRequest(rt string, body []byte) bool {
	o, ok := parseObject(body)
	if !ok {
		return false
	}
	switch rt {
	case opChat:
		return b.chatRequest(o)
	case opEmbed:
		return b.embedRequest(o)
	case opRerank:
		return b.rerankRequest(o)
	}
	return false
}

// chatRequest: an object with an array "messages".
func (b *builder) chatRequest(o object) bool {
	v, ok := o.get("messages")
	if !ok {
		return false
	}
	msgs, ok := parseArray(v)
	if !ok {
		return false
	}
	for i, m := range msgs {
		addr := "request/messages/" + strconv.Itoa(i)
		b.message(addr, addr, m)
	}
	if t, ok := o.get("tools"); ok {
		b.jsonBlock("request/tools", t, true)
	}
	b.params(o, "messages", "tools")
	return true
}

// embedRequest: an object with "input".
func (b *builder) embedRequest(o object) bool {
	in, ok := o.get("input")
	if !ok {
		return false
	}
	const addr = "request/input"
	if s, ok := decodeString(in); ok {
		b.text(Block{Address: addr, Kind: kindText}, s)
	} else if elems, ok := parseArray(in); !ok {
		b.jsonBlock(addr, in, true)
	} else if allNumbers(elems) {
		b.tokens(addr, in)
	} else {
		for i, e := range elems {
			b.embedInput(addr+"/"+strconv.Itoa(i), e)
		}
	}
	b.params(o, "input")
	return true
}

// embedInput is one element of an input array that is not one token list:
// a string, a token list (or a lone token), or other JSON.
func (b *builder) embedInput(addr string, e []byte) {
	if s, ok := decodeString(e); ok {
		b.text(Block{Address: addr, Kind: kindText}, s)
		return
	}
	if isNumber(e) {
		b.tokens(addr, e)
		return
	}
	if elems, ok := parseArray(e); ok && allNumbers(elems) {
		b.tokens(addr, e)
		return
	}
	b.jsonBlock(addr, e, true)
}

// tokens appends a token list, its raw JSON: not translatable.
func (b *builder) tokens(addr string, raw []byte) {
	b.add(Block{Address: addr, Kind: kindTokens, Inline: true}, raw)
}

// rerankRequest: an object with "query" and/or "documents".
func (b *builder) rerankRequest(o object) bool {
	q, hasQ := o.get("query")
	d, hasD := o.get("documents")
	if !hasQ && !hasD {
		return false
	}
	if hasQ {
		b.textOrJSON("request/query", q)
	}
	if hasD {
		b.documents(d)
	}
	b.params(o, "query", "documents")
	return true
}

func (b *builder) documents(d []byte) {
	docs, ok := parseArray(d)
	if !ok {
		b.jsonBlock("request/documents", d, true)
		return
	}
	for i, doc := range docs {
		addr := "request/documents/" + strconv.Itoa(i)
		if o, ok := parseObject(doc); ok {
			if s, ok := o.str("text"); ok {
				b.text(Block{Address: addr, Kind: kindText}, s)
				continue
			}
		}
		b.textOrJSON(addr, doc)
	}
}

// textOrJSON appends a string value as text, any other as raw JSON.
func (b *builder) textOrJSON(addr string, v []byte) {
	if s, ok := decodeString(v); ok {
		b.text(Block{Address: addr, Kind: kindText}, s)
		return
	}
	b.jsonBlock(addr, v, true)
}

// params appends the request's remaining top-level members — all but skip —
// as one object: keys sorted ascending, each key and value as written,
// compact; the last occurrence of a key named twice. None remaining: none.
func (b *builder) params(o object, skip ...string) {
	last := make(map[string]member, len(o))
	for _, m := range o {
		if !slices.Contains(skip, m.key) {
			last[m.key] = m
		}
	}
	if len(last) == 0 {
		return
	}
	keys := make([]string, 0, len(last))
	for k := range last {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(last[k].rawKey)
		buf.WriteByte(':')
		buf.Write(last[k].val)
	}
	buf.WriteByte('}')
	b.jsonBlock("request/params", buf.Bytes(), true)
}

// msgMeta is what every content block of one message carries.
type msgMeta struct {
	role, name, toolCallID string
	kind                   string // text, or tool_result for a tool or function message
}

// message decomposes one chat message at base (request/messages/{i} or
// response/choices/{idx}); textAddr is the address of a string content
// (the message's own in a request, base/content in an answer). Its blocks
// come in a fixed order: reasoning, content, refusal, tool calls, the
// legacy function_call.
func (b *builder) message(base, textAddr string, raw []byte) {
	o, ok := parseObject(raw)
	if !ok {
		b.jsonBlock(base, raw, true)
		return
	}
	m := msgMeta{role: o.text("role"), name: o.text("name"), kind: kindText}
	if m.role == "tool" || m.role == "function" {
		m.kind, m.toolCallID = kindToolResult, o.text("tool_call_id")
	}
	if r, ok := reasoningOf(o); ok {
		b.text(Block{Address: base + "/reasoning", Kind: kindText, Role: m.role, Name: "reasoning"}, r)
	}
	if c, ok := o.get("content"); ok && !isNull(c) {
		b.content(base, textAddr, c, m)
	}
	if r, ok := o.str("refusal"); ok {
		b.text(Block{Address: base + "/refusal", Kind: kindText, Role: m.role, Name: "refusal"}, r)
	}
	if tc, ok := o.get("tool_calls"); ok && !isNull(tc) {
		b.toolCalls(base, m.role, tc)
	}
	if fc, ok := o.get("function_call"); ok && !isNull(fc) {
		b.functionCall(base+"/function_call", m.role, fc)
	}
}

// functionCall is the legacy single call of a message ({name, arguments}):
// a tool_call block like toolCall's, without an id.
func (b *builder) functionCall(addr, role string, raw []byte) {
	blk := Block{Address: addr, Kind: kindToolCall, Role: role}
	o, ok := parseObject(raw)
	if !ok {
		b.text(blk, raw)
		return
	}
	blk.Name = o.text("name")
	args, ok := o.get("arguments")
	if !ok {
		b.text(blk, raw)
		return
	}
	if s, ok := decodeString(args); ok {
		b.text(blk, s)
		return
	}
	b.text(blk, args)
}

// reasoningOf is a message's or a delta's reasoning: "reasoning_content",
// else "reasoning", when it is a string.
func reasoningOf(o object) ([]byte, bool) {
	if s, ok := o.str("reasoning_content"); ok {
		return s, true
	}
	return o.str("reasoning")
}

// content decomposes a message's content: a string, or an array of parts.
func (b *builder) content(base, textAddr string, c []byte, m msgMeta) {
	if s, ok := decodeString(c); ok {
		b.text(Block{Address: textAddr, Kind: m.kind, Role: m.role, Name: m.name, ToolCallID: m.toolCallID}, s)
		return
	}
	parts, ok := parseArray(c)
	if !ok {
		b.add(Block{Address: textAddr, Kind: kindJSON, Role: m.role, Name: m.name, ToolCallID: m.toolCallID,
			Translatable: true, Inline: true}, c)
		return
	}
	for j, p := range parts {
		b.part(base+"/content/"+strconv.Itoa(j), p, m)
	}
}

// part decomposes one content part.
func (b *builder) part(addr string, raw []byte, m msgMeta) {
	blk := Block{Address: addr, Role: m.role, Name: m.name, ToolCallID: m.toolCallID}
	o, ok := parseObject(raw)
	if ok {
		switch o.text("type") {
		case "text":
			if s, ok := o.str("text"); ok {
				blk.Kind = m.kind
				b.text(blk, s)
				return
			}
		case "image_url":
			b.imagePart(blk, o, raw)
			return
		case "input_audio":
			b.audioPart(blk, o, raw)
			return
		case "file":
			b.filePart(blk, o, raw)
			return
		}
	}
	blk.Kind, blk.Translatable, blk.Inline = kindJSON, true, true
	b.add(blk, raw)
}

// imagePart: the URL, inline only when it is not a data URL; a part without
// a URL string is its raw JSON, not inline.
func (b *builder) imagePart(blk Block, o object, raw []byte) {
	blk.Kind = kindImage
	url, ok := o.str("image_url")
	if !ok {
		if io, isObj := o.obj("image_url"); isObj {
			url, ok = io.str("url")
		}
	}
	if !ok {
		b.add(blk, raw)
		return
	}
	mt, isData := dataURLMediaType(url)
	blk.MediaType, blk.Inline = mt, !isData
	b.add(blk, url)
}

// audioPart: the base64 data, the media type from the format field.
func (b *builder) audioPart(blk Block, o object, raw []byte) {
	blk.Kind = kindAudio
	a, ok := o.obj("input_audio")
	if !ok {
		b.add(blk, raw)
		return
	}
	if f, ok := a.str("format"); ok && len(f) > 0 && len("audio/")+len(f) <= maxMediaTypeLen {
		blk.MediaType = "audio/" + string(f)
	}
	if d, ok := a.str("data"); ok {
		b.add(blk, d)
		return
	}
	b.add(blk, raw)
}

// filePart: the raw part, the media type from a data URL in file.file_data.
func (b *builder) filePart(blk Block, o object, raw []byte) {
	blk.Kind = kindFile
	if f, ok := o.obj("file"); ok {
		if d, ok := f.str("file_data"); ok {
			blk.MediaType, _ = dataURLMediaType(d)
		}
	}
	b.add(blk, raw)
}

// dataURLMediaType reports whether s is a data URL ("data:", any case) and
// returns its media type: the header up to the first ';' or ',' ("" when
// there is none within maxMediaTypeLen bytes).
func dataURLMediaType(s []byte) (string, bool) {
	const scheme = "data:"
	if len(s) < len(scheme) || !strings.EqualFold(string(s[:len(scheme)]), scheme) {
		return "", false
	}
	head := s[len(scheme):]
	head = head[:min(len(head), maxMediaTypeLen+1)]
	end := bytes.IndexAny(head, ";,")
	if end < 0 {
		return "", true
	}
	return string(head[:end]), true
}

// toolCalls decomposes a message's tool calls at base/tool_calls/{k}.
func (b *builder) toolCalls(base, role string, v []byte) {
	calls, ok := parseArray(v)
	if !ok {
		b.jsonBlock(base+"/tool_calls", v, true)
		return
	}
	for k, c := range calls {
		b.toolCall(base+"/tool_calls/"+strconv.Itoa(k), role, c)
	}
}

// toolCall: the function's arguments — decoded when they are a JSON string,
// else as written; the whole call as written when it has no function object
// or the function no arguments.
func (b *builder) toolCall(addr, role string, raw []byte) {
	blk := Block{Address: addr, Kind: kindToolCall, Role: role}
	o, ok := parseObject(raw)
	if !ok {
		b.text(blk, raw)
		return
	}
	blk.ToolCallID = o.text("id")
	fn, ok := o.obj("function")
	if !ok {
		b.text(blk, raw)
		return
	}
	blk.Name = fn.text("name")
	args, ok := fn.get("arguments")
	if !ok {
		b.text(blk, raw)
		return
	}
	if s, ok := decodeString(args); ok {
		b.text(blk, s)
		return
	}
	b.text(blk, args)
}
