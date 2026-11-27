package recblocks_test

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/recblocks"
)

// wb is a block as a test compares it: every field plus the content.
type wb struct {
	Address, Side, Kind, Role, Name, ToolCallID, MediaType string
	Content                                                string
	UTF8, Translatable, Inline                             bool
}

func view(b recblocks.Block) wb {
	return wb{
		Address: b.Address, Side: b.Side, Kind: b.Kind, Role: b.Role, Name: b.Name,
		ToolCallID: b.ToolCallID, MediaType: b.MediaType, Content: string(b.Content()),
		UTF8: b.UTF8, Translatable: b.Translatable, Inline: b.Inline,
	}
}

func sideOf(addr string) string {
	side, _, _ := strings.Cut(addr, "/")
	return side
}

// txt is a text block of a valid UTF-8 content.
func txt(addr, role, content string) wb {
	return wb{Address: addr, Side: sideOf(addr), Kind: "text", Role: role, Content: content,
		UTF8: true, Translatable: true, Inline: true}
}

// js is a json block of a valid UTF-8 content.
func js(addr, content string) wb {
	return wb{Address: addr, Side: sideOf(addr), Kind: "json", Content: content,
		UTF8: true, Translatable: true, Inline: true}
}

// rawOf is the raw block of a whole side body.
func rawOf(side string, body []byte) wb {
	ok := utf8.Valid(body)
	return wb{Address: side, Side: side, Kind: "raw", Content: string(body),
		UTF8: ok, Translatable: ok, Inline: true}
}

// addrRE is the address grammar: a side, then fixed words or decimal
// indices without leading zeros.
var addrRE = regexp.MustCompile(`^(request|response)(/(messages|content|tool_calls|reasoning|tools|params|input|query|documents|choices|results|error|0|[1-9][0-9]*))*$`)

var kinds = map[string]bool{
	"text": true, "image": true, "audio": true, "file": true, "tool_call": true,
	"tool_result": true, "json": true, "tokens": true, "error": true, "raw": true,
}

// checkInvariants holds for every Result: unique, well-formed addresses,
// request blocks before response blocks, sizes and flags that match the
// content, nothing translatable that is not UTF-8, and no blocks of a side
// that is not present.
func checkInvariants(t *testing.T, r recblocks.Result) {
	t.Helper()
	seen := map[string]bool{}
	inResponse := false
	for _, b := range r.Blocks {
		if seen[b.Address] {
			t.Errorf("duplicate address %q", b.Address)
		}
		seen[b.Address] = true
		if !addrRE.MatchString(b.Address) {
			t.Errorf("address %q does not match the grammar", b.Address)
		}
		if b.Side != sideOf(b.Address) {
			t.Errorf("block %q: side %q", b.Address, b.Side)
		}
		if b.Side == "response" {
			inResponse = true
		} else if inResponse {
			t.Errorf("request block %q after a response block", b.Address)
		}
		if b.Bytes != len(b.Content()) {
			t.Errorf("block %q: Bytes %d, content %d", b.Address, b.Bytes, len(b.Content()))
		}
		if b.UTF8 != utf8.Valid(b.Content()) {
			t.Errorf("block %q: UTF8 %v disagrees with the content", b.Address, b.UTF8)
		}
		if b.Translatable && !b.UTF8 {
			t.Errorf("block %q: translatable without valid UTF-8", b.Address)
		}
		if !kinds[b.Kind] {
			t.Errorf("block %q: kind %q", b.Address, b.Kind)
		}
		if b.Side == "request" && r.Request.State != "present" {
			t.Errorf("block %q of a request side in state %q", b.Address, r.Request.State)
		}
		if b.Side == "response" && r.Response.State != "present" {
			t.Errorf("block %q of a response side in state %q", b.Address, r.Response.State)
		}
	}
}

// blocksOf checks the invariants and returns the blocks as the tests
// compare them.
func blocksOf(t *testing.T, r recblocks.Result) []wb {
	t.Helper()
	checkInvariants(t, r)
	out := make([]wb, 0, len(r.Blocks))
	for _, b := range r.Blocks {
		out = append(out, view(b))
	}
	return out
}

// sideBlocks returns the blocks of one side.
func sideBlocks(t *testing.T, r recblocks.Result, side string) []wb {
	t.Helper()
	var out []wb
	for _, b := range blocksOf(t, r) {
		if b.Side == side {
			out = append(out, b)
		}
	}
	return out
}

func equalBlocks(t *testing.T, got, want []wb) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d blocks, want %d", len(got), len(want))
	}
	for i := range max(len(got), len(want)) {
		switch {
		case i >= len(got):
			t.Errorf("missing block %d: %+v", i, want[i])
		case i >= len(want):
			t.Errorf("extra block %d: %+v", i, got[i])
		case got[i] != want[i]:
			t.Errorf("block %d:\n got  %+v\n want %+v", i, got[i], want[i])
		}
	}
}

// bs is one backslash: escapes like a lone surrogate are spelled with it at
// run time.
const bs = "\\"

// request is a done row with a request body and no answer.
func request(op, path, body string) recblocks.Input {
	return recblocks.Input{Op: op, Path: path, State: "done", Status: 200, Request: []byte(body), ResponseNull: true}
}

// answer is a done row with a request body and an answer.
func answer(op, path string, stream bool, status int, req, resp string) recblocks.Input {
	return recblocks.Input{Op: op, Path: path, Stream: stream, State: "done", Status: status,
		Request: []byte(req), Response: []byte(resp), ResponseBytes: int64(len(resp))}
}

// sse joins data and comment lines the way the gateway relays them: each
// followed by a blank line.
func sse(lines ...string) string {
	return strings.Join(lines, "\n\n") + "\n\n"
}

// corpus is one input of every rule: the determinism test and the fuzz seeds
// run over it.
func corpus() []recblocks.Input {
	deep := strings.Repeat("[", 20000) + strings.Repeat("]", 20000)
	return []recblocks.Input{
		request("chat", "/v1/chat/completions", chatBody),
		request("chat", "/v1/chat/completions", partsBody),
		request("", "/v1/chat/completions", `{"messages":[{"role":"user","content":"x"}]}`),
		request("embed", "/v1/embeddings", `{"input":["a",[1,2],{"x":1},3],"model":"e"}`),
		request("embed", "/v1/embeddings", `{"input":[1,2,3]}`),
		request("rerank", "/v2/rerank", rerankBody),
		request("", "/v1/models", ` {"x": 1} `),
		request("chat", "/v1/chat/completions", "{\"m\":\"\xff\xfe\x00\"}"),
		request("chat", "/v1/chat/completions", `{"messages": [`),
		request("chat", "/v1/chat/completions", deep),
		answer("chat", "/v1/chat/completions", true, 200, `{"messages":[]}`, streamBody),
		answer("chat", "/v1/chat/completions", false, 200, `{"messages":[]}`, chatAnswer),
		answer("chat", "/v1/chat/completions", false, 400, `{}`, `{"error":{"message":"bad"}}`),
		answer("rerank", "/v1/rerank", false, 200, rerankBody, `{"results":[{"index":0,"relevance_score":0.5}]}`),
		answer("chat", "/v1/chat/completions", true, 200, `{}`, sse("data: nope", ": ping")),
		{Op: "chat", State: "open", Request: []byte(`{"messages":[]}`), ResponseNull: true},
		{Op: "chat", State: "done", RequestNull: true, ResponseNull: true, ResponseBytes: 99},
	}
}
