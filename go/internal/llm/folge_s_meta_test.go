package llm

// FOLGE-S S2 (RC-D, cluster C8): decodeChunkMeta answers as the base relay's
// decode — json.Unmarshal into streamChunk — with the same values and the
// same error text, for the shapes engines send and for the ones they should
// not (type errors in the text fields, duplicate, case-variant and escaped
// keys, null after a value, invalid UTF-8). A differential test against the
// base decode, which stays in the tree for the chat harness; the fuzz
// target extends it.

import (
	"encoding/json"
	"fmt"
	"testing"
)

// baseChunkMeta is the base relay's read of a chunk (sseRelay.dispatch at
// 437a9f49).
func baseChunkMeta(payload []byte) (chunkMeta, error) {
	var c streamChunk
	if err := json.Unmarshal(payload, &c); err != nil {
		return chunkMeta{}, err
	}
	m := chunkMeta{ID: c.ID, Model: c.Model, Usage: c.Usage, Timings: c.Timings, Error: c.Error}
	if len(c.Choices) > 0 {
		m.FinishReason = c.Choices[0].FinishReason
	}
	return m, nil
}

var folgeSMetaCorpus = []string{
	`{"id":"I","model":"M","choices":[{"index":0,"delta":{"content":"a","reasoning":"r"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"cost":0.5},"timings":{"prompt_n":9,"predicted_n":2}}`,
	`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"f","arguments":{"a":[1,2]}}}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"index":1.5}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"index":"0"}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"id":5}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"function":{"name":[]}}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"function":"f"}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":{"index":0}}}]}`,
	`{"choices":[{"delta":{"tool_calls":[1]}}]}`,
	`{"choices":[{"delta":{"content":5}}]}`,
	`{"choices":[{"delta":{"reasoning":{}}}]}`,
	`{"choices":[{"delta":5}]}`,
	`{"choices":[{"delta":null,"finish_reason":null}]}`,
	`{"choices":[null,{"finish_reason":"stop"}]}`,
	`{"choices":[{"finish_reason":5}]}`,
	`{"choices":{}}`,
	`{"id":5}`,
	`{"id":"x","id":null}`,
	`{"id":"x","ID":"y","Id":null}`,
	`{"MODEL":"m","Choices":[{"FINISH_REASON":"length"}]}`,
	`{"\u0069d":"esc-key","fin\u0069sh_reason":"no"}`,
	`{"id":"a\u00e4\n\ud83d\ude00\ud800 \"q\"","model":"\u003c"}`,
	"{\"id\":\"a\xffb\",\"model\":\"\xe2\x82\"}",
	`{"usage":{"prompt_tokens":1e2}}`,
	`{"usage":{"prompt_tokens":-0,"completion_tokens":9223372036854775807,"cost":1e400}}`,
	`{"usage":null,"timings":null,"error":null}`,
	`{"error":{"code":502,"message":"upstream died"}}`,
	`{"error":{"code":"x","message":5}}`,
	`{"error":"x"}`,
	`{"choices":[{"delta":{"content":"a"}}]} `,
	`[]`,
	`null`,
	`"x"`,
	`{"choices":[`,
	`{"id":"a"}{}`,
	``,
	`{"choices":[{"finish_reason":"a"}],"choices":[{"finish_reason":"b"}]}`,
	`{"choices":[{"finish_reason":"a"},{"finish_reason":"c"}],"choices":[{"index":0}]}`,
	`{"usage":{"prompt_tokens":1},"usage":{"completion_tokens":2}}`,
	"{\"choices\":[{\"delta\":{\"content\":\"a\x01b\"}}]}",
	"{\"id\":\"a\tb\"}",
	`{"id":"a\x"}`,
	`{"id":"a\u12"}`,
	`{"usage":{"prompt_tokens":01}}`,
	`{"usage":{"prompt_tokens":1.}}`,
	`{"usage":{"cost":-}}`,
	`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":{"k":[1,{"x":"\u00e4"}]}}}]}}],"id":"t"}`,
	`{"a":[1,"x",{"b":null},[true,false]],"id":"skip"}`,
}

