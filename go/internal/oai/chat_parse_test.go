package oai_test

// The parse gates of POST /v1/chat/completions (wave W02-07; design/02 §4.5,
// §4.5.1, §4.5.2, §3.2, §5 B1/B10/B18):
//
//   - G2 (L7): the bodies ctx-B's own openai wire (llm.chatOpenAI) sends pass
//     — reasoning, top_k, min_p, logprobs, top_logprobs, response_format,
//     temperature 0 — and llm.Decide against the gateway reads logprobs;
//   - G3 (B1): DROP keys never reach the wire and are named in
//     X-Ctx-Ignored-Params; unknown keys are refused on the top level, the
//     message level and the content-part level; developer becomes system;
//   - G4 (B10): the max_tokens ceilings (gateway ceiling, alias default,
//     tenant ceiling on unset keys) and every policy cap, each with its
//     boundary case served;
//   - G10 (B18): image parts are data URIs of four image types only, valid
//     base64 within vision_max_bytes, over every role — refused before any
//     upstream contact;
//   - G18 (E4): stream true and n ≠ 1 are refused, n = 1 is served without n.
//
// Every refusal is checked on its envelope (code, param, type) and on the
// wire: 0 upstream hits.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
)

// --- helpers of the parse gates ---.

// pgRig is a chat rig over one SGLang-shaped upstream.
func pgRig(t *testing.T, answer string, opts ...rigOption) (*chatRig, *upstream) {
	t.Helper()
	u := newUpstream(t, answer)
	return newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, opts...), u
}

// pgRefused asserts a 400 of the parse: code, param, the invalid-request
// type, and no upstream contact since before.
func pgRefused(t *testing.T, a chatAnswer, u *upstream, before int64, code, param string) {
	t.Helper()
	if a.status != http.StatusBadRequest {
		t.Errorf("status %d, want 400 (body %s)", a.status, a.body)
		return
	}
	gotCode, gotParam, gotType := a.envelope(t)
	if gotCode != code || gotParam != param || gotType != oai.TypeInvalidRequest {
		t.Errorf("envelope code=%q param=%q type=%q, want code=%q param=%q type=%q", gotCode, gotParam, gotType, code, param, oai.TypeInvalidRequest)
	}
	if hits := u.hits.Load() - before; hits != 0 {
		t.Errorf("%d upstream hits for a refused request, want 0", hits)
	}
}

// pgServed asserts a 200 with exactly one upstream hit since before and
// returns the body the upstream saw.
func pgServed(t *testing.T, a chatAnswer, u *upstream, before int64) map[string]any {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", a.status, a.body)
	}
	if hits := u.hits.Load() - before; hits != 1 {
		t.Fatalf("%d upstream hits, want 1", hits)
	}
	return u.lastBody(t)
}

// pgIgnored is the set X-Ctx-Ignored-Params names, sorted.
func pgIgnored(a chatAnswer) []string {
	v := a.header.Get("X-Ctx-Ignored-Params")
	if v == "" {
		return nil
	}
	out := strings.Split(v, ", ")
	sort.Strings(out)
	return out
}

// pgMessages is the upstream messages array.
func pgMessages(t *testing.T, up map[string]any) []any {
	t.Helper()
	msgs, ok := up["messages"].([]any)
	if !ok {
		t.Fatalf("upstream messages %T, want an array", up["messages"])
	}
	return msgs
}

// pgDecode decodes a JSON literal of the test into a generic value.
func pgDecode(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("test literal %s: %v", raw, err)
	}
	return v
}

// pgPadded is prefix + padding + suffix of exactly n bytes (a JSON value at
// a byte cap).
func pgPadded(t *testing.T, prefix, suffix string, n int) string {
	t.Helper()
	pad := n - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("cannot pad to %d bytes", n)
	}
	return prefix + strings.Repeat("x", pad) + suffix
}

