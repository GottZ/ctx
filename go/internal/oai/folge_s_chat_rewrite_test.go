package oai

// The non-stream differential gate of FOLGE-S S2 (RC-D, cluster C8): the
// client body rewriteChat writes for every upstream answer of the corpus is
// byte for byte the base's (rewriteChat at 437a9f49, the map rewrite), the
// error the same where the base refused. The golden file was written from
// the base (CTX_FOLGE_S_CHAT_GOLDEN=update) before the token walk replaced
// it; bodies are stored as base64 (the corpus carries invalid UTF-8, which a
// JSON string of the golden would not keep).

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const folgeSChatGoldenPath = "testdata/folge_s_chat_rewrite_trace.json"

type folgeSChatCase struct {
	name         string
	lane         string
	reasoningOff bool
	upstream     string
}

// folgeSChatGolden is one answer's rewrite as the base wrote it.
type folgeSChatGolden struct {
	Name string `json:"name"`
	Out  []byte `json:"out"`
	Err  string `json:"err,omitempty"`
}

// folgeSChatCorpus is the answer corpus: the engines' shapes, the
// normalisations (logprobs, reasoning, usage filter), the encoder's
// escapes, duplicate and escaped keys, and the refusals.
func folgeSChatCorpus() []folgeSChatCase {
	sglang := `{"id":"ID-CANARY-sglang","object":"chat.completion","created":1700000000,"model":"MODEL-CANARY",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from the engine","reasoning_content":"THINK-CANARY","tool_calls":null},` +
		`"logprobs":null,"finish_reason":"stop","matched_stop":151645}],` +
		`"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3,"prompt_tokens_details":null},"metadata":{"weight_version":"HOST-CANARY"}}`
	llama := `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"hello from the engine"}}],` +
		`"created":1700000000,"model":"MODEL-CANARY","system_fingerprint":"b6000-HOST-CANARY","object":"chat.completion",` +
		`"usage":{"completion_tokens":3,"prompt_tokens":11,"total_tokens":14},"id":"chatcmpl-ID-CANARY",` +
		`"timings":{"prompt_n":11,"prompt_ms":1.5,"predicted_n":3}}`
	openrouter := `{"id":"gen-1","provider":"Prov","model":"vendor/model","object":"chat.completion","created":1700000000,` +
		`"choices":[{"logprobs":null,"finish_reason":"stop","native_finish_reason":"stop","index":0,` +
		`"message":{"role":"assistant","content":"hi","refusal":null,"reasoning":"R-TEXT","reasoning_details":[{"type":"reasoning.text","text":"RD","format":"unknown","index":0}]}}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"cost":0.0001,"is_byok":false,` +
		`"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},"cost_details":{"upstream_inference_cost":null},"completion_tokens_details":{"reasoning_tokens":1}}}`
	tools := `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Berlin\", \"unit\": \"<c>\"}"}}]},"finish_reason":"tool_calls"}]}`
	escapes := `{"id":"e","choices":[{"index":0,"message":{"role":"assistant","content":"<b>&amp;</b> \u003c \u2028 ` + "  " + ` \u00e4ä € 😀 \ud83d\ude00 \ud800 \"q\" \\ \/ \b\f\n\r\t \u0001 \u007f` + "\x7f" + `"},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	invalidUTF8 := `{"id":"e","choices":[{"index":0,"message":{"role":"assistant","content":"a` + "\xff\xfe" + `b` + "\xe2\x82" + `c` + "\xed\xa0\x80" + `"},"finish_reason":"stop","x` + "\xff" + `":1}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	spaced := " \n\t{ \"id\" : \"s\" , \"choices\" : [ { \"index\" : 0 , \"message\" : { \"role\" : \"assistant\" , \"content\" : \"  a  b  \" , \"n\" : [ 1 , 2 , { \"k\" : null } ] } , \"finish_reason\" : \"stop\" } ] , \"usage\" : { \"total_tokens\" : 2 , \"prompt_tokens\" : 1 , \"completion_tokens\" : 1 } }\r\n"
	dupKeys := `{"id":"d","choices":[{"index":0,"message":{"role":"assistant","content":"first"},"finish_reason":"stop"}],` +
		`"choices":[{"index":1,"message":{"role":"assistant","content":"second","content":"third"},"finish_reason":"length","finish_reason":"stop","index":2}],` +
		`"usage":{"prompt_tokens":1,"prompt_tokens":9,"completion_tokens":2,"total_tokens":3},"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9,"prompt_tokens_details":{"cached_tokens":1},"prompt_tokens_details":null}}`
	escKeys := `{"id":"k","\u0063hoices":[{"ind\u0065x":0,"m\u0065ssage":{"r\u006fle":"assistant","cont\u0065nt":"esc","reasoning_\u0063ontent":"RC"},"fin\u0069sh_reason":"stop","\u003c":"lt","Z":1,"a":2,"\u00e4":3,"é":4,"a\/b":5}],` +
		`"Choices":[{"index":9}],"usag\u0065":{"prompt_t\u006fkens":1,"completion_tokens":1,"total_tokens":2,"Total_tokens":7}}`
	multi := `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"},null,` +
		`{"index":1,"message":{"role":"assistant","content":"b","reasoning_content":null},"logprobs":{"content":[{"token":"b","logprob":-0.5,"bytes":[98],"top_logprobs":[]}]},"finish_reason":"stop"},` +
		`{"message":{"role":"assistant","content":"no index"}},{},{"message":null},{"message":"text"},{"message":[1,2]}],"usage":null}`
	numbers := `{"id":"n","choices":[{"index":0,"message":{"role":"assistant","content":"x","n":[1.0,1e3,-0,0.5E-2,12345678901234567890,1.7976931348623157e309]},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1e2,"completion_tokens":1.5,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":1,"x":[]},"completion_tokens_details":7}}`
	usageKinds := []string{
		`{"id":"u","choices":[],"usage":"str"}`,
		`{"id":"u","choices":[],"usage":[1]}`,
		`{"id":"u","choices":[],"usage":{}}`,
		`{"id":"u","choices":[],"usage":{"cost":1,"other":{"a":1}}}`,
		`{"id":"u","choices":[]}`,
	}
	refusals := []string{
		`{"id":"r"}`,
		`{"id":"r","choices":null}`,
		`{"id":"r","choices":{"index":0}}`,
		`{"id":"r","choices":"x"}`,
		`{"id":"r","choices":[1]}`,
		`{"id":"r","choices":[[]]}`,
		`{"id":"r","choices":["x"]}`,
		`{"id":"r","choices":[true]}`,
		`[]`,
		`"x"`,
		`null`,
		`12`,
		`{"id":"r","choices":[`,
		`not json`,
		``,
		`{"id":"r","choices":[]} trailing`,
		`{"id":"r","choices":[]}{}`,
	}
	var out []folgeSChatCase
	for _, lane := range []string{"", "bulk"} {
		for _, off := range []bool{false, true} {
			suffix := "-" + lane
			if off {
				suffix += "-off"
			}
			for _, c := range []struct{ n, u string }{
				{"sglang", sglang}, {"llama", llama}, {"openrouter", openrouter}, {"tools", tools}, {"escapes", escapes},
				{"invalid-utf8", invalidUTF8}, {"spaced", spaced}, {"dup-keys", dupKeys}, {"esc-keys", escKeys}, {"multi", multi}, {"numbers", numbers},
			} {
				out = append(out, folgeSChatCase{c.n + suffix, lane, off, c.u})
			}
		}
	}
	for i, u := range usageKinds {
		out = append(out, folgeSChatCase{"usage-kind-" + string(rune('a'+i)), "", false, u})
	}
	for i, u := range refusals {
		out = append(out, folgeSChatCase{"refusal-" + string(rune('a'+i)), "", false, u})
	}
	return out
}

func folgeSChatTrace() []folgeSChatGolden {
	var out []folgeSChatGolden
	for _, c := range folgeSChatCorpus() {
		body, err := rewriteChat([]byte(c.upstream), "chatcmpl-GOLDEN<&>", 1700000000, "alias-<b>&", c.lane, c.reasoningOff)
		g := folgeSChatGolden{Name: c.name, Out: body}
		if err != nil {
			g.Err = err.Error()
		}
		out = append(out, g)
	}
	return out
}

// TestRewriteChatTraceIsTheBase compares the non-stream rewrite of the
// corpus with the golden file.
func TestRewriteChatTraceIsTheBase(t *testing.T) {
	got := folgeSChatTrace()
	if os.Getenv("CTX_FOLGE_S_CHAT_GOLDEN") == "update" {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(got); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(folgeSChatGoldenPath, b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d answers)", folgeSChatGoldenPath, len(got))
		return
	}
	raw, err := os.ReadFile(folgeSChatGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var want []folgeSChatGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("trace has %d answers, golden %d", len(got), len(want))
	}
	refused := 0
	for i := range got {
		g, w := got[i], want[i]
		if w.Err != "" {
			refused++
		}
		if g.Name != w.Name || !bytes.Equal(g.Out, w.Out) || g.Err != w.Err {
			t.Errorf("%s: got out=%q err=%q\nbase out=%q err=%q", w.Name, g.Out, g.Err, w.Out, w.Err)
		}
	}
	if refused == 0 || refused == len(want) || !strings.Contains(string(raw), `"name"`) {
		t.Fatalf("golden of %d answers has %d refusals: not the corpus", len(want), refused)
	}
}
