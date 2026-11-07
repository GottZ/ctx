package rerank

// The strict decoder's own pins (RC-D S2 FOLGE-R, beside the S1 contract
// tests): the base's member rules on the forms the corpus does not carry —
// a repeated "results" list folding into the entries the earlier one left,
// resets, repeated usage, case-folded and escaped names, the nesting limit —
// each against the base's decode (folgeRSame); the syntax check against
// json.Valid; the read's bounds; and the dialect forms AnswerEntryMax and
// AnswerFixed are derived from.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// TestDecodeAnswerFollowsTheBaseOnRepeatedMembers: answers the corpus does
// not carry, decoded as the base decodes them — scores bit for bit, the
// error text for every answer of at most n entries.
func TestDecodeAnswerFollowsTheBaseOnRepeatedMembers(t *testing.T) {
	deep := strings.Repeat("[", maxAnswerDepth-1) + strings.Repeat("]", maxAnswerDepth-1)
	for _, c := range []folgeRAnswer{
		{"second list folds into the first", `{"results":[{"index":0,"relevance_score":1},{"index":1,"relevance_score":2}],"results":[{"index":1},{"index":0}]}`, 2, true},
		{"second list repeats an index", `{"results":[{"index":0,"relevance_score":1},{"index":1,"relevance_score":2}],"results":[{"index":1},{"relevance_score":5}]}`, 2, true},
		{"longer first list, short last", `{"results":[{"index":0,"relevance_score":1},{"index":0,"relevance_score":1},{"index":0}],"results":[{"relevance_score":2}]}`, 1, true},
		{"null entry keeps the earlier", `{"results":[{"index":0,"relevance_score":3}],"results":[null]}`, 1, true},
		{"three lists", `{"results":[{"index":2}],"results":[{"relevance_score":1},{"index":1,"relevance_score":2}],"results":[{},{},{"index":0,"relevance_score":4}]}`, 3, true},
		{"null resets", `{"results":[{"index":0,"relevance_score":1}],"results":null,"results":[{"index":0}]}`, 1, true},
		{"empty list resets", `{"results":[{"index":0,"relevance_score":1}],"results":[],"results":[{"index":0}]}`, 1, true},
		{"null resets before a fold", `{"results":[{"index":0,"relevance_score":1}],"results":null,"results":[{"index":0}],"results":[{}]}`, 1, true},
		{"empty list resets before a fold", `{"results":[{"index":0,"relevance_score":1}],"results":[],"results":[{"index":0}],"results":[null]}`, 1, true},
		{"data after a reset", `{"results":[{"index":0,"relevance_score":0.5}],"results":[],"data":[{"index":0,"relevance_score":0.25}]}`, 1, true},
		{"data repeated, the last counts", `{"data":[{"index":0,"relevance_score":0.5}],"data":[{"index":0,"relevance_score":0.75}]}`, 1, true},
		{"data null after a list", `{"data":[{"index":0,"relevance_score":0.5}],"data":null}`, 1, true},
		{"results beside data", `{"data":5,"results":[{"index":0,"relevance_score":0.5}]}`, 1, true},
		{"usage repeated", `{"results":[{"index":0,"relevance_score":1}],"usage":{"prompt_tokens":3},"usage":{"total_tokens":9},"usage":null}`, 1, true},
		{"usage counter null", `{"results":[{"index":0,"relevance_score":1}],"usage":{"prompt_tokens":3,"prompt_tokens":null}}`, 1, true},
		{"index null keeps", `{"results":[{"index":1,"index":null,"relevance_score":1}]}`, 2, true},
		{"score null after a number", `{"results":[{"index":0,"relevance_score":1,"relevance_score":null}]}`, 1, true},
		{"folded names", `{"reſults":[{"INDEX":0,"relevance_ſcore":1.5}],"usage":{"prompt_to` + "\u212a" + `ens":4}}`, 1, true},
		{"escaped names", `{"res\u0075lts":[{"\u0069ndex":0,"relevance_\u017fcore":2.5}],"\u0055sage":{"total_tokens":2}}`, 1, true},
		{"escaped quote in a name", `{"res\"ults":[{"index":0,"relevance_score":1}]}`, 1, true},
		{"dash is no underscore", `{"results":[{"index":0,"relevance-score":1}]}`, 1, true},
		{"long name", `{"results_` + strings.Repeat("\\u0078", 40) + `":1,"results":[{"index":0,"relevance_score":1}]}`, 1, true},
		{"surrogates in a name", `{"\ud800results\udc00":[1],"results":[{"index":0,"relevance_score":1}]}`, 1, true},
		{"top-level null", `null`, 1, true},
		{"min index", `{"results":[{"index":-9223372036854775808,"relevance_score":1}]}`, 1, true},
		{"echo object with nested lists", `{"results":[{"document":{"text":"]}\"[{","x":[[{}],[]]},"index":0,"relevance_score":-0}]}`, 1, true},
		{"invalid UTF-8 in a string", "{\"results\":[{\"index\":0,\"relevance_score\":1,\"document\":\"\xff\xfe\"}]}", 1, true},
		// Errors on both sides, texts free.
		{"type error past n in an earlier list", `{"results":[{"index":0,"relevance_score":1},{"index":"x"}],"results":[{"index":0,"relevance_score":1}]}`, 1, false},
		{"type error in a data list beside results", `{"results":[{"index":0,"relevance_score":1}],"data":[{"index":"x"}]}`, 1, false},
		{"type error in usage after the list", `{"results":[{"index":0,"relevance_score":1}],"usage":{"prompt_tokens":1.5}}`, 1, false},
		{"results not a list", `{"results":{"index":0}}`, 1, false},
		{"entry a string", `{"results":["x"]}`, 1, false},
		{"score a bool", `{"results":[{"index":0,"relevance_score":true}]}`, 1, false},
		{"index an object", `{"results":[{"index":{},"relevance_score":1}]}`, 1, false},
		{"control character in a string", "{\"results\":[{\"index\":0,\"relevance_score\":1,\"x\":\"\x01\"}]}", 1, false},
		{"bad escape", `{"results":[{"index":0,"relevance_score":1,"x":"\q"}]}`, 1, false},
		{"bad unicode escape", `{"results":[{"index":0,"relevance_score":1,"x":"\u00g0"}]}`, 1, false},
		{"leading zero", `{"results":[{"index":01,"relevance_score":1}]}`, 1, false},
		{"bare minus", `{"results":[{"index":0,"relevance_score":-}]}`, 1, false},
		{"trailing comma", `{"results":[{"index":0,"relevance_score":1},]}`, 1, false},
		{"literal cut", `{"results":[{"index":0,"relevance_score":1,"x":tru}]}`, 1, false},
		{"nested at the limit", `{"results":[{"index":0,"relevance_score":1}],"x":` + deep + `}`, 1, true},
		{"nested past the limit", `{"results":[{"index":0,"relevance_score":1}],"x":[` + deep + `]}`, 1, false},
		{"empty", ``, 1, false},
		{"whitespace only", " \n", 1, false},
	} {
		for _, d := range []string{"", DomainAuto, DomainLogit, DomainProbability} {
			folgeRSame(t, c.name, []byte(c.raw), c.n, d, c.pinText)
		}
	}
}

