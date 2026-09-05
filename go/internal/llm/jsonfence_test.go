package llm

import "testing"

// TestStripJSONFence pins the lenient variant. The row that carries the
// behaviour decision of Welle T04-13 is "trailing fence, never opened": an
// answer ending in ``` without opening one keeps its ```, because at that
// point the backticks are model content and not fence syntax. dream's
// recurrence parser used to swallow them and no longer does.
func TestStripJSONFence(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain object", `{"a":1}`, `{"a":1}`},
		{"plain array", `[1,2,3]`, `[1,2,3]`},
		{"json fence with newlines", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"json fence without newlines", "```json{\"a\":1}```", `{"a":1}`},
		{"bare fence", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"fence with surrounding whitespace", "  \n ```json\n{\"a\":1}\n```  \n ", `{"a":1}`},
		{"plain json with surrounding whitespace", "  \n\t{\"a\":1}\r\n  ", `{"a":1}`},
		{"opening fence, never closed", "```json\n{\"a\":1}", `{"a":1}`},
		{"trailing fence, never opened", "{\"a\":1} ```", "{\"a\":1} ```"},
		{"trailing fence, never opened, no space", "{\"a\":1}```", "{\"a\":1}```"},
		{"trailing fence, never opened, newline", "{\"a\":1}\n```", "{\"a\":1}\n```"},
		{"backticks inside a json string value", "{\"a\":\"x```y\"}", "{\"a\":\"x```y\"}"},
		{"empty", "", ""},
		{"whitespace only", "   \n\t ", ""},
		{"fence marker only", "```", ""},
		{"json fence marker only", "```json", ""},
		{"invalid utf8 body stays byte-identical", "\xff\xfe{\"a\":1}", "\xff\xfe{\"a\":1}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripJSONFence(tc.raw); got != tc.want {
				t.Errorf("StripJSONFence(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestStripJSONFenceIdempotent: applying the strip twice must not change the
// result. A second pass over an already unwrapped body is what happens when a
// caller chain grows a second tolerance point, and it must be a no-op.
func TestStripJSONFenceIdempotent(t *testing.T) {
	raws := []string{
		`{"a":1}`,
		"```json\n{\"a\":1}\n```",
		"```\n[1,2]\n```",
		"{\"a\":1} ```",
		"",
	}
	for _, raw := range raws {
		once := StripJSONFence(raw)
		if twice := StripJSONFence(once); twice != once {
			t.Errorf("not idempotent for %q: once=%q twice=%q", raw, once, twice)
		}
	}
}