// pgTools is a tools array of n minimal function tools.
func pgTools(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"type":"function","function":{"name":"f%d"}}`, i)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// pgEntries is a logit_bias object of n entries.
func pgEntries(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"%d":-1`, 1000+i)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// pgStops is a stop array of n sequences.
func pgStops(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"<s%d>"`, i)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// pgAlias is a _global chat alias with its own policy for one test.
func pgAlias(id, name, policy string) chatAlias {
	return chatAlias{id, backends.GlobalScope, name, "chat", "chat", backends.SensCredentials, policy, true}
}

const (
	idPGCapped  = "00000000-0000-4000-8000-0000000c4b01"
	idPGDefault = "00000000-0000-4000-8000-0000000c4b02"
	idPGCap     = "00000000-0000-4000-8000-0000000c4b03"
	idPGNoVis   = "00000000-0000-4000-8000-0000000c4b04"
)

// --- G18 and the alias-independent refusals ---.

// TestChatStreamAndNRefused (G18, E4): n > 1 is RC-F's — 400
// unsupported_value at its param before any upstream contact; a stream
// flag that is no boolean and stream_options that are no object (or carry
// a non-boolean include_usage) are 400 invalid_type; n = 1 and stream false
// are served, and no n goes upstream. stream true is served since W02-08
// (chat_stream_test.go).
func TestChatStreamAndNRefused(t *testing.T) {
	rig, u := pgRig(t, answerSGLang)
	for _, tc := range []struct {
		name, extra, code, param string
	}{
		{"stream not a boolean", `"stream":"yes"`, oai.CodeInvalidType, "stream"},
		{"stream_options not an object", `"stream":true,"stream_options":true`, oai.CodeInvalidType, "stream_options"},
		{"include_usage not a boolean", `"stream":true,"stream_options":{"include_usage":"yes"}`, oai.CodeInvalidType, "stream_options.include_usage"},
		{"n 2", `"n":2`, oai.CodeUnsupportedValue, "n"},
		{"n 128", `"n":128`, oai.CodeUnsupportedValue, "n"},
		{"n 0", `"n":0`, oai.CodeUnsupportedValue, "n"},
		{"n not an integer", `"n":1.5`, oai.CodeInvalidType, "n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := u.hits.Load()
			pgRefused(t, rig.post(t, chatBody("chat", tc.extra)), u, before, tc.code, tc.param)
		})
	}
	t.Run("n 1 served without n", func(t *testing.T) {
		before := u.hits.Load()
		up := pgServed(t, rig.post(t, chatBody("chat", `"n":1`)), u, before)
		if _, ok := up["n"]; ok {
			t.Errorf("upstream body carries n = %v, want no n (E4)", up["n"])
		}
	})
	t.Run("stream false served", func(t *testing.T) {
		before := u.hits.Load()
		up := pgServed(t, rig.post(t, chatBody("chat", `"stream":false`)), u, before)
		if up["stream"] != false {
			t.Errorf("upstream stream = %v, want false", up["stream"])
		}
	})
}

// TestChatBodyNotAnObject: a body that is no JSON object is 400
// invalid_json without a param.
func TestChatBodyNotAnObject(t *testing.T) {
	rig, u := pgRig(t, answerSGLang)
	for name, body := range map[string]string{
		"empty":         ``,
		"garbage":       `not json`,
		"array":         `[{"model":"chat"}]`,
		"string":        `"chat"`,
		"null":          `null`,
		"number":        `42`,
		"truncated":     `{"model":"chat","messages":[`,
		"trailing data": chatBody("chat") + `{"x":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			before := u.hits.Load()
			pgRefused(t, rig.post(t, body), u, before, oai.CodeInvalidJSON, "")
		})
	}
}

// TestChatRequiredParameters: model and messages are required.
func TestChatRequiredParameters(t *testing.T) {
	rig, u := pgRig(t, answerSGLang)
	for _, tc := range []struct {
		name, body, code, param string
	}{
		{"model missing", `{"messages":[{"role":"user","content":"hi"}]}`, oai.CodeMissingRequiredParameter, "model"},
		{"model empty", `{"model":"","messages":[{"role":"user","content":"hi"}]}`, oai.CodeMissingRequiredParameter, "model"},
		{"model null", `{"model":null,"messages":[{"role":"user","content":"hi"}]}`, oai.CodeMissingRequiredParameter, "model"},
		{"model not a string", `{"model":7,"messages":[{"role":"user","content":"hi"}]}`, oai.CodeInvalidType, "model"},
		{"messages missing", `{"model":"chat"}`, oai.CodeMissingRequiredParameter, "messages"},
		{"messages empty", `{"model":"chat","messages":[]}`, oai.CodeUnsupportedValue, "messages"},
		{"messages not an array", `{"model":"chat","messages":"hi"}`, oai.CodeInvalidType, "messages"},
		{"role missing", `{"model":"chat","messages":[{"content":"hi"}]}`, oai.CodeMissingRequiredParameter, "messages[0].role"},
		{"user content missing", `{"model":"chat","messages":[{"role":"user"}]}`, oai.CodeMissingRequiredParameter, "messages[0].content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := u.hits.Load()
			pgRefused(t, rig.post(t, tc.body), u, before, tc.code, tc.param)
		})
	}
}

// --- G3 B1: the allowlist on every level ---.

// pgDropValues is the DROP class of design/02 §4.5 (top level) with a value
// a client or vendor SDK would send; stream_options.include_obfuscation is
// a subkey of a CTX key and not listed here.
var pgDropValues = map[string]string{
	"priority":               `10`,
	"chat_template_kwargs":   `{"enable_thinking":true}`,
	"user":                   `"user-1"`,
	"safety_identifier":      `"sid-1"`,
	"prompt_cache_key":       `"pck-1"`,
	"prompt_cache_retention": `"24h"`,
	"prompt_cache_options":   `{"mode":"x"}`,
	"metadata":               `{"k":"v"}`,
	"store":                  `true`,
	"verbosity":              `"low"`,
	"moderation":             `{"x":1}`,
	"rid":                    `"rid-1"`,
	"request_id":             `"req-1"`,
	"id_slot":                `0`,
	"cache_prompt":           `true`,
	"lora":                   `"adapter-1"`,
	"lora_path":              `"/models/lora-1"`,
	"return_hidden_states":   `true`,
	"custom_logit_processor": `"cHJvY2Vzc29y"`,
	"bootstrap_host":         `"10.0.0.1"`,
	"bootstrap_port":         `8998`,
	"bootstrap_room":         `7`,
	"kv_transfer_params":     `{"do_remote_decode":true}`,
	"vllm_xargs":             `{"x":1}`,
	"cache_salt":             `"salt-1"`,
	"session_params":         `{"id":"s-1"}`,
	"modalities":             `["text"]`,
}

