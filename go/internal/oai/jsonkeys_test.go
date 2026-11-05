package oai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// decodedMembers counts the members of a JSON object with the stdlib token
// stream — the reference for objectMembers (H-2).
func decodedMembers(t testing.TB, raw []byte) int {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return -1
	}
	n := 0
	for dec.More() {
		if _, err := dec.Token(); err != nil {
			return -1
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return -1
		}
		n++
	}
	return n
}

var objectMemberCases = []string{
	`{}`, ` { } `, `{"a":1}`, `{"a":1,"a":2}`, `{"a":"x\"y","b":"}"}`, `{"a":"\\","b":2}`, `{"a":"\\\"","b":{"c":[1,2,{"d":"]}"}]}}`,
	`{"url":"data:x","url":"http://x"}`, "{\n\t\"a\" :\r\n true , \"b\": null,\"c\":-1.5e3}", `{"a":[],"b":{},"c":""}`,
	`{"a":[[[[[[["}"]]]]]]],"a":0}`, `{"\"":1,"\\":2}`, `{"é":1,"é":2}`,
}

// TestObjectMembersMatchesTheDecoder (H-2): the allocation-free member count
// equals the decoder's on objects with escapes, braces inside strings, nested
// containers, literals and whitespace — the count is the duplicate detector.
func TestObjectMembersMatchesTheDecoder(t *testing.T) {
	for _, c := range objectMemberCases {
		if got, want := objectMembers([]byte(c)), decodedMembers(t, []byte(c)); got != want {
			t.Errorf("objectMembers(%s) = %d, want %d", c, got, want)
		}
	}
	deep := strings.Repeat(`{"a":`, 5000) + `1` + strings.Repeat(`}`, 5000)
	if got := objectMembers([]byte(deep)); got != 1 {
		t.Errorf("deep nesting: %d members, want 1", got)
	}
	if got := objectMembers([]byte(`[1]`)); got != -1 {
		t.Errorf("an array counts %d, want -1", got)
	}
	probe := []byte(objectMemberCases[6])
	if testing.AllocsPerRun(100, func() { _ = objectMembers(probe) }) != 0 {
		t.Error("objectMembers allocates")
	}
}

// FuzzObjectMembers: for every valid JSON object the count matches the
// decoder's.
func FuzzObjectMembers(f *testing.F) {
	for _, c := range objectMemberCases {
		f.Add([]byte(c))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || m == nil {
			return
		}
		if got, want := objectMembers(raw), decodedMembers(t, raw); got != want {
			t.Fatalf("objectMembers(%q) = %d, want %d", raw, got, want)
		}
	})
}

// TestCheckHull (H-2): the allocation-free hull check and its decoding path
// agree — a repeated key, a case variant, an escaped case variant, a Unicode
// fold variant ("ſ" folds to "s"), a repeat past hullPairwiseMax keys — and
// the known values come out as sent.
func TestCheckHull(t *testing.T) {
	known := []string{"type", "json_schema"}
	many := `{"type":"x",` + strings.Repeat(`"k0":1,`, 0)
	for i := 0; i < 20; i++ {
		many += `"k` + string(rune('a'+i)) + `":1,`
	}
	many += `"ka":2}`
	for _, tc := range []struct {
		raw, code, param string
	}{
		{`{"type":"a","json_schema":{}}`, "", ""},
		{`{"type":"a","vendor":1}`, "", ""},
		{`{"type":"a","type":"b"}`, CodeInvalidJSON, "rf.type"},
		{`{"type":"a","vendor":1,"vendor":2}`, CodeInvalidJSON, "rf.vendor"},
		{`{"type":"a","Type":"b"}`, CodeUnsupportedParameter, "rf.Type"},
		{`{"type":"a","Type":"b"}`, CodeUnsupportedParameter, "rf.Type"},
		{`{"type":"a","type":"b"}`, CodeInvalidJSON, "rf.type"},
		{`{"ſ":1,"json_ſchema":{}}`, CodeUnsupportedParameter, "rf.json_ſchema"},
		{many, CodeInvalidJSON, "rf.ka"},
	} {
		var vals [2][]byte
		ok, perr := checkHull("rf", -1, []byte(tc.raw), known, vals[:])
		switch {
		case !ok:
			t.Errorf("%s: no object", tc.raw)
		case tc.code == "" && perr != nil:
			t.Errorf("%s: refused %s %s, want accepted", tc.raw, perr.code, perr.param)
		case tc.code != "" && (perr == nil || perr.code != tc.code || perr.param != tc.param):
			t.Errorf("%s: %+v, want %s %s", tc.raw, perr, tc.code, tc.param)
		}
	}
	var vals [2][]byte
	if _, perr := checkHull("rf", 3, []byte(`{"type": "json_schema" ,"json_schema":{"a":1}}`), known, vals[:]); perr != nil ||
		string(vals[0]) != `"json_schema"` || string(vals[1]) != `{"a":1}` {
		t.Errorf("values %q %q (%v), want the raw values as sent", vals[0], vals[1], perr)
	}
	if _, perr := checkHull("tools", 3, []byte(`{"type":"function","Type":1}`), toolHullKeys, vals[:]); perr == nil || perr.param != "tools[3].Type" {
		t.Errorf("array level path %+v, want tools[3].Type", perr)
	}
	if ok, _ := checkHull("rf", -1, []byte(`"text"`), known, vals[:]); ok {
		t.Error("a string counts as an object")
	}
	probe := []byte(`{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`)
	if n := testing.AllocsPerRun(100, func() {
		var v [2][]byte
		_, _ = checkHull("tools", 0, probe, toolHullKeys, v[:])
	}); n != 0 {
		t.Errorf("checkHull allocates %.0f times on a plain hull", n)
	}
}