// TestCheckAnswerSyntaxIsJSONValid: the syntax check admits what json.Valid
// admits (encoding/json's grammar under the v1 options), at the nesting
// limit and around it included.
func TestCheckAnswerSyntaxIsJSONValid(t *testing.T) {
	inputs := []string{
		`{}`, `[]`, `null`, `true`, `false`, `0`, `-0`, `1.5e+10`, `"x"`, ` {"a":[1,2,{"b":null}]} `,
		`{"a":1,"a":2}`, "\"\xff\"", "\"\x7f\"", `"\ud800"`, `"\/"`, `{"a" : 1 }`, "{\t\"a\"\r\n:\n1}",
		``, ` `, `{`, `}`, `[1,]`, `{"a":}`, `{"a"}`, `{1:2}`, `{"a":1,}`, `[1 2]`, `01`, `1.`, `.1`, `1e`, `1e+`, `-`,
		`+1`, `tru`, `truex`, `nul`, `"`, `"\`, `"\u12"`, `"\x"`, "\"\x00\"", "\xef\xbb\xbf{}", `{} {}`, `[}`, `{]`, `"a":1`,
		`[` + strings.Repeat("[", maxAnswerDepth-1) + strings.Repeat("]", maxAnswerDepth-1) + `]`,
		`[` + strings.Repeat("[", maxAnswerDepth) + strings.Repeat("]", maxAnswerDepth) + `]`,
		strings.Repeat(`{"a":`, maxAnswerDepth) + `1` + strings.Repeat(`}`, maxAnswerDepth),
		strings.Repeat(`{"a":`, maxAnswerDepth+1) + `1` + strings.Repeat(`}`, maxAnswerDepth+1),
	}
	for _, in := range inputs {
		got, want := checkAnswerSyntax([]byte(in)) == nil, json.Valid([]byte(in))
		if got != want {
			t.Errorf("%.40q: checkAnswerSyntax admits %v, json.Valid %v", in, got, want)
		}
	}
}

// chunks is a reader without a length that hands out its bytes in small
// reads (the transport's body of a chunked or decompressed answer).
func chunks(b []byte) io.Reader { return iotest.HalfReader(bytes.NewReader(b)) }

// TestReadAnswerStaysWithinItsCap: an answer of a known length within the
// cap is one allocation of that length, one past it is refused unread; an
// answer without a length is read into a buffer of an answer without an
// echo, grown once to the cap for an echo, refused past the cap — and the
// read allocates at most AnswerReadMax.
func TestReadAnswerStaysWithinItsCap(t *testing.T) {
	const docs, body = 4, 10 << 10
	limit := answerReadCap(docs, body)
	for _, c := range []struct {
		name    string
		size    int64
		known   bool
		refused bool
	}{
		{"known, small", 300, true, false},
		{"known, at the cap", limit, true, false},
		{"known, past the cap", limit + 1, true, true},
		{"unknown, small", 300, false, false},
		{"unknown, at the first buffer", limit - body, false, false},
		{"unknown, an echo", limit - 100, false, false},
		{"unknown, at the cap", limit, false, false},
		{"unknown, past the cap", limit + 1, false, true},
	} {
		raw := bytes.Repeat([]byte{'x'}, int(c.size))
		r := chunks(raw)
		cl := int64(-1)
		if c.known {
			cl, r = c.size, bytes.NewReader(raw)
		}
		var got []byte
		var err error
		b, m := folgeRMeasure(true, func() { got, err = readAnswer(r, cl, docs, body) })
		t.Logf("FOLGE-R read: %-28s %d B: allocated %d B in %d, err=%v (cap %d, AnswerReadMax %d)", c.name, c.size, b, m, err, limit, AnswerReadMax(docs, body))
		if c.refused {
			if err == nil {
				t.Errorf("%s: %d B read, want a refusal past the %d-byte cap", c.name, c.size, limit)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("%s: read %d B, err %v — want the %d B answer", c.name, len(got), err, c.size)
		}
		if b > uint64(AnswerReadMax(docs, body)) {
			t.Errorf("%s: the read allocated %d B, AnswerReadMax is %d B", c.name, b, AnswerReadMax(docs, body))
		}
		// An answer without an echo and without a length takes the first
		// buffer only (the cap less the body), not the cap.
		if !c.known && c.size <= limit-body && b > uint64(limit-body+AnswerReadSlack) {
			t.Errorf("%s: an answer of %d B without a length allocated %d B, want its first buffer of %d B", c.name, c.size, b, limit-body)
		}
	}
	if _, err := readAnswer(iotest.ErrReader(errors.New("boom")), -1, docs, body); err == nil {
		t.Errorf("a failing reader read without an error")
	}
}

// longestFloat is the longest literal a float64 renders to (Go's and
// nlohmann's shortest round-trip form, seventeen digits and a three-digit
// exponent with its sign).
const longestFloat = "-2.2250738585072014e-308"

// TestAnswerReadCapHoldsTheDialects derives AnswerEntryMax and AnswerFixed:
// the entries and frames of the measured answer forms — llama.cpp, Voyage,
// Cohere v1, vLLM around its document echo — with a five-digit index and
// the longest float literal stay within them, and a full answer of each
// form over a body of its documents stays within answerReadCap.
func TestAnswerReadCapHoldsTheDialects(t *testing.T) {
	const idx = 99999
	docs := []string{"a document <b>with</b> \"quotes\" and äöü", strings.Repeat("y", 300)}
	model := "BAAI/bge-reranker-v2-m3-with-a-long-repository-name-of-the-hub"
	body := RequestBodyLen(model, "the query", docs)
	entries := map[string]func(doc string, i int) string{
		"llama.cpp": func(_ string, i int) string {
			return fmt.Sprintf(`{"index":%d,"relevance_score":%s}`, i, longestFloat)
		},
		"voyage": func(_ string, i int) string {
			return fmt.Sprintf(`{"relevance_score":%s,"index":%d}`, longestFloat, i)
		},
		"cohere-v1": func(_ string, i int) string {
			return fmt.Sprintf(`{"index":%d,"relevance_score":%s}`, i, longestFloat)
		},
		"vllm-echo": func(doc string, i int) string {
			return fmt.Sprintf(`{"index":%d,"document":{"text":%s,"multi_modal":null},"relevance_score":%s}`, i, appendWireString(nil, doc), longestFloat)
		},
	}
	frames := map[string][2]string{
		"llama.cpp": {`{"model":"` + model + `","object":"list","usage":{"prompt_tokens":9223372036854775807,"total_tokens":9223372036854775807},"results":[`, `]}`},
		"voyage":    {`{"object":"list","data":[`, `],"model":"` + model + `","usage":{"total_tokens":9223372036854775807}}`},
		"cohere-v1": {`{"id":"0b5a7c1d-7000-4000-8000-00000000a001","results":[`, `],"meta":{"api_version":{"version":"1","is_deprecated":false,"is_experimental":false},"billed_units":{"search_units":9223372036854775807},"warnings":[]}}`},
		"vllm-echo": {`{"id":"rerank-0b5a7c1d70004000800000000000a001","model":"` + model + `","usage":{"total_tokens":9223372036854775807},"results":[`, `]}`},
	}
	for name, entry := range entries {
		f := frames[name]
		var answer, decodable strings.Builder
		answer.WriteString(f[0])
		decodable.WriteString(f[0])
		most := 0
		for i, d := range docs {
			e := entry(d, idx)
			if i > 0 {
				answer.WriteByte(',')
				decodable.WriteByte(',')
			}
			answer.WriteString(e)
			decodable.WriteString(entry(d, i))
			own := len(e) + 1
			if name == "vllm-echo" {
				own -= len(appendWireString(nil, d)) // the echo is the body's share
			}
			most = max(most, own)
		}
		answer.WriteString(f[1])
		decodable.WriteString(f[1])
		frame := len(f[0]) + len(f[1])
		t.Logf("FOLGE-R dialect %-10s entry beside its echo %d B (AnswerEntryMax %d), frame %d B (AnswerFixed %d), answer %d B, cap %d B",
			name, most, AnswerEntryMax, frame, AnswerFixed, answer.Len(), answerReadCap(len(docs), body))
		if most > AnswerEntryMax || frame > AnswerFixed || int64(answer.Len()) > answerReadCap(len(docs), body) {
			t.Errorf("%s: an entry of %d B, a frame of %d B or the answer of %d B is past AnswerEntryMax %d, AnswerFixed %d or the cap %d",
				name, most, frame, answer.Len(), AnswerEntryMax, AnswerFixed, answerReadCap(len(docs), body))
		}
		if scores, _, err := decodeAnswer([]byte(decodable.String()), len(docs), DomainLogit); err != nil || len(scores) != len(docs) {
			t.Errorf("%s: the form does not decode: %v %v", name, scores, err)
		}
	}
}
