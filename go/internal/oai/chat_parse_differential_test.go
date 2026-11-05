package oai

// Gate 2 of FOLGE-P: the allocation-free parse against the base parse of
// 4839a958 (parse_oracle_test.go, kept verbatim) as a pure test oracle. Over
// a corpus — every body the route tests post (testdata/chat_parse_corpus.jsonl,
// recorded by chat_corpus_test.go), the forms of the heap probe in small,
// repeats, nesting depth 9 996 / 10 001, invalid UTF-8, escapes, whitespace —
// and under go test -fuzz both parses must agree on acceptance, error code,
// param and text, the ignored keys (X-Ctx-Ignored-Params), the CTX values,
// every PASS value byte for byte and the upstream message array. The only
// differences allowed are the ones FOLGE-P names (allowedDeviation), each
// counted.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// diffLimits are the policy limits the differential runs every body under:
// the defaults, vision with a small image cap, and tight caps.
var diffLimits = []policyLimits{
	{toolsMaxCount: 64, toolsMaxBytes: 256 << 10, responseFormatMaxBytes: 64 << 10, logitBiasMaxEntries: 64, stopMaxEntries: 4, visionMaxBytes: 4 << 20},
	{toolsMaxCount: 64, toolsMaxBytes: 256 << 10, responseFormatMaxBytes: 64 << 10, logitBiasMaxEntries: 64, stopMaxEntries: 4, vision: true, visionMaxBytes: 1024},
	{toolsMaxCount: 2, toolsMaxBytes: 300, responseFormatMaxBytes: 64, logitBiasMaxEntries: 20, stopMaxEntries: 1, vision: true, visionMaxBytes: 3},
}

// parseOutcome is what one parse leaves, in a comparable form.
type parseOutcome struct {
	stage                      string // "top", "policy" or "ok"
	code, param, message       string
	model, serviceTier         string
	stream                     bool
	n, maxTokens, messageCount int
	ignored                    []string
	pass                       map[string]string
	messages                   string
	unsized                    bool // a rewritten array whose length is not the size reserved for it
}

func errOutcome(stage string, perr *paramError) parseOutcome {
	return parseOutcome{stage: stage, code: perr.code, param: perr.param, message: perr.message}
}

// newParse runs the FOLGE-P parse.
func newParse(body []byte, l policyLimits) parseOutcome {
	req, perr := parseTopLevel(body)
	if perr != nil {
		return errOutcome("top", perr)
	}
	o := parseOutcome{model: req.model, serviceTier: req.serviceTier, stream: req.stream, n: req.n, maxTokens: req.maxTokens,
		pass: map[string]string{}}
	for k, v := range req.pass {
		o.pass[k] = string(v)
	}
	raw := req.messages
	var reserved int64
	if perr := req.parsePolicy(l, func(n int64) bool { reserved += n; return true }); perr != nil {
		o.stage, o.code, o.param, o.message = "policy", perr.code, perr.param, perr.message
		return o
	}
	o.stage, o.messageCount, o.ignored, o.messages = "ok", req.messageCount, req.ignored, string(req.messages)
	o.unsized = reserved != 0 && (int64(len(req.messages)) != reserved || cap(req.messages) != len(req.messages)) ||
		reserved == 0 && !bytes.Equal(raw, req.messages)
	return o
}

// baseParse runs the base parse (the oracle).
func baseParse(body []byte, l policyLimits) parseOutcome {
	req, perr := baseParseTopLevel(body)
	if perr != nil {
		return errOutcome("top", perr)
	}
	o := parseOutcome{model: req.model, serviceTier: req.serviceTier, stream: req.stream, n: req.n, maxTokens: req.maxTokens,
		pass: map[string]string{}}
	for k, v := range req.pass {
		o.pass[k] = string(v)
	}
	if perr := req.parsePolicy(l); perr != nil {
		o.stage, o.code, o.param, o.message = "policy", perr.code, perr.param, perr.message
		return o
	}
	o.stage, o.messageCount, o.ignored, o.messages = "ok", req.messageCount, req.ignored, string(req.messages)
	return o
}

