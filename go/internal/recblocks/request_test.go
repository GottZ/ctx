package recblocks_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/recblocks"
)

const chatBody = `{"model": "m",
 "messages": [
  {"role": "system", "content": "You are terse."},
  {"role": "user", "name": "alice", "content": "Hi \"q\"\n\/x"},
  {"role": "assistant", "content": null, "reasoning_content": "think",
   "tool_calls": [
    {"id": "call_1", "type": "function", "function": {"name": "get", "arguments": "{\"a\":1}"}},
    {"id": "call_2", "type": "function", "function": {"name": "put", "arguments": {"b": 2}}},
    {"id": "call_3", "type": "custom"}]},
  {"role": "tool", "tool_call_id": "call_1", "content": "result text"},
  {"role": "function", "name": "get", "content": "legacy"},
  42,
  {"role": "user"},
  {"role": "assistant", "reasoning": "short", "content": "done"}
 ],
 "tools": [ {"type": "function", "function": {"name": "get"}} ],
 "temperature": 0.50 , "stream":false}`

func TestChatRequestMessages(t *testing.T) {
	r := recblocks.Decompose(request("chat", "/v1/chat/completions", chatBody))
	if r.Request.State != "present" || r.Request.Format != "json" || r.Request.Bytes != int64(len(chatBody)) {
		t.Fatalf("request side %+v", r.Request)
	}
	user := txt("request/messages/1", "user", "Hi \"q\"\n/x")
	user.Name = "alice"
	reasoning := txt("request/messages/2/reasoning", "assistant", "think")
	reasoning.Name = "reasoning"
	call := func(addr, name, id, content string) wb {
		w := txt(addr, "assistant", content)
		w.Kind, w.Name, w.ToolCallID = "tool_call", name, id
		return w
	}
	tool := txt("request/messages/3", "tool", "result text")
	tool.Kind, tool.ToolCallID = "tool_result", "call_1"
	fn := txt("request/messages/4", "function", "legacy")
	fn.Kind, fn.Name = "tool_result", "get"
	short := txt("request/messages/7/reasoning", "assistant", "short")
	short.Name = "reasoning"
	equalBlocks(t, blocksOf(t, r), []wb{
		txt("request/messages/0", "system", "You are terse."),
		user,
		reasoning,
		call("request/messages/2/tool_calls/0", "get", "call_1", `{"a":1}`),
		call("request/messages/2/tool_calls/1", "put", "call_2", `{"b": 2}`),
		call("request/messages/2/tool_calls/2", "", "call_3", `{"id": "call_3", "type": "custom"}`),
		tool,
		fn,
		js("request/messages/5", "42"),
		short,
		txt("request/messages/7", "assistant", "done"),
		js("request/tools", `[ {"type": "function", "function": {"name": "get"}} ]`),
		js("request/params", `{"model":"m","stream":false,"temperature":0.50}`),
	})
}

const partsBody = `{"messages":[
 {"role":"user","name":"bob","content":[
  {"type":"text","text":"look"},
  {"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},
  {"type":"image_url","image_url":{"url":"https://example.org/a.jpg","detail":"high"}},
  {"type":"image_url","image_url":"data:image/jpeg;base64,/9j/"},
  {"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}},
  {"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERi0=","filename":"a.pdf"}},
  {"type":"video_url","video_url":{"url":"x"}},
  {"type":"text","text":7},
  "bare"]},
 {"role":"tool","tool_call_id":"c9","content":[{"type":"text","text":"out"},{"type":"image_url","image_url":{"url":"http://h/i.png"}}]}
],"model":"vision"}`