func folgeSMetaSame(t *testing.T, payload []byte) {
	t.Helper()
	got, gerr := decodeChunkMeta(payload)
	want, werr := baseChunkMeta(payload)
	g := fmt.Sprintf("%q %q %q %+v %+v %+v err=%v", got.ID, got.Model, got.FinishReason, got.Usage, got.Timings, got.Error, gerr)
	w := fmt.Sprintf("%q %q %q %+v %+v %+v err=%v", want.ID, want.Model, want.FinishReason, want.Usage, want.Timings, want.Error, werr)
	if got.Usage != nil && want.Usage != nil && (got.Usage.Cost == nil) == (want.Usage.Cost == nil) && (got.Usage.Cost == nil || *got.Usage.Cost == *want.Usage.Cost) {
		gu, wu := *got.Usage, *want.Usage
		gu.Cost, wu.Cost = nil, nil
		got.Usage, want.Usage = &gu, &wu
		g = fmt.Sprintf("%q %q %q %+v %+v %+v err=%v", got.ID, got.Model, got.FinishReason, got.Usage, got.Timings, got.Error, gerr)
		w = fmt.Sprintf("%q %q %q %+v %+v %+v err=%v", want.ID, want.Model, want.FinishReason, want.Usage, want.Timings, want.Error, werr)
	}
	if got.Error != nil && want.Error != nil && string(got.Error.Code) == string(want.Error.Code) && got.Error.Message == want.Error.Message {
		got.Error, want.Error = nil, nil
		g = fmt.Sprintf("%q %q %q %+v %+v err=%v", got.ID, got.Model, got.FinishReason, got.Usage, got.Timings, gerr)
		w = fmt.Sprintf("%q %q %q %+v %+v err=%v", want.ID, want.Model, want.FinishReason, want.Usage, want.Timings, werr)
	}
	if g != w {
		t.Errorf("%q:\n got %s\nbase %s", payload, g, w)
	}
}

func TestDecodeChunkMetaIsTheBase(t *testing.T) {
	for _, p := range folgeSMetaCorpus {
		folgeSMetaSame(t, []byte(p))
	}
}

func FuzzDecodeChunkMetaIsTheBase(f *testing.F) {
	for _, p := range folgeSMetaCorpus {
		f.Add([]byte(p))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		folgeSMetaSame(t, payload)
	})
}

// TestWalkChunkMetaTakesTheEngineShapes: the walk itself answers the
// chunks engines send (the base decode is the exception, not the rule).
func TestWalkChunkMetaTakesTheEngineShapes(t *testing.T) {
	for _, p := range []string{
		`{"id":"I","object":"chat.completion.chunk","created":1,"model":"M","choices":[{"index":0,"delta":{"role":"assistant","content":"a \"q\" ä ä","reasoning_content":null,"tool_calls":null},"logprobs":null,"finish_reason":null,"matched_stop":null}],"usage":null}`,
		`{"choices":[{"finish_reason":"stop","index":0,"delta":{}}],"created":1,"id":"c","model":"m","system_fingerprint":"b","object":"chat.completion.chunk","usage":{"completion_tokens":2,"prompt_tokens":5,"total_tokens":7},"timings":{"prompt_n":5,"prompt_ms":1.5,"predicted_n":2}}`,
		`{"id":"gen","provider":"P","model":"v/m","choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls","native_finish_reason":"tool_calls","logprobs":null}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"cost":0.0001,"prompt_tokens_details":{"cached_tokens":0}}}`,
	} {
		m, ok := walkChunkMeta([]byte(p))
		if !ok {
			t.Errorf("the walk refused an engine chunk: %s", p)
			continue
		}
		want, err := baseChunkMeta([]byte(p))
		if err != nil || m.ID != want.ID || m.Model != want.Model || m.FinishReason != want.FinishReason {
			t.Errorf("walk %+v, base %+v %v", m, want, err)
		}
	}
}