// wireForm is a message array as llm.BuildUpstreamBody of the base sent it:
// compacted and HTML-escaped (json.Marshal of the raw value).
func wireForm(raw string) string {
	b, err := json.Marshal(json.RawMessage(raw))
	if err != nil {
		return "!" + err.Error()
	}
	return string(b)
}

// sameOutcome compares two outcomes; the message arrays by their wire form
// (FOLGE-P P-4: a rewritten message keeps the client's value bytes, the
// base had escaped them once more), and byte for byte when the base sent the
// array as it came.
func sameOutcome(body []byte, n, o parseOutcome) string {
	if n.stage != o.stage || n.code != o.code || n.param != o.param || n.message != o.message {
		return fmt.Sprintf("answer: new %s %q %q %q, base %s %q %q %q", n.stage, n.code, n.param, n.message, o.stage, o.code, o.param, o.message)
	}
	if n.stage == "top" {
		return ""
	}
	if n.model != o.model || n.serviceTier != o.serviceTier || n.stream != o.stream || n.n != o.n || n.maxTokens != o.maxTokens {
		return fmt.Sprintf("CTX values: new %+v, base %+v", n, o)
	}
	if !maps.Equal(n.pass, o.pass) {
		return fmt.Sprintf("PASS values: new %v, base %v", n.pass, o.pass)
	}
	if n.stage != "ok" {
		return ""
	}
	if n.unsized {
		return "the rewritten message array is not the size parsePolicy reserved"
	}
	if n.messageCount != o.messageCount || !slices.Equal(n.ignored, o.ignored) {
		return fmt.Sprintf("count/ignored: new %d %v, base %d %v", n.messageCount, n.ignored, o.messageCount, o.ignored)
	}
	if wireForm(n.messages) != wireForm(o.messages) {
		return fmt.Sprintf("upstream messages: new %s, base %s", wireForm(n.messages), wireForm(o.messages))
	}
	if bytes.Contains(body, []byte(o.messages)) && n.messages != o.messages {
		return fmt.Sprintf("an unchanged message array is no longer sent as it came: new %s, base %s", n.messages, o.messages)
	}
	return ""
}

// The FOLGE-P deviations from the base answer (bau report FOLGE-P
// §Antwort-Änderungen), each with its condition:
//
//   - repeatBeyondWindow (P-1): the base named a repeated key; FOLGE-P finds a
//     repeat of a key the level does not name only when the first of the two
//     is among the level's first pairwiseKeys such keys. The body then has
//     more than pairwiseKeys members on one level.
//   - logitBiasCapFirst (P-2): logit_bias past its cap is the cap's
//     unsupported_value even when a token id repeats beyond the window (the
//     base decoded the whole map to find the repeat first).
//   - oracleMapOrder: the base ranged over a Go map (message fields, a part's
//     or image_url's unknown keys, a decoded hull's case variants) and named
//     one of several refused keys at random; FOLGE-P names one of them
//     deterministically. Allowed when one of 64 base runs gives FOLGE-P's
//     answer.
func allowedDeviation(body []byte, l policyLimits, n, o parseOutcome) string {
	repeat := o.code == CodeInvalidJSON && strings.HasPrefix(o.message, "The request repeats the key")
	if repeat && manyMembers(body) {
		if n.code == CodeUnsupportedValue && n.param == "logit_bias" && strings.HasPrefix(o.param, "logit_bias.") {
			return "logitBiasCapFirst"
		}
		return "repeatBeyondWindow"
	}
	for range 64 {
		if sameOutcome(body, n, baseParse(body, l)) == "" {
			return "oracleMapOrder"
		}
	}
	return ""
}