func TestChatRequestContentParts(t *testing.T) {
	r := recblocks.Decompose(request("chat", "/v1/chat/completions", partsBody))
	const p = "request/messages/0/content/"
	named := func(w wb) wb { w.Name = "bob"; return w }
	payload := func(addr, kind, mt, content string, inline bool) wb {
		return named(wb{Address: addr, Side: "request", Kind: kind, Role: "user", MediaType: mt,
			Content: content, UTF8: true, Inline: inline})
	}
	jsonPart := func(addr, content string) wb {
		w := js(addr, content)
		w.Role = "user"
		return named(w)
	}
	toolText := txt("request/messages/1/content/0", "tool", "out")
	toolText.Kind, toolText.ToolCallID = "tool_result", "c9"
	toolImage := wb{Address: "request/messages/1/content/1", Side: "request", Kind: "image", Role: "tool",
		ToolCallID: "c9", Content: "http://h/i.png", UTF8: true, Inline: true}
	equalBlocks(t, blocksOf(t, r), []wb{
		named(txt(p+"0", "user", "look")),
		payload(p+"1", "image", "image/png", "data:image/png;base64,iVBORw0KGgo=", false),
		payload(p+"2", "image", "", "https://example.org/a.jpg", true),
		payload(p+"3", "image", "image/jpeg", "data:image/jpeg;base64,/9j/", false),
		payload(p+"4", "audio", "audio/wav", "UklGRg==", false),
		payload(p+"5", "file", "application/pdf",
			`{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERi0=","filename":"a.pdf"}}`, false),
		jsonPart(p+"6", `{"type":"video_url","video_url":{"url":"x"}}`),
		jsonPart(p+"7", `{"type":"text","text":7}`),
		jsonPart(p+"8", `"bare"`),
		toolText,
		toolImage,
		js("request/params", `{"model":"vision"}`),
	})
}

// A lone surrogate escape decodes to U+FFFD, as encoding/json yields it; the
// side address keeps the body byte for byte.
func TestChatRequestLoneSurrogate(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"a` + bs + `ud800b"}]}`
	in := request("chat", "/v1/chat/completions", body)
	r := recblocks.Decompose(in)
	equalBlocks(t, blocksOf(t, r), []wb{txt("request/messages/0", "user", "a"+string(rune(0xFFFD))+"b")})
	side, ok := recblocks.Lookup(r, in, "request")
	if !ok || string(side.Content()) != body {
		t.Fatalf("side read %v %q", ok, side.Content())
	}
}

func TestEmbedRequest(t *testing.T) {
	tokens := func(addr, content string) wb {
		w := js(addr, content)
		w.Kind, w.Translatable = "tokens", false
		return w
	}
	cases := []struct {
		name, op, path, body string
		want                 []wb
	}{
		{"string", "embed", "/v1/embeddings", `{"model":"e","input":"hello"}`,
			[]wb{txt("request/input", "", "hello"), js("request/params", `{"model":"e"}`)}},
		{"strings by path", "", "/v1/embeddings", `{"input":["a","b"],"model":"e","dimensions":256}`,
			[]wb{txt("request/input/0", "", "a"), txt("request/input/1", "", "b"),
				js("request/params", `{"dimensions":256,"model":"e"}`)}},
		{"one token list", "embed", "/v1/embeddings", `{"input":[1, 2,3]}`,
			[]wb{tokens("request/input", `[1, 2,3]`)}},
		{"token lists", "embed", "/v1/embeddings", `{"input":[[1,2],[3]]}`,
			[]wb{tokens("request/input/0", `[1,2]`), tokens("request/input/1", `[3]`)}},
		{"mixed", "embed", "/v1/embeddings", `{"input":["a",[1,2],{"x":1},3]}`,
			[]wb{txt("request/input/0", "", "a"), tokens("request/input/1", `[1,2]`),
				js("request/input/2", `{"x":1}`), tokens("request/input/3", `3`)}},
		{"object input", "embed", "/v1/embeddings", `{"input":{"x":1}}`,
			[]wb{js("request/input", `{"x":1}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			equalBlocks(t, blocksOf(t, recblocks.Decompose(request(c.op, c.path, c.body))), c.want)
		})
	}
}

const rerankBody = `{"model":"r","query":"q?","documents":["d0",{"text":"d1","id":7},{"x":1}],"top_n":2}`

