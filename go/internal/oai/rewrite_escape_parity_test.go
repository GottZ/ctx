package oai

// The escape parity of the non-stream rewrite walk (RC-D S2, cluster C8,
// FOLGE-S; condition of the main context for the gosec G705 exception at
// the route's answer write, chat.go writeAnswer): the walk's client body is
// byte for byte what the base's map rewrite wrote through json.Marshal —
// the encoder gosec's taint check knows as the HTML escape — for every
// escape class a string can carry: each control character (as the \u00XX
// and the short escapes an upstream must send), <, > and & raw and escaped,
// U+2028 and U+2029 raw and escaped, invalid UTF-8 bytes, a lone surrogate
// escape, a surrogate pair, quote and backslash, '/' and its escape — in a
// message's content, in a tool call's arguments string, in a member name,
// in usage, and in the id and the alias the route sets. TestRewriteChatIsTheBase
// and FuzzRewriteChatIsTheBase run the general corpus; this pin hits the
// classes on purpose.

import (
	"fmt"
	"strings"
	"testing"
)

// escapeParityStrings are JSON string bodies (between the quotes, as an
// upstream sends them) of every escape class.
func escapeParityStrings() []string {
	var out []string
	for c := 0; c < 0x20; c++ {
		out = append(out, fmt.Sprintf(`a\u%04xb`, c))
	}
	out = append(out,
		`\b\f\n\r\t`, `\"`, `\\`, `/`, `\/`,
		`<script>&amp;</script>`, `\u003c\u003e\u0026`, `\u003C\u003E\u0026`,
		"\u2028\u2029", `\u2028\u2029`,
		"\xff\xfe", "a\xc3", "\xed\xa0\x80",
		`\ud800`, `\udc00x`, `\ud83d\ude00`, "😀ä€",
		`\u0000`, "\x7f", `\u007f`,
	)
	return out
}

// TestRewriteChatEscapeParity runs every class through every position of
// an answer and through the id and the alias, with reasoning on and off.
func TestRewriteChatEscapeParity(t *testing.T) {
	answers := 0
	for _, s := range escapeParityStrings() {
		for _, up := range []string{
			`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"` + s + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`{"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{\"q\":\"` + strings.ReplaceAll(s, `\`, `\\`) + `\"}"}}]},"finish_reason":"tool_calls","logprobs":null}]}`,
			`{"choices":[{"index":0,"message":{"role":"assistant","content":"x","reasoning_content":"` + s + `","` + s + `":"k"}}],"` + s + `":1}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":0,"` + s + `":"` + s + `"}}}`,
		} {
			for _, off := range []bool{false, true} {
				folgeSChatSame(t, []byte(up), "", off)
				answers++
			}
		}
		// The id and the alias the route sets carry the class too.
		up := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"}}]}`)
		id, alias := "chatcmpl-"+s, "alias-"+s
		got, gerr := rewriteChat(up, id, 1700000000, alias, "bulk", false)
		want, werr := baseRewriteChat(up, id, 1700000000, alias, "bulk", false)
		if string(got) != string(want) || fmt.Sprint(gerr) != fmt.Sprint(werr) {
			t.Errorf("id/alias %q:\n got %q err=%v\nbase %q err=%v", s, got, gerr, want, werr)
		}
		answers++
	}
	t.Logf("escape parity: %d answers over %d escape classes, byte-equal to json.Marshal's base", answers, len(escapeParityStrings()))
}
