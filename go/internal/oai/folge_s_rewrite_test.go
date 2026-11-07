package oai

// The differential gate of FOLGE-S (RC-D S1, cluster C8): the client SSE
// line of every upstream chunk of the trace corpus is byte for byte the
// base's (rewriteChunk at ab6b2415 plus the stream writer's frame), with the
// same skip, runes, usage and prompt_n, chunk by chunk through a role carry
// per sequence. The golden file was written from the base
// (CTX_FOLGE_S_GOLDEN=update); a preservation pin — green on the S1 stub by
// construction, it holds S2's token walk to the base's bytes.

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const folgeSGoldenPath = "testdata/folge_s_rewrite_trace.json"

type folgeSSeq struct {
	name                       string
	alias, lane                string
	reasoningOff, includeUsage bool
	chunks                     []string
}

// folgeSGolden is one chunk's rewrite as the base wrote it.
type folgeSGolden struct {
	Seq     string  `json:"seq"`
	I       int     `json:"i"`
	Out     string  `json:"out"`
	Skip    bool    `json:"skip"`
	Runes   int     `json:"runes"`
	Usage   *[2]int `json:"usage"`
	PromptN int     `json:"prompt_n"`
	Err     string  `json:"err,omitempty"`
}

func folgeSTraceCorpus() []folgeSSeq {
	sg := func(delta, finish, usage string) string {
		return `{"id":"ID-sg","object":"chat.completion.chunk","created":1700000000,"model":"MODEL",` +
			`"choices":[{"index":0,"delta":` + delta + `,"logprobs":null,"finish_reason":` + finish + `,"matched_stop":null}],"usage":` + usage + `}`
	}
	usage := `{"id":"ID-sg","object":"chat.completion.chunk","created":1700000000,"model":"MODEL","choices":[],"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":null,"cost":0.1}}`
	plain := []string{
		sg(`{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}`, "null", "null"),
		sg(`{"role":null,"content":"Hallo ä € 😀 <b>&amp;</b>   \"q\" \\ \n\t","reasoning_content":null,"tool_calls":null}`, "null", "null"),
		sg(`{"role":null,"content":"","reasoning_content":null,"tool_calls":null}`, `"stop"`, "null"),
		usage,
	}
	think := []string{
		sg(`{"role":"assistant","content":null,"reasoning_content":"THINK","tool_calls":null}`, "null", "null"),
		sg(`{"role":null,"content":null,"reasoning_content":"more","reasoning":"x","tool_calls":null}`, "null", "null"),
		sg(`{"role":null,"content":"answer","reasoning_content":null,"tool_calls":null}`, "null", "null"),
		sg(`{"content":""}`, `"stop"`, "null"),
		usage,
	}
	llama := []string{
		`{"choices":[{"finish_reason":null,"index":0,"delta":{"role":"assistant","content":null}}],"created":1762500000,"id":"chatcmpl-x","model":"m","system_fingerprint":"b6000","object":"chat.completion.chunk"}`,
		`{"choices":[{"finish_reason":null,"index":0,"delta":{"content":"tok"}}],"created":1762500000,"id":"chatcmpl-x","model":"m","system_fingerprint":"b6000","object":"chat.completion.chunk"}`,
		`{"choices":[{"finish_reason":"stop","index":0,"delta":{}}],"created":1762500000,"id":"chatcmpl-x","model":"m","system_fingerprint":"b6000","object":"chat.completion.chunk","usage":{"completion_tokens":2,"prompt_tokens":5,"total_tokens":7},"timings":{"prompt_n":5,"prompt_ms":1.5,"predicted_n":2}}`,
	}
	tools := []string{
		sg(`{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`, "null", "null"),
		sg(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`, "null", "null"),
		sg(`{"tool_calls":[{"index":0,"function":{"arguments":"\"Berlin\"}"}}]}`, "null", "null"),
		sg(`{}`, `"tool_calls"`, "null"),
	}
	edge := []string{
		`{"id":"e","choices":[{"delta":{"content":"no index"}}]}`,
		`{"id":"e","choices":[{"index":0}]}`,
		`{"id":"e","choices":null}`,
		`{"id":"e"}`,
		`{"id":"e","choices":[{"index":0,"delta":{"content":"a"}},{"index":1,"delta":{"role":"assistant","content":"b","refusal":"no"}}],"extra":{"k":[1,2]}}`,
		`{"id":"e","choices":[{"index":0,"delta":{"content":"lp"},"logprobs":{"content":[{"token":"lp","logprob":-0.1,"bytes":[108,112],"top_logprobs":[]}]}}]}`,
		`{"id":"e","choices":[{"index":0,"delta":{"content":"u"}}],"usage":null}`,
		`{"id":"e","choices":[{"index":0,"delta":{"content":"u2"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"weird":true}}`,
		`  {"id":"e","choices":[{"index":0,"delta":{"content":"  spaced  "}}]}  `,
	}
	bad := []string{
		`{"id":"b","choices":{"index":0}}`,
		`{"id":"b","choices":[`,
		`not json`,
	}
	return []folgeSSeq{
		{"plain", "chat", "", false, false, plain},
		{"plain-usage", "chat", "bulk", false, true, plain},
		{"think-off", "chat-noreason", "", true, true, think},
		{"think-on", "chat", "", false, false, think},
		{"llama", "a-very-long-alias-name-for-the-rewrite-to-grow-the-chunk", "", false, true, llama},
		{"tools", "chat", "", false, false, tools},
		{"edge", "chat", "", false, true, edge},
		{"edge-off", "chat", "", true, false, edge},
		{"bad", "chat", "", false, false, bad},
	}
}

// folgeSTrace rewrites the corpus through rewriteChunkInto, one dst per
// sequence (the per-attempt buffer of S2).
func folgeSTrace() []folgeSGolden {
	var out []folgeSGolden
	for _, s := range folgeSTraceCorpus() {
		roles := &roleCarry{}
		var dst []byte
		for i, c := range s.chunks {
			sc, err := rewriteChunkInto(dst, []byte(c), "chatcmpl-GOLDEN", 1700000000, s.alias, s.lane, s.reasoningOff, s.includeUsage, roles)
			g := folgeSGolden{Seq: s.name, I: i, Skip: sc.skip, Runes: sc.runes, Usage: sc.usage, PromptN: sc.promptN}
			if err != nil {
				g.Err = err.Error()
			} else if !sc.skip {
				g.Out = string(sc.out)
				dst = sc.out
			}
			out = append(out, g)
		}
	}
	return out
}

// TestRewriteChunkTraceIsTheBase compares the trace with the golden file.
func TestRewriteChunkTraceIsTheBase(t *testing.T) {
	got := folgeSTrace()
	if os.Getenv("CTX_FOLGE_S_GOLDEN") == "update" {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(got); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(folgeSGoldenPath, b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d chunks)", folgeSGoldenPath, len(got))
		return
	}
	raw, err := os.ReadFile(folgeSGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var want []folgeSGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("trace has %d chunks, golden %d", len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		gu, wu := "nil", "nil"
		if g.Usage != nil {
			gu = strings.TrimSpace(string(mustJSON(*g.Usage)))
		}
		if w.Usage != nil {
			wu = strings.TrimSpace(string(mustJSON(*w.Usage)))
		}
		if g.Out != w.Out || g.Skip != w.Skip || g.Runes != w.Runes || gu != wu || g.PromptN != w.PromptN || (g.Err == "") != (w.Err == "") {
			t.Errorf("%s[%d]: got out=%q skip=%v runes=%d usage=%s prompt_n=%d err=%q\nbase out=%q skip=%v runes=%d usage=%s prompt_n=%d err=%q",
				g.Seq, g.I, g.Out, g.Skip, g.Runes, gu, g.PromptN, g.Err, w.Out, w.Skip, w.Runes, wu, w.PromptN, w.Err)
		}
	}
}
