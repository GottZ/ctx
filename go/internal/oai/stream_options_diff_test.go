package oai

// stream_options as the struct decode read it (W02-08 rebase round 2, part
// A5): the parse walks the object (memberIter) instead of decoding it into
// a struct, and answers what encoding/json's decode answered — the last
// member whose decoded name is include_usage or differs from it only in
// letter case (Unicode folding: "ſ" folds to "s"), a null one as absent, a
// value other than a boolean the 400 at stream_options.include_usage, every
// other member unread. The oracle is the parse FOLGE-P replaced, with
// W02-08's readCTX hunk (parse_oracle_test.go); the cases go through the
// FOLGE-P differential (diffCheck: every limit set, every outcome field,
// includeUsage among them).

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// streamOptionsBodies are the request bodies of the stream_options cases.
func streamOptionsBodies() [][]byte {
	values := []string{
		`{"include_usage":true}`, `{"include_usage":false}`, `{"include_usage":null}`, `{}`,
		`{"include_usage": true }`, "{\"include_usage\":\n\ttrue}",
		`{"INCLUDE_USAGE":true}`, `{"Include_Usage":true}`, `{"include_uſage":true}`, `{"include_uſage":false}`,
		`{"includeusage":true}`, `{"include-usage":true}`, `{"include_usage_":true}`, `{"_include_usage":true}`,
		`{"include_usage":true}`, `{"include_usage":false}`, `{"include_USAGE":true}`, `{"include_usage":"x"}`,
		`{"include_usage":true,"include_usage":null}`, `{"include_usage":null,"include_usage":true}`,
		`{"include_usage":true,"Include_Usage":false}`, `{"Include_Usage":false,"include_usage":true}`,
		`{"include_usage":null,"INCLUDE_USAGE":true}`, `{"include_usage":1,"include_usage":true}`, `{"include_usage":true,"include_usage":1}`,
		`{"include_usage":"true"}`, `{"include_usage":1}`, `{"include_usage":0}`, `{"include_usage":[]}`, `{"include_usage":{}}`,
		`{"include_usage":[true]}`, `{"include_usage":{"a":true}}`, `{"include_usage":"` + strings.Repeat("\xff", 512) + `"}`,
		`{"include_usage":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`,
		`{"x":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `,"include_usage":true}`,
		`{"x":{"include_usage":false},"include_usage":true}`, `{"x":1,"y":"z","include_usage":false,"w":[1,2]}`,
		"{\"\xffinclude_usage\":true}", `{"include_usage\u0000":true}`, `{"":true}`, `{"include_usage":true,"":1,"":2}`,
		`[]`, `"x"`, `1`, `true`, `null`, `[{"include_usage":true}]`,
	}
	var out [][]byte
	for _, v := range values {
		for _, stream := range []string{`"stream":true,`, `"stream":false,`, ``} {
			out = append(out, []byte(fmt.Sprintf(`{"model":"chat",%s"stream_options":%s,"messages":[{"role":"user","content":"hi"}]}`, stream, v)))
		}
	}
	return out
}

func TestStreamOptionsAnswerAsTheStructDecode(t *testing.T) {
	dev := map[string]int{}
	bodies := streamOptionsBodies()
	for _, body := range bodies {
		diffCheck(t, body, dev)
	}
	t.Logf("stream_options: %d bodies × %d limit sets: %v", len(bodies), len(diffLimits), dev)
	if dev["equal"] != len(bodies)*len(diffLimits) {
		t.Errorf("%d of %d comparisons equal (%v): stream_options allows no deviation", dev["equal"], len(bodies)*len(diffLimits), dev)
	}
	// The cases must reach both answers and both values.
	var on, off, refused int
	for _, body := range bodies {
		switch n := newParse(body, diffLimits[0]); {
		case n.stage == "top" && n.param == "stream_options.include_usage":
			refused++
		case n.stage == "ok" && n.includeUsage:
			on++
		case n.stage == "ok":
			off++
		}
	}
	if on == 0 || off == 0 || refused == 0 {
		t.Errorf("include_usage true %d, false %d, refused %d — the cases miss an answer", on, off, refused)
	}
}

// TestStreamOptionsIncludeUsageOnlyOnAStream (W02-08 hardening 4, F5-1;
// Delta-Review 5 §6): include_usage is read only on a stream. Without
// stream: true (absent, false, null) stream_options answers as on root
// 829a45ec — an object, its members unread: the answer of the same body
// with stream_options {}, whatever include_usage holds (the replay of the
// differential corpus over the route answered 400 invalid_type for 222
// non-stream bodies before this hardening); with stream: true a value other
// than a boolean or null is the 400 invalid_type at
// stream_options.include_usage.
func TestStreamOptionsIncludeUsageOnlyOnAStream(t *testing.T) {
	with := func(body []byte, member string) []byte {
		tb := bytes.TrimLeft(body, " \t\r\n")
		if len(tb) == 0 || tb[0] != '{' || bytes.HasPrefix(bytes.TrimLeft(tb[1:], " \t\r\n"), []byte("}")) {
			return nil
		}
		return append([]byte("{"+member+","), tb[1:]...)
	}
	values := []string{`"x"`, `"true"`, `""`, `1`, `0`, `-1.5`, `{}`, `{"a":true}`, `[]`, `[true]`}
	for _, v := range values {
		so := `"stream_options":{"include_usage":` + v + `}`
		for _, stream := range []string{``, `"stream":false,`, `"stream":null,`} {
			base := []byte(`{"model":"chat",` + stream + `"messages":[{"role":"user","content":"hi"}]}`)
			body, empty := with(base, so), with(base, `"stream_options":{}`)
			for li, l := range diffLimits {
				got, want := newParse(body, l), newParse(empty, l)
				if !reflect.DeepEqual(got, want) || got.stage != "ok" || got.includeUsage {
					t.Errorf("limits %d, %s: %+v, want the answer without include_usage %+v", li, body, got, want)
				}
			}
			diffCheck(t, body, map[string]int{})
		}
		body := with([]byte(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`), so)
		if got := newParse(body, diffLimits[0]); got.stage != "top" || got.code != CodeInvalidType || got.param != "stream_options.include_usage" {
			t.Errorf("%s: %s %q at %q, want 400 %s at stream_options.include_usage", body, got.stage, got.code, got.param, CodeInvalidType)
		}
		diffCheck(t, body, map[string]int{})
	}
	// The differential corpus, every body without stream: true.
	corpus, compared := diffCorpus(t), 0
	for _, b := range corpus {
		if s := jsonMember(b, "stream"); s != nil && string(bytes.TrimSpace(s)) == "true" {
			continue
		}
		body, empty := with(b, `"stream_options":{"include_usage":"x"}`), with(b, `"stream_options":{}`)
		if body == nil {
			continue
		}
		compared++
		if got, want := newParse(body, diffLimits[0]), newParse(empty, diffLimits[0]); !reflect.DeepEqual(got, want) {
			t.Errorf("corpus body %.200q with include_usage \"x\": %s %q at %q, want the answer without it: %s %q at %q",
				b, got.stage, got.code, got.param, want.stage, want.code, want.param)
		}
	}
	if compared < 250 {
		t.Errorf("%d corpus bodies compared, want at least the 250 of testdata/chat_parse_corpus.jsonl", compared)
	}
	t.Logf("include_usage only on a stream: %d values, %d corpus bodies", len(values), compared)
}
