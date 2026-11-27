package recblocks_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/recblocks"
)

const chatAnswer = `{"id":"chatcmpl-x","object":"chat.completion","choices":[
 {"index":0,"message":{"role":"assistant","content":"Hello","reasoning_content":"r0"},"finish_reason":"stop"},
 {"index":1,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}},
 {"index":2,"message":{"role":"assistant","content":[{"type":"text","text":"part"}]}}
],"usage":{"total_tokens":3}}`

func reasoningBlock(addr, role, content string) wb {
	w := txt(addr, role, content)
	w.Name = "reasoning"
	return w
}

func toolCallBlock(addr, role, name, id, content string) wb {
	w := txt(addr, role, content)
	w.Kind, w.Name, w.ToolCallID = "tool_call", name, id
	return w
}

func TestChatAnswer(t *testing.T) {
	r := recblocks.Decompose(answer("chat", "/v1/chat/completions", false, 200, `{"messages":[]}`, chatAnswer))
	if r.Response.State != "present" || r.Response.Format != "json" || r.Response.Bytes != int64(len(chatAnswer)) {
		t.Fatalf("response side %+v", r.Response)
	}
	if r.Response.StreamLines != 0 || r.Response.StreamLinesUnparsed != 0 {
		t.Errorf("stream counts on a non-stream answer: %+v", r.Response)
	}
	equalBlocks(t, sideBlocks(t, r, "response"), []wb{
		reasoningBlock("response/choices/0/reasoning", "assistant", "r0"),
		txt("response/choices/0/content", "assistant", "Hello"),
		toolCallBlock("response/choices/1/tool_calls/0", "assistant", "f", "c1", "{}"),
		txt("response/choices/2/content/0", "assistant", "part"),
	})
}

func TestChatAnswerChoiceIndexes(t *testing.T) {
	cases := []struct {
		name, body string
		want       []wb
	}{
		{"duplicate index falls back to positions",
			`{"choices":[{"index":0,"message":{"role":"assistant","content":"a"}},{"index":0,"message":{"role":"assistant","content":"b"}}]}`,
			[]wb{txt("response/choices/0/content", "assistant", "a"), txt("response/choices/1/content", "assistant", "b")}},
		{"distinct indexes in document order",
			`{"choices":[{"index":1,"message":{"content":"one"}},{"index":0,"message":{"content":"zero"}}]}`,
			[]wb{txt("response/choices/1/content", "", "one"), txt("response/choices/0/content", "", "zero")}},
		{"non-object choice and message",
			`{"choices":[7,{"index":1,"message":"str"},{"index":2}]}`,
			[]wb{js("response/choices/0", "7"), js("response/choices/1", `"str"`)}},
		{"index not a plain integer",
			`{"choices":[{"index":1.5,"message":{"content":"x"}},{"index":-1,"message":{"content":"y"}}]}`,
			[]wb{txt("response/choices/0/content", "", "x"), txt("response/choices/1/content", "", "y")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recblocks.Decompose(answer("chat", "/v1/chat/completions", false, 200, `{}`, c.body))
			equalBlocks(t, sideBlocks(t, r, "response"), c.want)
		})
	}
}