// TestUpstreamBodyNeverCarriesDropKeys (G3, B1): the fake upstream records
// the body; no DROP key and no message reasoning trace reaches it, their
// names are in X-Ctx-Ignored-Params; an unknown key on any level is a 400
// at its param before any upstream contact; developer goes upstream as
// system with the rest of the message unchanged.
func TestUpstreamBodyNeverCarriesDropKeys(t *testing.T) {
	t.Run("drop keys never upstream, named in the header", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang)
		keys := make([]string, 0, len(pgDropValues))
		for k := range pgDropValues {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]string, 0, len(keys))
		for _, k := range keys {
			fields = append(fields, fmt.Sprintf("%q:%s", k, pgDropValues[k]))
		}
		body := `{"model":"chat","messages":[{"role":"user","content":"hi","reasoning":"OLD-THOUGHT-CANARY","reasoning_content":"OLD-THOUGHT-CANARY"}],` +
			strings.Join(fields, ",") + `}`
		a := rig.post(t, body)
		up := pgServed(t, a, u, 0)
		for _, k := range keys {
			if v, ok := up[k]; ok {
				t.Errorf("DROP key %q reached the upstream body (= %v)", k, v)
			}
		}
		msgs := pgMessages(t, up)
		if want := pgDecode(t, `{"role":"user","content":"hi"}`); !reflect.DeepEqual(msgs[0], want) {
			t.Errorf("upstream messages[0] = %v, want %v (reasoning traces removed, rest unchanged)", msgs[0], want)
		}
		raw, _ := json.Marshal(up)
		if bytes.Contains(raw, []byte("OLD-THOUGHT-CANARY")) {
			t.Errorf("a message reasoning trace reached the upstream body: %s", raw)
		}
		want := append(slices.Clone(keys), "messages[].reasoning", "messages[].reasoning_content")
		sort.Strings(want)
		if got := pgIgnored(a); !slices.Equal(got, want) {
			t.Errorf("X-Ctx-Ignored-Params = %q, want %q", got, want)
		}
	})

	t.Run("unknown top-level key refused", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang)
		for k, v := range map[string]string{
			"functions":          `[{"name":"f","parameters":{}}]`,
			"function_call":      `"auto"`,
			"audio":              `{"voice":"alloy","format":"wav"}`,
			"web_search_options": `{}`,
			"prediction":         `{"type":"content","content":"x"}`,
			"foo":                `1`,
			"Priority":           `10`,
			"extra_body":         `{"rid":"x"}`,
		} {
			t.Run(k, func(t *testing.T) {
				before := u.hits.Load()
				pgRefused(t, rig.post(t, chatBody("chat", fmt.Sprintf("%q:%s", k, v))), u, before, oai.CodeUnsupportedParameter, k)
			})
		}
		t.Run("modalities beyond text", func(t *testing.T) {
			before := u.hits.Load()
			pgRefused(t, rig.post(t, chatBody("chat", `"modalities":["text","audio"]`)), u, before, oai.CodeUnsupportedParameter, "modalities")
		})
	})

	t.Run("unknown message key refused", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang)
		for _, tc := range []struct {
			name, messages, code, param string
		}{
			{"rid in a message", `[{"role":"user","content":"hi","rid":"rid-1"}]`, oai.CodeUnsupportedParameter, "messages[0].rid"},
			{"unknown key in the second message", `[{"role":"user","content":"hi"},{"role":"assistant","content":"x","foo":1}]`, oai.CodeUnsupportedParameter, "messages[1].foo"},
			{"function_call in a message", `[{"role":"assistant","content":"x","function_call":{"name":"f"}}]`, oai.CodeUnsupportedParameter, "messages[0].function_call"},
			{"unknown role", `[{"role":"function","content":"x"}]`, oai.CodeUnsupportedValue, "messages[0].role"},
			{"tool_calls on a user message", `[{"role":"user","content":"hi","tool_calls":[]}]`, oai.CodeUnsupportedParameter, "messages[0].tool_calls"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				before := u.hits.Load()
				pgRefused(t, rig.post(t, `{"model":"chat","messages":`+tc.messages+`}`), u, before, tc.code, tc.param)
			})
		}
	})

	t.Run("unknown content part key refused", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang)
		img := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png-bytes"))
		for _, tc := range []struct {
			name, model, part, code, param string
		}{
			{"unknown key on a text part", "chat", `{"type":"text","text":"hi","foo":1}`, oai.CodeUnsupportedParameter, "messages[0].content[0].foo"},
			{"cache_control on a text part", "chat", `{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}`, oai.CodeUnsupportedParameter, "messages[0].content[0].cache_control"},
			{"unknown key on an image part", "vis", fmt.Sprintf(`{"type":"image_url","image_url":{"url":%q},"foo":1}`, img), oai.CodeUnsupportedParameter, "messages[0].content[0].foo"},
			{"unknown key inside image_url", "vis", fmt.Sprintf(`{"type":"image_url","image_url":{"url":%q,"foo":1}}`, img), oai.CodeUnsupportedParameter, "messages[0].content[0].image_url.foo"},
			{"input_audio part", "chat", `{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}`, oai.CodeUnsupportedValue, "messages[0].content[0].type"},
			{"file part", "chat", `{"type":"file","file":{"file_id":"f"}}`, oai.CodeUnsupportedValue, "messages[0].content[0].type"},
			{"video_url part", "vis", `{"type":"video_url","video_url":{"url":"http://127.0.0.1/v.mp4"}}`, oai.CodeUnsupportedValue, "messages[0].content[0].type"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				before := u.hits.Load()
				body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[%s]}]}`, tc.model, tc.part)
				pgRefused(t, rig.post(t, body), u, before, tc.code, tc.param)
			})
		}
	})

	t.Run("developer goes upstream as system", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang)
		dev := `{"role":"developer","content":"be brief","name":"dev-1"}`
		user := `{"role":"user","content":[{"type":"text","text":"hi"}],"name":"u-1"}`
		a := rig.post(t, `{"model":"chat","messages":[`+dev+`,`+user+`]}`)
		msgs := pgMessages(t, pgServed(t, a, u, 0))
		if len(msgs) != 2 {
			t.Fatalf("upstream got %d messages, want 2", len(msgs))
		}
		if want := pgDecode(t, `{"role":"system","content":"be brief","name":"dev-1"}`); !reflect.DeepEqual(msgs[0], want) {
			t.Errorf("upstream messages[0] = %v, want %v", msgs[0], want)
		}
		if want := pgDecode(t, user); !reflect.DeepEqual(msgs[1], want) {
			t.Errorf("upstream messages[1] = %v, want %v (unchanged)", msgs[1], want)
		}
		if got := pgIgnored(a); got != nil {
			t.Errorf("X-Ctx-Ignored-Params = %q for a request without DROP keys, want none", got)
		}
	})
}

// --- G4 B10: max_tokens ceilings and the policy caps ---.

// pgMaxTokens is the upstream max_tokens of one served request.
func pgMaxTokens(t *testing.T, rig *chatRig, u *upstream, body string) any {
	t.Helper()
	before := u.hits.Load()
	up := pgServed(t, rig.post(t, body), u, before)
	if _, ok := up["max_completion_tokens"]; ok {
		t.Errorf("upstream carries max_completion_tokens = %v; ctx sends max_tokens only", up["max_completion_tokens"])
	}
	return up["max_tokens"]
}

// TestClientMaxTokensClampedByCeiling (B10): client max_tokens 262144, no
// alias cap, no row cap (the row has no limits = 0 = unbounded) ⇒ the
// upstream max_tokens is gateway.max_output_tokens_ceiling (32768); the
// alias cap lowers it, the setting is read live, a smaller client value
// passes unchanged.
func TestClientMaxTokensClampedByCeiling(t *testing.T) {
	rig, u := pgRig(t, answerSGLang)
	rig.reg.reload(t, append(chatAliases(), pgAlias(idPGCap, "capped500", `{"max_output_tokens":500}`)))
	for _, tc := range []struct {
		name, model, extra string
		want               float64
	}{
		{"max_tokens above the ceiling", "chat", `"max_tokens":262144`, 32768},
		{"max_completion_tokens above the ceiling", "chat", `"max_completion_tokens":262144`, 32768},
		{"below the ceiling unchanged", "chat", `"max_tokens":100`, 100},
		{"exactly the ceiling", "chat", `"max_tokens":32768`, 32768},
		{"alias cap below the ceiling", "capped500", `"max_tokens":262144`, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgMaxTokens(t, rig, u, chatBody(tc.model, tc.extra)); got != tc.want {
				t.Errorf("upstream max_tokens = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("ceiling setting read live", func(t *testing.T) {
		rig.setSettings(func(s *oai.Settings) { s.MaxOutputTokensCeiling = 1000 })
		t.Cleanup(func() {
			rig.setSettings(func(s *oai.Settings) { s.MaxOutputTokensCeiling = chatSettings().MaxOutputTokensCeiling })
		})
		if got := pgMaxTokens(t, rig, u, chatBody("chat", `"max_tokens":262144`)); got != float64(1000) {
			t.Errorf("upstream max_tokens = %v, want the live ceiling 1000", got)
		}
	})
}

// TestDefaultMaxTokensInjected (B10): without a client value the upstream
// max_tokens is the alias's default_max_tokens, else
// gateway.default_max_tokens (4096) — read live.
func TestDefaultMaxTokensInjected(t *testing.T) {
	rig, u := pgRig(t, answerSGLang)
	rig.reg.reload(t, append(chatAliases(), pgAlias(idPGDefault, "dflt", `{"default_max_tokens":777}`)))
	if got := pgMaxTokens(t, rig, u, chatBody("chat")); got != float64(4096) {
		t.Errorf("no client value, no alias default: upstream max_tokens = %v, want gateway.default_max_tokens 4096", got)
	}
	if got := pgMaxTokens(t, rig, u, chatBody("dflt")); got != float64(777) {
		t.Errorf("alias default_max_tokens 777: upstream max_tokens = %v, want 777", got)
	}
	if got := pgMaxTokens(t, rig, u, chatBody("dflt", `"max_tokens":50`)); got != float64(50) {
		t.Errorf("client 50 over alias default 777: upstream max_tokens = %v, want 50", got)
	}
	rig.setSettings(func(s *oai.Settings) { s.DefaultMaxTokens = 1500 })
	if got := pgMaxTokens(t, rig, u, chatBody("chat")); got != float64(1500) {
		t.Errorf("gateway.default_max_tokens 1500 (live): upstream max_tokens = %v, want 1500", got)
	}
}

// pgTenantRig is a rig whose _global rows a tenant alias may reach (the
// export filter is G6's; here the role is exported so the tenant alias is
// served).
func pgTenantRig(t *testing.T) (*chatRig, *upstream) {
	t.Helper()
	rig, u := pgRig(t, answerSGLang)
	rig.setSettings(func(s *oai.Settings) { s.ExportableRoles = []string{"chat"} })
	return rig, u
}

// TestTenantAliasCeilingOnUnsetMaxTokens (B10, W02-04 handover (1)): the
// tenant alias tchat sets no max_output_tokens ⇒ the tenant ceiling 8192
// clamps both the client value and the default; the _global alias under the
// same request gets the gateway ceiling.
func TestTenantAliasCeilingOnUnsetMaxTokens(t *testing.T) {
	rig, u := pgTenantRig(t)
	if got := pgMaxTokens(t, rig, u, chatBody("tchat", `"max_tokens":262144`)); got != float64(8192) {
		t.Errorf("tenant alias, client 262144: upstream max_tokens = %v, want tenant_alias_ceiling.max_output_tokens 8192", got)
	}
	if got := pgMaxTokens(t, rig, u, chatBody("chat", `"max_tokens":262144`)); got != float64(32768) {
		t.Errorf("_global alias, client 262144: upstream max_tokens = %v, want the gateway ceiling 32768 (control)", got)
	}
	rig.setSettings(func(s *oai.Settings) { s.DefaultMaxTokens = 20000 })
	if got := pgMaxTokens(t, rig, u, chatBody("tchat")); got != float64(8192) {
		t.Errorf("tenant alias, default 20000: upstream max_tokens = %v, want the tenant ceiling 8192", got)
	}
	rig.setSettings(func(s *oai.Settings) { s.TenantAliasCeiling.MaxOutputTokens = 2048 })
	if got := pgMaxTokens(t, rig, u, chatBody("tchat", `"max_tokens":262144`)); got != float64(2048) {
		t.Errorf("tenant ceiling 2048 (live): upstream max_tokens = %v, want 2048", got)
	}
}

// TestTenantAliasCeilingOnUnsetToolsCount (B10): tchat sets no
// tools.max_count, the tenant ceiling is 2 ⇒ 3 tools are a 400 at tools, 2
// are served; the _global alias takes 3 (its default is 64).
func TestTenantAliasCeilingOnUnsetToolsCount(t *testing.T) {
	rig, u := pgTenantRig(t)
	rig.setSettings(func(s *oai.Settings) { s.TenantAliasCeiling.ToolsMaxCount = 2 })
	before := u.hits.Load()
	pgRefused(t, rig.post(t, chatBody("tchat", `"tools":`+pgTools(3))), u, before, oai.CodeUnsupportedValue, "tools")
	before = u.hits.Load()
	up := pgServed(t, rig.post(t, chatBody("tchat", `"tools":`+pgTools(2))), u, before)
	if tools, _ := up["tools"].([]any); len(tools) != 2 {
		t.Errorf("upstream tools = %v, want the 2 sent", up["tools"])
	}
	before = u.hits.Load()
	pgServed(t, rig.post(t, chatBody("chat", `"tools":`+pgTools(3))), u, before)
}

// TestMaxCompletionTokensMapping (§4.5.2 stage 3): max_completion_tokens is
// max_tokens; both set and different is a 400; non-positive is a 400.
func TestMaxCompletionTokensMapping(t *testing.T) {
	rig, u := pgRig(t, answerSGLang)
	if got := pgMaxTokens(t, rig, u, chatBody("chat", `"max_completion_tokens":300`)); got != float64(300) {
		t.Errorf("max_completion_tokens 300: upstream max_tokens = %v, want 300", got)
	}
	if got := pgMaxTokens(t, rig, u, chatBody("chat", `"max_completion_tokens":300`, `"max_tokens":300`)); got != float64(300) {
		t.Errorf("both 300: upstream max_tokens = %v, want 300", got)
	}
	for _, tc := range []struct {
		name, code, param string
		extra             []string
	}{
		{"both set, different", oai.CodeUnsupportedValue, "max_tokens", []string{`"max_completion_tokens":300`, `"max_tokens":301`}},
		{"max_tokens 0", oai.CodeUnsupportedValue, "max_tokens", []string{`"max_tokens":0`}},
		{"max_completion_tokens negative", oai.CodeUnsupportedValue, "max_completion_tokens", []string{`"max_completion_tokens":-5`}},
		{"max_tokens not an integer", oai.CodeInvalidType, "max_tokens", []string{`"max_tokens":12.5`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := u.hits.Load()
			pgRefused(t, rig.post(t, chatBody("chat", tc.extra...)), u, before, tc.code, tc.param)
		})
	}
}

// pgCapCase is one cap with the request at the cap (served) and one past it
// (refused at param).
type pgCapCase struct {
	name, model, atCap, pastCap, param string
}

// pgCapCases are the §3.2 caps: set by the alias policy (alias capped) and
// the design defaults of an unset key (alias chat, policy without caps).
func pgCapCases(t *testing.T) []pgCapCase {
	t.Helper()
	toolsPrefix, toolsSuffix := `[{"type":"function","function":{"name":"f","description":"`, `"}}]`
	rfPrefix, rfSuffix := `{"type":"json_schema","json_schema":{"name":"s","schema":{"type":"object","description":"`, `"}}}`
	return []pgCapCase{
		{"tools.max_count policy 2", "capped", pgTools(2), pgTools(3), "tools"},
		{"tools.max_count default 64", "chat", pgTools(64), pgTools(65), "tools"},
		{"tools.max_bytes policy 512", "capped", pgPadded(t, toolsPrefix, toolsSuffix, 512), pgPadded(t, toolsPrefix, toolsSuffix, 513), "tools"},
		{"tools.max_bytes default 256 KiB", "chat", pgPadded(t, toolsPrefix, toolsSuffix, 256<<10), pgPadded(t, toolsPrefix, toolsSuffix, 256<<10+1), "tools"},
		{"response_format.max_bytes policy 256", "capped", pgPadded(t, rfPrefix, rfSuffix, 256), pgPadded(t, rfPrefix, rfSuffix, 257), "response_format"},
		{"response_format.max_bytes default 64 KiB", "chat", pgPadded(t, rfPrefix, rfSuffix, 64<<10), pgPadded(t, rfPrefix, rfSuffix, 64<<10+1), "response_format"},
		{"logit_bias.max_entries policy 3", "capped", pgEntries(3), pgEntries(4), "logit_bias"},
		{"logit_bias.max_entries default 64", "chat", pgEntries(64), pgEntries(65), "logit_bias"},
		{"stop.max_entries policy 2", "capped", pgStops(2), pgStops(3), "stop"},
		{"stop.max_entries default 4", "chat", pgStops(4), pgStops(5), "stop"},
		{"top_logprobs spec bound 20", "chat", `20`, `21`, "top_logprobs"},
	}
}

// TestChatPolicyCaps (B10, design/02 §3.2): every size cap refuses one past
// the cap with 400 unsupported_value at its param before any upstream
// contact, and serves the request exactly at the cap with the value
// verbatim upstream.
func TestChatPolicyCaps(t *testing.T) {
	rig, u := pgRig(t, answerSGLang)
	capped := `{"tools.max_count":2,"tools.max_bytes":512,"response_format.max_bytes":256,"logit_bias.max_entries":3,"stop.max_entries":2}`
	rig.reg.reload(t, append(chatAliases(), pgAlias(idPGCapped, "capped", capped)))
	for _, tc := range pgCapCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			before := u.hits.Load()
			pgRefused(t, rig.post(t, chatBody(tc.model, fmt.Sprintf("%q:%s", tc.param, tc.pastCap))), u, before, oai.CodeUnsupportedValue, tc.param)
			before = u.hits.Load()
			up := pgServed(t, rig.post(t, chatBody(tc.model, fmt.Sprintf("%q:%s", tc.param, tc.atCap))), u, before)
			if want := pgDecode(t, tc.atCap); !reflect.DeepEqual(up[tc.param], want) {
				t.Errorf("upstream %s differs from the value sent at the cap", tc.param)
			}
		})
	}
	for _, tc := range []struct {
		name, extra, code, param string
	}{
		{"response_format unknown type", `"response_format":{"type":"grammar","grammar":"root ::= x"}`, oai.CodeUnsupportedValue, "response_format.type"},
		{"response_format without type", `"response_format":{"json_schema":{}}`, oai.CodeUnsupportedValue, "response_format.type"},
		{"top_logprobs negative", `"top_logprobs":-1`, oai.CodeUnsupportedValue, "top_logprobs"},
		{"tools not an array", `"tools":{"type":"function"}`, oai.CodeInvalidType, "tools"},
		{"stop array of non-strings", `"stop":[1,2]`, oai.CodeInvalidType, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := u.hits.Load()
			pgRefused(t, rig.post(t, chatBody("chat", tc.extra)), u, before, tc.code, tc.param)
		})
	}
	t.Run("stop as a string served", func(t *testing.T) {
		before := u.hits.Load()
		if up := pgServed(t, rig.post(t, chatBody("capped", `"stop":"<end>"`)), u, before); up["stop"] != "<end>" {
			t.Errorf("upstream stop = %v, want the string sent", up["stop"])
		}
	})
}

// --- G10 B18: image parts are data URIs only ---.

// pgDataURI is a data URI of mime over n payload bytes.
func pgDataURI(mime string, n int) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x89}, n))
}

// pgVisionMax is vision_max_bytes of the harness alias vis.
const pgVisionMax = 1024

// pgImageRefused are the image_url.url forms an engine would fetch or read
// server-side, or that break the data-URI rule (design/02 §4.5.1, B18).
func pgImageRefused() map[string]string {
	valid := base64.StdEncoding.EncodeToString([]byte("png-bytes!!!"))
	return map[string]string{
		"http loopback":             "http://127.0.0.1/x.png",
		"https loopback":            "https://127.0.0.1/x.png",
		"uppercase scheme":          "HTTP://example/x.png",
		"protocol-relative":         "//127.0.0.1/x.png",
		"file url":                  "file:///etc/x.png",
		"bare path":                 "/models/x.png",
		"relative path":             "x.png",
		"empty url":                 "",
		"svg data uri":              "data:image/svg+xml;base64," + valid,
		"data uri with a parameter": "data:image/png;charset=x;base64," + valid,
		"data uri without base64":   "data:image/png,rawbytes",
		"uppercase data uri":        "DATA:IMAGE/PNG;BASE64," + valid,
		"uppercase mime":            "data:image/PNG;base64," + valid,
		"text mime":                 "data:text/plain;base64," + valid,
		"empty payload":             "data:image/png;base64,",
		"invalid base64":            "data:image/png;base64,!!!!notbase64",
		"unpadded base64":           "data:image/png;base64,iVBORw0KGgo",
		"base64url alphabet":        "data:image/png;base64,ab-_",
		"base64 with a newline":     "data:image/png;base64,iVBO\nRw0K",
		"decoded above the cap":     pgDataURI("image/png", pgVisionMax+1),
		"data uri behind a space":   " " + pgDataURI("image/png", 8),
	}
}

// pgImagePlacement puts one image part into a request at a role; the param
// is where the url sits.
type pgImagePlacement struct {
	role  string
	build func(part string) string
	param string
}

func pgImagePlacements() []pgImagePlacement {
	text := `{"type":"text","text":"look"}`
	return []pgImagePlacement{
		{"user", func(p string) string { return `[{"role":"user","content":[` + p + `]}]` }, "messages[0].content[0].image_url.url"},
		{"system", func(p string) string {
			return `[{"role":"system","content":[` + p + `]},{"role":"user","content":"hi"}]`
		}, "messages[0].content[0].image_url.url"},
		{"developer", func(p string) string {
			return `[{"role":"developer","content":[` + text + `,` + p + `]},{"role":"user","content":"hi"}]`
		}, "messages[0].content[1].image_url.url"},
		{"assistant", func(p string) string {
			return `[{"role":"user","content":"hi"},{"role":"assistant","content":[` + text + `,` + p + `]},{"role":"user","content":"again"}]`
		}, "messages[1].content[1].image_url.url"},
		{"tool", func(p string) string {
			return `[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"c1","content":[` + p + `]}]`
		}, "messages[2].content[0].image_url.url"},
	}
}

func pgImagePart(url string) string {
	raw, _ := json.Marshal(map[string]any{"type": "image_url", "image_url": map[string]any{"url": url, "detail": "low"}})
	return string(raw)
}

// TestImageURLNeverForwardedRemote (G10, B18): alias vis (vision true,
// vision_max_bytes 1024): every remote, local-path or malformed image url
// is a 400 unsupported_value at its url — in every role, 0 upstream hits; a
// valid data URI of each of the four types at the size cap is served and
// reaches the upstream unchanged; an alias without vision refuses even a
// valid data URI.
func TestImageURLNeverForwardedRemote(t *testing.T) {
	rig, u := pgRig(t, answerSGLang)
	rig.reg.reload(t, append(chatAliases(), pgAlias(idPGNoVis, "novis", `{"vision":false,"vision_max_bytes":1024}`)))
	for _, pl := range pgImagePlacements() {
		for name, url := range pgImageRefused() {
			t.Run(pl.role+"/"+name, func(t *testing.T) {
				before := u.hits.Load()
				body := `{"model":"vis","messages":` + pl.build(pgImagePart(url)) + `}`
				pgRefused(t, rig.post(t, body), u, before, oai.CodeUnsupportedValue, pl.param)
			})
		}
	}
	for _, pl := range pgImagePlacements() {
		for _, mime := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
			t.Run(pl.role+"/valid "+mime+" at the cap", func(t *testing.T) {
				before := u.hits.Load()
				messages := pl.build(pgImagePart(pgDataURI(mime, pgVisionMax)))
				up := pgServed(t, rig.post(t, `{"model":"vis","messages":`+messages+`}`), u, before)
				// developer goes upstream as system (G3); everything else as sent
				want := pgDecode(t, strings.Replace(messages, `"role":"developer"`, `"role":"system"`, 1))
				if got := up["messages"]; !reflect.DeepEqual(got, want) {
					t.Errorf("upstream messages differ from the ones sent: the image part must reach the upstream unchanged")
				}
			})
		}
	}
	for _, model := range []string{"chat", "novis"} {
		t.Run("valid data uri on "+model+" without vision", func(t *testing.T) {
			before := u.hits.Load()
			body := `{"model":"` + model + `","messages":[{"role":"user","content":[` + pgImagePart(pgDataURI("image/png", 8)) + `]}]}`
			pgRefused(t, rig.post(t, body), u, before, oai.CodeUnsupportedValue, "messages[0].content[0].image_url.url")
		})
	}
	t.Run("url not a string", func(t *testing.T) {
		before := u.hits.Load()
		body := `{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":7}}]}]}`
		pgRefused(t, rig.post(t, body), u, before, oai.CodeInvalidType, "messages[0].content[0].image_url.url")
	})
}