// manyMembers reports an object level with more than pairwiseKeys members
// somewhere in body (a coarse, conservative test: the members of all levels
// are counted together).
func manyMembers(body []byte) bool {
	return bytes.Count(body, []byte(":")) > pairwiseKeys
}

// diffCheck compares both parses of body under every diffLimits entry.
func diffCheck(t testing.TB, body []byte, deviations map[string]int) {
	t.Helper()
	for li, l := range diffLimits {
		n, o := newParse(body, l), baseParse(body, l)
		d := sameOutcome(body, n, o)
		if d == "" {
			deviations["equal"]++
			continue
		}
		if kind := allowedDeviation(body, l, n, o); kind != "" {
			deviations[kind]++
			continue
		}
		t.Errorf("limits %d, body %.300q: %s", li, body, d)
	}
}

// diffCorpus is the corpus of the differential.
func diffCorpus(t testing.TB) [][]byte {
	t.Helper()
	f, err := os.Open("testdata/chat_parse_corpus.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var s string
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("corpus line %q: %v", sc.Bytes(), err)
		}
		out = append(out, []byte(s))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return append(out, generatedCorpus()...)
}

// generatedCorpus are the probe forms in small, repeats, depth, invalid
// UTF-8, escapes and whitespace variants.
func generatedCorpus() [][]byte {
	msg := `{"model":"chat","messages":[{"role":"user","content":"x"}]`
	rep := func(s string, n int) string { return strings.Repeat(s, n) }
	join := func(f func(i int) string, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = f(i)
		}
		return strings.Join(parts, ",")
	}
	keys := func(n int) string { return join(func(i int) string { return fmt.Sprintf(`"k%03d":0`, i) }, n) }
	img := `data:image/png;base64,AAAA`
	nest := func(d int) string { return rep("[", d) + rep("]", d) }
	var c []string
	for _, n := range []int{1, 15, 16, 17, 40} {
		c = append(c,
			`{"model":"chat","messages":[`+join(func(int) string { return `{"role":"user","content":"x"}` }, n)+`]}`,
			`{"model":"chat","messages":[`+join(func(int) string { return `{"role":"developer","content":"x"}` }, n)+`]}`,
			`{"model":"chat","messages":[{"role":"user","content":[`+join(func(int) string { return `{"type":"text","text":"x"}` }, n)+`]}]}`,
			msg+`,"stop":[`+join(func(int) string { return `"x"` }, n)+`]}`,
			msg+`,"logit_bias":{`+join(func(i int) string { return fmt.Sprintf(`"%d":0`, i) }, n)+`}}`,
			msg+`,"logit_bias":{`+join(func(i int) string { return fmt.Sprintf(`"%d":0`, i%7) }, n)+`}}`,
			msg+`,`+keys(n)+`}`,
			msg+`,`+keys(n)+`,"k000":1}`,
			msg+`,"modalities":[`+join(func(int) string { return `"text"` }, n)+`]}`,
			msg+`,"tools":[`+join(func(int) string { return `{"type":"function","function":{"name":"f"}}` }, n)+`]}`,
			msg+`,"tool_choice":{"type":"function","function":{"name":"f"},`+keys(n)+`}}`,
			msg+`,"tool_choice":{"type":"function","function":{"name":"f"},`+keys(n)+`,"Type":1}}`,
			msg+`,"tool_choice":{"type":"function",`+keys(n)+`,"function":{"name":"f"},"function":1}}`,
			`{"model":"vis","messages":[{"role":"user","content":[`+join(func(int) string { return `{"type":"image_url","image_url":{"url":"data:image\/png;base64,AAAA"}}` }, n)+`]}]}`,
		)
	}
	c = append(c,
		msg+`,"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":`+nest(9996)+`}}}`,
		msg+`,"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":`+nest(10001)+`}}}`,
		msg+`,"tools":[{"type":"function","function":{"name":"f","parameters":`+nest(9990)+`}}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[`+nest(9990)+`]}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"<a&b>  `+"  "+`"}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"`+"\xff\xfe"+`"}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"x","`+"\xff"+`":1,"`+"\xfe"+`":2}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}],"`+"\xff"+`":1,"`+"\xfe"+`":2}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}],"\ud800":1,"\udbff":2}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}],"😀":1,"😀":2}`,
		`{"model":"chat","messages":[{"role":"developer","content":"x","reasoning":"r"}]}`,
		`{"model":"chat","model":"chat","messages":[]}`,
		"{\n \"model\" : \"chat\" ,\r\n\t\"messages\" : [ { \"role\" : \"developer\" , \"content\" : [ { \"type\" : \"text\" , \"text\" : \"x\" } ] , \"name\" : null } ] , \"temperature\" : 0.5 , \"stop\" : [ \"a\" , null ] }",
		`{"model":"chat","messages":[{"role":"assistant","content":null,"name":1,"refusal":2,"tool_call_id":3,"tool_calls":4}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+img+`","b":1,"a":2}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+img+`"},"text":"x","a":1}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA="}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=A"}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAB="}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,===="}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/gif;base64,`+rep("A", 4096)+`"}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":null,"detail":null}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":null},{"type":1}]}]}`,
		`{"model":"chat","messages":[{"role":null,"content":"x"}]}`,
		msg+`,"n":1`+rep("0", 400)+`}`,
		msg+`,"max_tokens":-0,"max_completion_tokens":0}`,
		msg+`,"top_logprobs":-0}`,
		msg+`,"top_logprobs":100}`,
		msg+`,"modalities":["text"]}`,
		msg+`,"modalities":[null]}`,
		msg+`,"modalities":null}`,
		msg+`,"stop":[1]}`,
		msg+`,"stop":"x"}`,
		`[]`, `null`, `"x"`, ` {} `, `{"model":"chat","messages":[{"role":"user","content":"x"}]} x`,
		// Unknown keys whose body order is not their sort order, on every
		// refusing level: the smallest is named.
		msg+`,"zz":1,"\u0061a":1,"mm":1}`,
		`{"model":"chat","messages":[{"role":"user","content":"x","zz":1,"aa":1}]}`,
		`{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x","zz":1,"aa":1}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+img+`","zz":1,"aa":1}}]}]}`,
		`{"zz":1,"model":"chat","aa":1,"messages":[]}`,
		// The named deviations (allowedDeviation): a repeat beyond the
		// window on a refusing level, on a hull and in logit_bias past its cap.
		msg+`,`+keys(40)+`,"k030":1}`,
		`{"model":"chat","messages":[{"role":"user","content":"x",`+keys(40)+`,"k030":1}]}`,
		msg+`,"tool_choice":{"type":"function","function":{"name":"f"},`+keys(40)+`,"k030":1}}`,
		msg+`,"logit_bias":{`+join(func(i int) string { return fmt.Sprintf(`"%d":0`, i) }, 100)+`,"50":1}}`,
	)
	out := make([][]byte, len(c))
	for i, s := range c {
		out[i] = []byte(s)
	}
	return out
}

// TestChatParseDifferential is gate 2 over the corpus.
func TestChatParseDifferential(t *testing.T) {
	corpus := diffCorpus(t)
	dev := map[string]int{}
	for _, body := range corpus {
		diffCheck(t, body, dev)
	}
	t.Logf("FOLGEP differential: %d bodies × %d limit sets: %v", len(corpus), len(diffLimits), dev)
	if dev["equal"] == 0 {
		t.Fatal("no body compared equal: the corpus did not load")
	}
}

// FuzzChatParseDifferential is gate 2 under the fuzzer (seeded with the
// corpus): go test -run '^$' -fuzz FuzzChatParseDifferential -fuzztime 120s.
func FuzzChatParseDifferential(f *testing.F) {
	for _, body := range diffCorpus(f) {
		if len(body) <= 4<<10 {
			f.Add(body)
		}
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		diffCheck(t, body, map[string]int{})
	})
}