func TestRerankRequest(t *testing.T) {
	for _, in := range []recblocks.Input{
		request("rerank", "/v1/rerank", rerankBody),
		request("", "/v2/rerank", rerankBody),
	} {
		equalBlocks(t, blocksOf(t, recblocks.Decompose(in)), []wb{
			txt("request/query", "", "q?"),
			txt("request/documents/0", "", "d0"),
			txt("request/documents/1", "", "d1"),
			js("request/documents/2", `{"x":1}`),
			js("request/params", `{"model":"r","top_n":2}`),
		})
	}
	equalBlocks(t, blocksOf(t, recblocks.Decompose(request("rerank", "/v1/rerank", `{"query":{"a":1}}`))),
		[]wb{js("request/query", `{"a":1}`)})
	equalBlocks(t, blocksOf(t, recblocks.Decompose(request("rerank", "/v1/rerank", `{"documents":"one"}`))),
		[]wb{js("request/documents", `"one"`)})
}

// A valid JSON body no route rule matches is one json block: the body
// unchanged.
func TestRequestFallbackOneBlock(t *testing.T) {
	cases := []struct{ name, op, path, body string }{
		{"unknown op", "foo", "/v1/chat/completions", `{"messages":[]}`},
		{"models with a body", "", "/v1/models", " {\"x\": 1} \n"},
		{"messages not an array", "chat", "/v1/chat/completions", `{"messages":"no"}`},
		{"chat array body", "chat", "/v1/chat/completions", `[1,2]`},
		{"embed without input", "embed", "/v1/embeddings", `{"model":"e"}`},
		{"rerank without query and documents", "rerank", "/v1/rerank", `{"top_n":1}`},
		{"unknown path", "", "/v1/other", `{"messages":[{"role":"user","content":"x"}]}`},
		{"op wins over path", "embed", "/v1/chat/completions", `{"messages":[{"role":"user","content":"x"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recblocks.Decompose(request(c.op, c.path, c.body))
			if r.Request.Format != "json" {
				t.Errorf("format %q", r.Request.Format)
			}
			equalBlocks(t, blocksOf(t, r), []wb{js("request", c.body)})
		})
	}
}

func TestChatRequestByPathAndEmpty(t *testing.T) {
	r := recblocks.Decompose(request("", "/v1/chat/completions", `{"messages":[{"role":"user","content":"x"}]}`))
	equalBlocks(t, blocksOf(t, r), []wb{txt("request/messages/0", "user", "x")})
	r = recblocks.Decompose(request("chat", "/v1/chat/completions", `{"messages":[]}`))
	equalBlocks(t, blocksOf(t, r), nil)
}

// Never repair: a body that is not valid UTF-8 or not valid JSON is one raw
// block holding its bytes unchanged.
func TestRequestRaw(t *testing.T) {
	cases := []struct {
		name   string
		body   []byte
		format string
	}{
		{"invalid utf-8 with a NUL", []byte{'{', '"', 'm', '"', ':', '"', 0xff, 0xfe, 0x00, '"', '}'}, "not_utf8"},
		// json.Valid accepts invalid UTF-8 inside a string: the UTF-8 rule
		// alone keeps this body from being decomposed (and repaired).
		{"invalid utf-8 in valid json", []byte("{\"messages\":[{\"role\":\"user\",\"content\":\"a\xffb\"}]}"), "not_utf8"},
		{"invalid json", []byte(`{"messages": [`), "not_json"},
		{"NUL in a string", []byte("{\"messages\":[{\"content\":\"x\x00y\"}]}"), "not_json"},
		{"two values", []byte(`{} {}`), "not_json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := recblocks.Input{Op: "chat", Path: "/v1/chat/completions", State: "done", Request: c.body, ResponseNull: true}
			r := recblocks.Decompose(in)
			if r.Request.State != "present" || r.Request.Format != c.format || r.Request.Bytes != int64(len(c.body)) {
				t.Fatalf("request side %+v", r.Request)
			}
			equalBlocks(t, blocksOf(t, r), []wb{rawOf("request", c.body)})
		})
	}
}