// --- G2 L7: ctx-B's own bodies ---.

// pgCapture records the raw request body and the answer's ignored-params
// header in front of the gateway.
type pgCapture struct {
	next    http.Handler
	mu      sync.Mutex
	bodies  [][]byte
	ignored []string
}

func (c *pgCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	c.next.ServeHTTP(w, r)
	c.mu.Lock()
	c.bodies = append(c.bodies, raw)
	c.ignored = append(c.ignored, w.Header().Get("X-Ctx-Ignored-Params"))
	c.mu.Unlock()
}

// last is the last body ctx-B sent, decoded, and its ignored header.
func (c *pgCapture) last(t *testing.T) (raw []byte, body map[string]any, ignored string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("ctx-B sent nothing to the gateway")
	}
	raw = c.bodies[len(c.bodies)-1]
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("ctx-B body: %v", err)
	}
	return raw, body, c.ignored[len(c.ignored)-1]
}

// answerDecide is an SGLang-shaped answer of one token with the first
// token's alternatives (OpenAI logprobs shape).
const answerDecide = `{"id":"ID-CANARY","object":"chat.completion","created":1700000000,"model":"MODEL-CANARY",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"A"},` +
	`"logprobs":{"content":[{"token":"A","logprob":-0.105,"bytes":[65],"top_logprobs":[{"token":"A","logprob":-0.105,"bytes":[65]},{"token":" B","logprob":-2.3,"bytes":[32,66]}]}]},` +
	`"finish_reason":"length"}],"usage":{"prompt_tokens":20,"completion_tokens":1,"total_tokens":21}}`