func TestAnswerError(t *testing.T) {
	errBlock := func(body string) wb {
		return wb{Address: "response/error", Side: "response", Kind: "error", Content: body,
			UTF8: true, Translatable: true, Inline: true}
	}
	cases := []struct {
		name   string
		status int
		stream bool
		body   string
		format string
		want   wb
	}{
		{"400 json", 400, false, `{"error":{"message":"bad"}}`, "json", errBlock(`{"error":{"message":"bad"}}`)},
		{"no status with a body", 0, false, "gone", "not_json", errBlock("gone")},
		{"503 text", 503, false, "upstream text", "not_json", errBlock("upstream text")},
		{"429 on a stream", 429, true, `{"error":{"message":"slow"}}`, "json", errBlock(`{"error":{"message":"slow"}}`)},
		{"500 invalid utf-8", 500, false, "bad \xff body", "not_utf8", rawOf("response", []byte("bad \xff body"))},
		{"300 is not 2xx", 300, false, `{}`, "json", errBlock(`{}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recblocks.Decompose(answer("chat", "/v1/chat/completions", c.stream, c.status, `{}`, c.body))
			if r.Response.Format != c.format || r.Response.StreamLines != 0 {
				t.Errorf("response side %+v, want format %q", r.Response, c.format)
			}
			equalBlocks(t, sideBlocks(t, r, "response"), []wb{c.want})
		})
	}
}

func TestOtherAnswers(t *testing.T) {
	results := js("response/results", `[{"index":0,"relevance_score":0.9}]`)
	results.Translatable = false
	cases := []struct {
		name, op, path, body string
		want                 wb
	}{
		{"rerank results", "rerank", "/v1/rerank", `{"id":"rerank-1","results":[{"index":0,"relevance_score":0.9}],"model":"r"}`, results},
		{"rerank by path", "", "/v2/rerank", `{"results":[{"index":0,"relevance_score":0.9}]}`, results},
		{"rerank without results", "rerank", "/v1/rerank", `{"model":"r"}`, js("response", `{"model":"r"}`)},
		{"embeddings body", "embed", "/v1/embeddings", `{"object":"list","data":[]}`, js("response", `{"object":"list","data":[]}`)},
		{"chat without choices", "chat", "/v1/chat/completions", ` {"id":"x"} `, js("response", ` {"id":"x"} `)},
		{"models", "", "/v1/models", `{"data":[]}`, js("response", `{"data":[]}`)},
		{"chat answer not json", "chat", "/v1/chat/completions", "plain", rawOf("response", []byte("plain"))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recblocks.Decompose(answer(c.op, c.path, false, 200, `{}`, c.body))
			equalBlocks(t, sideBlocks(t, r, "response"), []wb{c.want})
		})
	}
}

var streamBody = sse(
	`data: {"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
	`: keepalive`,
	`data: {"choices":[{"index":0,"delta":{"reasoning_content":"th"}},{"index":1,"delta":{"role":"assistant","content":"B1"}}]}`,
	`data: {"choices":[{"index":0,"delta":{"reasoning":"ink","reasoning_content":"ink"}}]}`,
	`data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
	`data:{"choices":[{"index":0,"delta":{"content":"lo \"w\" "}}]}`,
	`data: not json`,
	`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
	`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"","function":{"arguments":"\"x\"}"}}]}}]}`,
	`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_10","function":{"name":"second","arguments":""}}]}}]}`,
	`data: {"error":{"message":"boom"},"choices":[{"index":0,"delta":{"content":"IGNORED"}}]}`,
	`data: {"choices":[{"index":1,"delta":{"content":"B2"}}]}`,
	`data: [1,2]`,
	`data: {"choices":[{"delta":{"content":"!"}}]}`,
)

func TestStreamAssembly(t *testing.T) {
	r := recblocks.Decompose(answer("chat", "/v1/chat/completions", true, 200,
		`{"messages":[{"role":"user","content":"hi"}],"stream":true}`, streamBody))
	if r.Response.State != "present" || r.Response.Format != "json" ||
		r.Response.StreamLines != 13 || r.Response.StreamLinesUnparsed != 2 {
		t.Fatalf("response side %+v", r.Response)
	}
	equalBlocks(t, blocksOf(t, r), []wb{
		txt("request/messages/0", "user", "hi"),
		js("request/params", `{"stream":true}`),
		reasoningBlock("response/choices/0/reasoning", "assistant", "think"),
		txt("response/choices/0/content", "assistant", `Hello "w" !`),
		toolCallBlock("response/choices/0/tool_calls/0", "assistant", "lookup", "call_9", `{"q":"x"}`),
		toolCallBlock("response/choices/0/tool_calls/1", "assistant", "second", "call_10", ""),
		txt("response/choices/1/content", "assistant", "B1B2"),
		streamErrBlock("response/error/0", `{"error":{"message":"boom"},"choices":[{"index":0,"delta":{"content":"IGNORED"}}]}`),
	})
}

// streamErrBlock is the block of an error event inside a 2xx stream.
func streamErrBlock(addr, payload string) wb {
	return wb{Address: addr, Side: "response", Kind: "error", Content: payload, UTF8: true, Translatable: true, Inline: true}
}

// TestStreamErrorEvents: an error event inside a 2xx stream — a data line
// whose object carries an "error" object or a non-empty "error" string — is
// a block of its own,
// response/error/<n> in line order, holding the event's payload as the client
// got it, after the choices assembled around it; a stream of an error event
// alone is that block, not an answer without blocks.
func TestStreamErrorEvents(t *testing.T) {
	cases := []struct {
		name, body string
		want       []wb
	}{
		{"error alone", sse(`data: {"error":{"message":"boom"}}`),
			[]wb{streamErrBlock("response/error/0", `{"error":{"message":"boom"}}`)}},
		{"after content", sse(`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
			`data: {"error":{"message":"boom","code":"upstream_error"}}`),
			[]wb{txt("response/choices/0/content", "assistant", "Hel"),
				streamErrBlock("response/error/0", `{"error":{"message":"boom","code":"upstream_error"}}`)}},
		{"a string error and a second event", sse(`data: {"error":"first"}`, `data: {"error":{"message":"second"}}`),
			[]wb{streamErrBlock("response/error/0", `{"error":"first"}`), streamErrBlock("response/error/1", `{"error":{"message":"second"}}`)}},
		{"a null error is no event", sse(`data: {"error":null,"choices":[{"index":0,"delta":{"content":"x"}}]}`),
			[]wb{txt("response/choices/0/content", "assistant", "x")}},
		{"false, an empty string or a number is no event", sse(`data: {"error":false,"choices":[{"index":0,"delta":{"content":"a"}}]}`,
			`data: {"error":"","choices":[{"index":0,"delta":{"content":"b"}}]}`, `data: {"error":0,"choices":[{"index":0,"delta":{"content":"c"}}]}`),
			[]wb{txt("response/choices/0/content", "assistant", "abc")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recblocks.Decompose(answer("chat", "/v1/chat/completions", true, 200, `{}`, c.body))
			if r.Response.Format != "json" {
				t.Errorf("response side %+v", r.Response)
			}
			equalBlocks(t, sideBlocks(t, r, "response"), c.want)
		})
	}
}

// TestStreamRefusalAndFunctionCall: a streamed refusal and a streamed legacy
// function_call are put together like content and tool calls, and are the
// blocks a non-stream answer has for them — in a message's order: reasoning,
// content, refusal, tool calls, function_call.
func TestStreamRefusalAndFunctionCall(t *testing.T) {
	body := sse(
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","refusal":"I can"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"refusal":"'t help."}}]}`,
		`data: {"choices":[{"index":0,"delta":{"refusal":null}}]}`,
		`data: {"choices":[{"index":1,"delta":{"role":"assistant","function_call":{"name":"lookup","arguments":"{\"q\":"}}}]}`,
		`data: {"choices":[{"index":1,"delta":{"function_call":{"arguments":"1}"}}}]}`,
		`data: {"choices":[{"index":2,"delta":{"role":"assistant","function_call":{"name":"f","arguments":"{}"},"refusal":"no","content":"c","tool_calls":[{"index":0,"id":"t","function":{"name":"g","arguments":"1"}}]}}]}`,
	)
	r := recblocks.Decompose(answer("chat", "/v1/chat/completions", true, 200, `{}`, body))
	refusal := func(addr, content string) wb {
		w := txt(addr, "assistant", content)
		w.Name = "refusal"
		return w
	}
	equalBlocks(t, sideBlocks(t, r, "response"), []wb{
		refusal("response/choices/0/refusal", "I can't help."),
		toolCallBlock("response/choices/1/function_call", "assistant", "lookup", "", `{"q":1}`),
		txt("response/choices/2/content", "assistant", "c"),
		refusal("response/choices/2/refusal", "no"),
		toolCallBlock("response/choices/2/tool_calls/0", "assistant", "g", "t", "1"),
		toolCallBlock("response/choices/2/function_call", "assistant", "f", "", "{}"),
	})
}

func TestStreamVariants(t *testing.T) {
	cases := []struct {
		name            string
		body            string
		lines, unparsed int
		want            []wb
	}{
		{"crlf lines",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\r\n\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"}}]}\r\n\r\n",
			2, 0, []wb{txt("response/choices/0/content", "assistant", "ab")}},
		{"tool call index missing is its position",
			sse(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"1"}},{"id":"b","function":{"name":"g","arguments":"2"}}]}}]}`),
			1, 0, []wb{toolCallBlock("response/choices/0/tool_calls/0", "assistant", "f", "a", "1"),
				toolCallBlock("response/choices/0/tool_calls/1", "assistant", "g", "b", "2")}},
		{"tool call indexes ascending, object arguments raw",
			sse(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"id":"z","function":{"name":"h","arguments":{"k": 1}}}]}}]}`,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"y","function":{"name":"g","arguments":"2"}}]}}]}`),
			2, 0, []wb{toolCallBlock("response/choices/0/tool_calls/1", "assistant", "g", "y", "2"),
				toolCallBlock("response/choices/0/tool_calls/3", "assistant", "h", "z", `{"k": 1}`)}},
		{"choice indexes ascending, invalid index is 0",
			sse(`data: {"choices":[{"index":2,"delta":{"content":"two"}}]}`,
				`data: {"choices":[{"index":-1,"delta":{"role":"tool","content":"zero"}}]}`),
			2, 0, []wb{txt("response/choices/0/content", "tool", "zero"), txt("response/choices/2/content", "assistant", "two")}},
		{"empty content key seen", sse(`data: {"choices":[{"index":0,"delta":{"content":""}}]}`),
			1, 0, []wb{txt("response/choices/0/content", "assistant", "")}},
		{"null content is not seen", sse(`data: {"choices":[{"index":0,"delta":{"content":null}}]}`),
			1, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recblocks.Decompose(answer("chat", "/v1/chat/completions", true, 200, `{}`, c.body))
			if r.Response.Format != "json" || r.Response.StreamLines != c.lines || r.Response.StreamLinesUnparsed != c.unparsed {
				t.Errorf("response side %+v", r.Response)
			}
			equalBlocks(t, sideBlocks(t, r, "response"), c.want)
		})
	}
}

// A stream without a single parsed data line is one raw block.
func TestStreamNothingParsed(t *testing.T) {
	cases := []struct {
		name            string
		body            string
		lines, unparsed int
	}{
		{"unparsable line", sse("data: nope", ": ping"), 1, 1},
		{"no data line", sse(": ping"), 0, 0},
		{"a plain JSON answer", `{"choices":[]}`, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recblocks.Decompose(answer("chat", "/v1/chat/completions", true, 200, `{}`, c.body))
			if r.Response.Format != "not_json" || r.Response.StreamLines != c.lines || r.Response.StreamLinesUnparsed != c.unparsed {
				t.Errorf("response side %+v", r.Response)
			}
			equalBlocks(t, sideBlocks(t, r, "response"), []wb{rawOf("response", []byte(c.body))})
		})
	}
	bad := "data: {\"choices\":[{\"delta\":{\"content\":\"\xff\"}}]}\n\n"
	r := recblocks.Decompose(answer("chat", "/v1/chat/completions", true, 200, `{}`, bad))
	if r.Response.Format != "not_utf8" || r.Response.StreamLines != 0 {
		t.Errorf("invalid UTF-8 stream: %+v", r.Response)
	}
	equalBlocks(t, sideBlocks(t, r, "response"), []wb{rawOf("response", []byte(bad))})
}

// TestRefusalAndFunctionCall: a message's refusal and its legacy
// function_call are blocks of their own, in the request and in the answer.
func TestRefusalAndFunctionCall(t *testing.T) {
	req := `{"messages":[{"role":"assistant","content":null,"function_call":{"name":"lookup","arguments":"{\"q\":1}"}}]}`
	ans := `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}]}`
	r := recblocks.Decompose(answer("chat", "/v1/chat/completions", false, 200, req, ans))
	fc := toolCallBlock("request/messages/0/function_call", "assistant", "lookup", "", `{"q":1}`)
	equalBlocks(t, sideBlocks(t, r, "request"), []wb{fc})
	ref := txt("response/choices/0/refusal", "assistant", "I can't help with that.")
	ref.Name = "refusal"
	equalBlocks(t, sideBlocks(t, r, "response"), []wb{ref})
}
