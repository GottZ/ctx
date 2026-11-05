package oai_test

// Pins of the hardening after Fremd-Review 2 (W02-07, H-2 = F2-2): on every
// level whose keys the parse checks, a repeated key or a key that differs
// from a known key of the level only in letter case is refused (400, 0 wire
// hits) — the checked value is the sent value. Free payloads keep their
// keys as sent (a JSON schema, a tool's parameters).

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// dupMsgs wraps one message object into a request for model.
func dupMsgs(model, msg string, extra ...string) string {
	b := `{"model":"` + model + `","messages":[` + msg + `]`
	for _, e := range extra {
		b += "," + e
	}
	return b + "}"
}

const dupImage = `data:image/png;base64,AAAA`

// manyKeys is n distinct members "k0":1 … "k<n-1>":1.
func manyKeys(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = `"k` + strconv.Itoa(i) + `":1`
	}
	return strings.Join(parts, ",")
}

// TestDuplicateAndCaseVariantKeysAreRefused: the review's two inputs and the
// briefed variants, one level each; every case 400 with the key's path as
// param and no wire contact.
func TestDuplicateAndCaseVariantKeysAreRefused(t *testing.T) {
	logitDup := `"logit_bias":{` + strings.Repeat(`"1":1,`, 64) + `"1":1}`
	for _, tc := range []struct {
		name, body, code, param string
	}{
		{"image_url: http url before a data url (review input 1)",
			dupMsgs("vis", `{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://127.0.0.1:9/dup.png","url":"`+dupImage+`"}}]}`),
			oai.CodeInvalidJSON, "messages[0].content[0].image_url.url"},
		{"image_url: escaped repeat of url",
			dupMsgs("vis", `{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+dupImage+`","url":"http://127.0.0.1:9/x.png"}}]}`),
			oai.CodeInvalidJSON, "messages[0].content[0].image_url.url"},
		{"response_format: Type next to type (review input 2)",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"response_format":{"type":"structural_tag","Type":"json_object"}`),
			oai.CodeUnsupportedParameter, "response_format.Type"},
		{"response_format: Type alone",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"response_format":{"Type":"json_object"}`),
			oai.CodeUnsupportedParameter, "response_format.Type"},
		{"response_format: escaped \\u0054ype next to type",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"response_format":{"type":"text","\u0054ype":"json_object"}`),
			oai.CodeUnsupportedParameter, "response_format.Type"},
		{"tool hull: a repeated vendor key behind 20 others",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"tools":[{"type":"function","function":{"name":"f"},`+manyKeys(20)+`,"k0":2}]`),
			oai.CodeInvalidJSON, "tools[0].k0"},
		{"response_format: repeated type",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"response_format":{"type":"json_object","type":"structural_tag"}`),
			oai.CodeInvalidJSON, "response_format.type"},
		{"response_format.json_schema: repeated name",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"response_format":{"type":"json_schema","json_schema":{"name":"a","name":"b","schema":{}}}`),
			oai.CodeInvalidJSON, "response_format.json_schema.name"},
		{"response_format.json_schema: Schema next to schema",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"response_format":{"type":"json_schema","json_schema":{"name":"a","schema":{},"Schema":{}}}`),
			oai.CodeUnsupportedParameter, "response_format.json_schema.Schema"},
		{"message: repeated content",
			dupMsgs("chat", `{"role":"user","content":"FIRSTC","content":"LASTC"}`), oai.CodeInvalidJSON, "messages[0].content"},
		{"message: repeated role",
			dupMsgs("chat", `{"role":"user","role":"system","content":"hi"}`), oai.CodeInvalidJSON, "messages[0].role"},
		{"message: Role",
			dupMsgs("chat", `{"role":"user","Role":"system","content":"hi"}`), oai.CodeUnsupportedParameter, "messages[0].Role"},
		{"top level: repeated model",
			`{"model":"chat","model":"chat","messages":[{"role":"user","content":"hi"}]}`, oai.CodeInvalidJSON, "model"},
		{"top level: Messages",
			`{"model":"chat","Messages":[{"role":"user","content":"x"}],"messages":[{"role":"user","content":"hi"}]}`,
			oai.CodeUnsupportedParameter, "Messages"},
		{"part: repeated text",
			dupMsgs("chat", `{"role":"user","content":[{"type":"text","text":"a","text":"b"}]}`), oai.CodeInvalidJSON, "messages[0].content[0].text"},
		{"tool hull: Type next to type",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"tools":[{"type":"function","Type":"x","function":{"name":"f"}}]`),
			oai.CodeUnsupportedParameter, "tools[0].Type"},
		{"tool hull: repeated function",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"tools":[{"type":"function","function":{"name":"f"},"function":{"name":"g"}}]`),
			oai.CodeInvalidJSON, "tools[0].function"},
		{"tool_choice: Function next to function",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, `"tools":[{"type":"function","function":{"name":"f"}}]`,
				`"tool_choice":{"type":"function","function":{"name":"f"},"Function":{"name":"g"}}`),
			oai.CodeUnsupportedParameter, "tool_choice.Function"},
		{"logit_bias: one token id 65 times (cap 64 counted once)",
			dupMsgs("chat", `{"role":"user","content":"hi"}`, logitDup), oai.CodeInvalidJSON, "logit_bias.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			a := rig.post(t, tc.body)
			if a.status != http.StatusBadRequest {
				t.Fatalf("status %d %s, want 400 (upstream hits %d)", a.status, a.body, u.hits.Load())
			}
			code, param, _ := a.envelope(t)
			if code != tc.code || param != tc.param {
				t.Errorf("code %q param %q, want %q %q", code, param, tc.code, tc.param)
			}
			if n := u.hits.Load(); n != 0 {
				t.Errorf("wire hits = %d, want 0", n)
			}
		})
	}
}

// TestFreePayloadKeepsItsKeys (H-2 positive control): a repeated key inside
// a free payload — the schema of response_format.json_schema, a tool's
// parameters — and an unknown key on a hull level are no checked keys: 200,
// one wire hit, the payload upstream as sent.
func TestFreePayloadKeepsItsKeys(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"json schema contents", `"response_format":{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object","type":"object","Type":1}}}`,
			`"schema":{"type":"object","type":"object","Type":1}`},
		{"tool parameters", `"tools":[{"type":"function","function":{"name":"f","parameters":{"q":1,"q":2,"Q":3}}}]`,
			`"parameters":{"q":1,"q":2,"Q":3}`},
		{"unknown hull key", `"response_format":{"type":"json_object","vendor_x":1}`, `"vendor_x":1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			a := rig.post(t, dupMsgs("chat", `{"role":"user","content":"hi"}`, tc.extra))
			if a.status != http.StatusOK || u.hits.Load() != 1 {
				t.Fatalf("status %d %s, hits %d, want 200 and 1 hit", a.status, a.body, u.hits.Load())
			}
			u.mu.Lock()
			raw := string(u.raws[0])
			u.mu.Unlock()
			if !strings.Contains(raw, tc.want) {
				t.Errorf("upstream body %s does not carry %s as sent", raw, tc.want)
			}
		})
	}
}