// ctxBIdentity is a second instance (ctx-B) in front of the gateway: its
// loop marker reaches ctx-A the way a peer's does.
var ctxBIdentity = fedhop.NewIdentity("fedcba9876543210fedcba9876543210", []byte("w02-07-pa-ctx-b-secret-012345678"))

// pgCtxB is ctx-B's backend row pointing at the gateway: an openai row
// whose model is the alias name, think off (ctx-B sends reasoning).
func pgCtxB(host string) backends.Backend {
	return backends.Backend{
		ID: "id-ctx-a", Name: "ctx-a", Host: host, Protocol: backends.ProtocolOpenAI, Model: "chat",
		Roles: []string{"chat"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Think: backends.ThinkMode("false"),
	}
}

// pgMissing reports the keys missing from m.
func pgMissing(m map[string]any, keys ...string) []string {
	var out []string
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// TestChatAcceptsCtxBBodies (G2, L7): the bodies ctx-B's openai wire sends
// (llm.Chat / llm.ChatJSON → chatOpenAI, recorded in front of the gateway)
// pass without a 400 — reasoning, top_k, min_p, top_p, presence and
// frequency penalty, logprobs, top_logprobs, response_format json_object
// and json_schema, temperature 0 — and llm.Decide against the gateway reads
// the first token's logprobs (choices are relayed verbatim).
func TestChatAcceptsCtxBBodies(t *testing.T) {
	rig, u := pgRig(t, answerDecide)
	capture := &pgCapture{next: rig.router}
	gw := httptest.NewServer(capture)
	t.Cleanup(gw.Close)
	ctx := fedhop.WithIdentity(context.Background(), ctxBIdentity)
	cb := pgCtxB(gw.URL)

	t.Run("decide reads logprobs through the gateway", func(t *testing.T) {
		before := u.hits.Load()
		resp, err := llm.Chat(ctx, cb, "Answer with one letter.", "A or B?", llm.DecideOptions(), 10*time.Second)
		if err != nil {
			t.Fatalf("ctx-B decide body refused by the gateway: %v", err)
		}
		_, sent, _ := capture.last(t)
		if miss := pgMissing(sent, "logprobs", "top_logprobs", "temperature", "reasoning", "max_tokens"); miss != nil {
			t.Fatalf("ctx-B's decide body lacks %v — the gate no longer probes them (body %v)", miss, sent)
		}
		if sent["temperature"] != float64(0) {
			t.Errorf("ctx-B sent temperature %v, want 0 (greedy)", sent["temperature"])
		}
		d, err := llm.Decide(resp, []string{"A", "B"})
		if err != nil {
			t.Fatalf("decide through the gateway: %v (top logprobs %v)", err, resp.TopLogprobs)
		}
		if d.Best != "A" || d.Mass < 0.9 {
			t.Errorf("decision %+v, want A with mass ≥ 0.9 (exp(-0.105)+exp(-2.3))", d)
		}
		up := pgServed(t, chatAnswer{status: http.StatusOK}, u, before)
		for k, want := range map[string]any{"logprobs": true, "top_logprobs": float64(20), "temperature": float64(0)} {
			if up[k] != want {
				t.Errorf("upstream %s = %v, want %v verbatim", k, up[k], want)
			}
		}
	})

	sampling := llm.Options{Temperature: 0, TopP: 0.9, TopK: 20, MinP: 0.05, PresencePenalty: 0.5, RepeatPenalty: 1.1, NumPredict: 64, TopLogprobs: 5}
	t.Run("sampling extensions and json_object", func(t *testing.T) {
		before := u.hits.Load()
		if _, err := llm.ChatJSON(ctx, cb, "Answer in JSON.", "A or B?", sampling, 10*time.Second); err != nil {
			t.Fatalf("ctx-B json body refused by the gateway: %v", err)
		}
		_, sent, _ := capture.last(t)
		l7 := []string{"top_k", "min_p", "top_p", "presence_penalty", "frequency_penalty", "response_format", "logprobs", "top_logprobs", "reasoning", "temperature"}
		if miss := pgMissing(sent, l7...); miss != nil {
			t.Fatalf("ctx-B's body lacks %v — the gate no longer probes them (body %v)", miss, sent)
		}
		up := pgServed(t, chatAnswer{status: http.StatusOK}, u, before)
		for _, k := range []string{"top_k", "min_p", "top_p", "presence_penalty", "frequency_penalty", "response_format", "logprobs", "top_logprobs", "temperature"} {
			if !reflect.DeepEqual(up[k], sent[k]) {
				t.Errorf("upstream %s = %v, want ctx-B's %v verbatim (PASS)", k, up[k], sent[k])
			}
		}
	})

	t.Run("json_schema on ctx-B's body", func(t *testing.T) {
		raw, sent, _ := capture.last(t)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := sent["response_format"]; !ok {
			t.Fatal("the recorded body has no response_format to replace")
		}
		schema := `{"type":"json_schema","json_schema":{"name":"verdict","strict":true,` +
			`"schema":{"type":"object","properties":{"label":{"type":"string","enum":["A","B"]}},"required":["label"],"additionalProperties":false}}}`
		body["response_format"] = json.RawMessage(schema)
		enc, _ := json.Marshal(body)
		before := u.hits.Load()
		up := pgServed(t, rig.post(t, string(enc)), u, before)
		if !reflect.DeepEqual(up["response_format"], pgDecode(t, schema)) {
			t.Errorf("upstream response_format = %v, want the json_schema sent", up["response_format"])
		}
	})

	t.Run("ctx-B row extra_body and model_map params", func(t *testing.T) {
		row := pgCtxB(gw.URL)
		row.ExtraBody = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
		opts := sampling
		opts.Extra = map[string]any{"repetition_penalty": 1.05} // a model_map param without an Options field
		before := u.hits.Load()
		if _, err := llm.Chat(ctx, row, "s", "u", opts, 10*time.Second); err != nil {
			t.Fatalf("ctx-B body with row extras refused by the gateway: %v", err)
		}
		_, sent, ignored := capture.last(t)
		if miss := pgMissing(sent, "chat_template_kwargs", "repetition_penalty"); miss != nil {
			t.Fatalf("ctx-B's body lacks %v (body %v)", miss, sent)
		}
		if ignored != "chat_template_kwargs" {
			t.Errorf("X-Ctx-Ignored-Params = %q, want chat_template_kwargs (DROP, never 400)", ignored)
		}
		up := pgServed(t, chatAnswer{status: http.StatusOK}, u, before)
		if _, ok := up["chat_template_kwargs"]; ok {
			t.Errorf("ctx-B's chat_template_kwargs reached the upstream (= %v)", up["chat_template_kwargs"])
		}
		if up["repetition_penalty"] != 1.05 {
			t.Errorf("upstream repetition_penalty = %v, want ctx-B's 1.05 verbatim (PASS)", up["repetition_penalty"])
		}
	})
}
