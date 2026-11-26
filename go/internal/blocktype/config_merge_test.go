package blocktype

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestMergeConfig565 pins the update rule of issue #565: the update's config
// is laid over the stored one field by field; a field the update leaves out
// keeps its stored value, null removes it, an array or the forge_state_map is
// replaced whole. RED probes: the merge returning the update as the whole
// config (the replace the handlers did before); null kept as a value; a
// level-2 object merged key by key.
func TestMergeConfig565(t *testing.T) {
	const stored = `{"v":1,
		"retrieval":{"policy":"damped","damping_factor":0.8,"untrusted":true,"intent_patterns":["a","b"]},
		"guard":{"check":false,"candidate":false,"mode":"archive","candidates":"all"},
		"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false},
		"workflow":{"states":["open","done"],"initial":"open","terminal":["done"],"forge_state_map":{"open":"open","closed":"done"}},
		"structural_link_classes":["references"],
		"classify":{"priority":20,"title_patterns":["session"]}}`
	cases := []struct {
		name, update string
		want         map[string]any // field path -> value; nil value = absent
	}{
		{"only the factor keeps every other field", `{"v":1,"retrieval":{"damping_factor":0.45}}`, map[string]any{
			"retrieval.damping_factor": 0.45, "retrieval.untrusted": true, "retrieval.policy": "damped",
			"guard.check": false, "dream.linkable": false, "digest.include": false, "overview.include": false,
			"classify.priority": float64(20)}},
		{"envelope only changes nothing", `{"v":1}`, map[string]any{
			"retrieval.untrusted": true, "retrieval.damping_factor": 0.8, "guard.check": false}},
		{"empty section keeps the stored one", `{"v":1,"classify":{}}`, map[string]any{
			"classify.priority": float64(20), "classify.title_patterns": []any{"session"}}},
		{"explicit false replaces", `{"v":1,"retrieval":{"untrusted":false}}`, map[string]any{
			"retrieval.untrusted": false, "retrieval.damping_factor": 0.8}},
		{"null removes a field", `{"v":1,"retrieval":{"untrusted":null}}`, map[string]any{
			"retrieval.untrusted": nil, "retrieval.policy": "damped"}},
		{"null removes a section", `{"v":1,"guard":null}`, map[string]any{
			"guard": nil, "retrieval.untrusted": true}},
		{"an array is replaced whole", `{"v":1,"retrieval":{"intent_patterns":["c"]},"structural_link_classes":["duplicate-of"]}`, map[string]any{
			"retrieval.intent_patterns": []any{"c"}, "structural_link_classes": []any{"duplicate-of"}}},
		{"forge_state_map is replaced whole", `{"v":1,"workflow":{"forge_state_map":{"closed":"done"}}}`, map[string]any{
			"workflow.forge_state_map": map[string]any{"closed": "done"}, "workflow.terminal": []any{"done"}}},
		{"a new section is added without its nulls", `{"v":1,"parent":{"mode":"none","relationship":null}}`, map[string]any{
			"parent.mode": "none", "parent.relationship": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := MergeConfig("probe", []byte(stored), []byte(tc.update))
			if err != nil {
				t.Fatalf("MergeConfig: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("merged config is not JSON: %v (%s)", err, out)
			}
			for path, want := range tc.want {
				got, ok := lookupPath(m, path)
				if want == nil {
					if ok {
						t.Errorf("%s = %v, want it absent (%s)", path, got, out)
					}
					continue
				}
				if !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %v (present %v), want %v (%s)", path, got, ok, want, out)
				}
			}
			if _, ok := m["v"]; !ok {
				t.Errorf("merged config lost v: %s", out)
			}
		})
	}
}

// TestMergeConfig565_FullConfigIsTheSame: a config that names every field the
// row stores writes exactly itself — the shape the admin form and the
// documented "copy the stored config and change one value" send. Checked over
// the stored config with one value changed and over the stored config as is.
func TestMergeConfig565_FullConfigIsTheSame(t *testing.T) {
	const stored = `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.8,"untrusted":true},"guard":{"check":false,"candidate":false,"mode":"archive","candidates":"all"},"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false},"parent":{"mode":"none"},"classify":{}}`
	full := strings.Replace(stored, `"damping_factor":0.8`, `"damping_factor":0.45`, 1)
	for _, update := range []string{stored, full} {
		out, err := MergeConfig("session-digest", []byte(stored), []byte(update))
		if err != nil {
			t.Fatalf("MergeConfig: %v", err)
		}
		var got, want any
		_ = json.Unmarshal(out, &got)
		_ = json.Unmarshal([]byte(update), &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("full config merged to %s, want exactly %s", out, update)
		}
	}
}

// TestMergeConfig565_UpdateIsCheckedAlone: the update's own part is refused
// for everything a config is refused for before its cross-field rules run, with
// the same text DecodePolicy gives — and a part the cross-field rules could not
// judge alone (damped without a factor, which the stored config carries) is
// not refused here.
func TestMergeConfig565_UpdateIsCheckedAlone(t *testing.T) {
	const stored = `{"v":1,"retrieval":{"policy":"full-pass","damping_factor":0.5}}`
	for _, tc := range []struct{ update, want string }{
		{`{"retrieval":{"damping_factor":0.4}}`, "envelope version"},
		{`{"v":2}`, "envelope version"},
		{`{"v":1,"guards":{"check":false}}`, `unknown field "guards"`},
		{`{"v":1,"retrieval":{"untrusted":true,"Untrusted":false}}`, "Untrusted"},
		{`{"v":1,"retrieval":{"policy":"damped"},"retrieval":{"untrusted":true}}`, "retrieval"},
		{`{"v":1,"classify":{"title_patterns":["` + strings.Repeat("x", maxConfigBytes) + `"]}}`, "exceeds"},
	} {
		if _, err := MergeConfig("probe", []byte(stored), []byte(tc.update)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("MergeConfig(%.60s…) err = %v, want one naming %q", tc.update, err, tc.want)
		}
		_, decErr := DecodePolicy("probe", globalScope, false, false, []byte(tc.update))
		_, mergeErr := MergeConfig("probe", []byte(stored), []byte(tc.update))
		if decErr == nil || mergeErr == nil || decErr.Error() != mergeErr.Error() {
			t.Errorf("update %.60s…: merge error %v, decode error %v — want the same text", tc.update, mergeErr, decErr)
		}
	}
	out, err := MergeConfig("probe", []byte(stored), []byte(`{"v":1,"retrieval":{"policy":"damped"}}`))
	if err != nil {
		t.Fatalf("a damped policy whose factor the row stores: %v", err)
	}
	if _, err := DecodePolicy("probe", globalScope, false, false, out); err != nil {
		t.Fatalf("merged config %s does not decode: %v", out, err)
	}
	if _, err := MergeConfig("probe", []byte(`[1]`), []byte(`{"v":1}`)); err == nil || !strings.Contains(err.Error(), "stored config") {
		t.Errorf("a stored config that is no object: err = %v, want a stored-config error", err)
	}
}

// TestMergeConfig565_NumbersAsWritten: a stored number is carried as written,
// not through a float64.
func TestMergeConfig565_NumbersAsWritten(t *testing.T) {
	out, err := MergeConfig("probe", []byte(`{"v":1,"classify":{"priority":9007199254740993}}`), []byte(`{"v":1,"dream":{"linkable":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Errorf("merged config %s lost the stored number's digits", out)
	}
}

func lookupPath(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// TestMergeConfig565_NoHTMLEscaping: strings with & < > keep their size through
// the merge. 24 title patterns of 128 such characters are 3 KiB as stored;
// HTML-escaped they are 18 KiB, past maxConfigBytes, and the unchanged config
// sent back would not decode. RED on 02a1a26e (json.Marshal escapes).
func TestMergeConfig565_NoHTMLEscaping(t *testing.T) {
	pats := make([]string, 24)
	for i := range pats {
		pats[i] = strings.Repeat("&", 64) + strings.Repeat("<>", 32)
	}
	var b strings.Builder
	b.WriteString(`{"v":1,"classify":{"title_patterns":[`)
	for i, p := range pats {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + p + `"`)
	}
	b.WriteString(`]}}`)
	stored := b.String()
	out, err := MergeConfig("probe", []byte(stored), []byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > len(stored)+16 || strings.Contains(string(out), `\u0026`) || strings.HasSuffix(string(out), "\n") {
		t.Fatalf("merged config is %d bytes for %d stored (escaped or newline-terminated): %.80s…", len(out), len(stored), out)
	}
	if _, err := DecodePolicy("probe", globalScope, false, false, out); err != nil {
		t.Fatalf("the unchanged config does not decode after the merge: %v", err)
	}
}

// TestUpdateConfig565_BrokenStored: over a stored config that does not decode
// an update is a replacement — the whole config — and must name every section
// the stored config names (in any spelling) and, for a builtin, every section
// its shipped definition sets; nulls in it are dropped. RED on 02a1a26e (no
// such path: the merge keeps the broken key).
func TestUpdateConfig565_BrokenStored(t *testing.T) {
	whole := `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.8,"untrusted":true},"guard":{"check":false,"candidate":false},"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`
	for _, tc := range []struct {
		label, name, stored, update, wantErr string
	}{
		{"unknown key, whole config", "session-digest", strings.Replace(whole, `{"v":1,`, `{"v":1,"zz_legacy":{"x":1},`, 1), whole, ""},
		{"unknown key, partial", "session-digest", strings.Replace(whole, `{"v":1,`, `{"v":1,"zz_legacy":{"x":1},`, 1), `{"v":1,"retrieval":{"damping_factor":0.5}}`, "must name"},
		{"unknown key, key null", "session-digest", strings.Replace(whole, `{"v":1,`, `{"v":1,"zz_legacy":{"x":1},`, 1), `{"v":1,"zz_legacy":null}`, `unknown field "zz_legacy"`},
		{"another spelling, whole config", "opsx", `{"v":1,"Digest":{"include":false},"classify":{"priority":3}}`, `{"v":1,"digest":{"include":false},"classify":{"priority":3}}`, ""},
		{"another spelling, partial", "opsx", `{"v":1,"Digest":{"include":false},"classify":{"priority":3}}`, `{"v":1,"classify":{"priority":4}}`, "digest"},
		{"no object, builtin, partial", "session-digest", `[]`, `{"v":1,"retrieval":{"damping_factor":0.5}}`, "guard"},
		{"no object, builtin, whole config", "session-digest", `[]`, whole, ""},
		{"no object, own type, anything that decodes", "opsx", `[]`, `{"v":1,"retrieval":{"damping_factor":0.5,"untrusted":null}}`, ""},
		{"no object, own type, an unknown key named null", "opsx", `[]`, `{"v":1,"retrieval":{"damping_factor":0.5,"zz":null}}`, `unknown field "zz"`},
		{"whole config with its own error", "opsx", `[]`, `{"v":1,"guards":{}}`, `unknown field "guards"`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			out, err := UpdateConfig(tc.name, globalScope, tc.name == "session-digest", false, []byte(tc.stored), []byte(tc.update))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || (!strings.HasPrefix(tc.wantErr, "unknown field") && !strings.Contains(err.Error(), "stored config does not decode")) {
					t.Fatalf("err = %v, want one naming the broken stored config and %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateConfig: %v", err)
			}
			if strings.Contains(string(out), "null") || strings.Contains(string(out), "zz") || strings.Contains(string(out), "Digest") {
				t.Fatalf("replacement %s carries a null or a key of the broken row", out)
			}
			if _, err := DecodePolicy(tc.name, globalScope, false, false, out); err != nil {
				t.Fatalf("replacement %s does not decode: %v", out, err)
			}
		})
	}
	// A stored config that decodes is merged, as before.
	out, err := UpdateConfig("session-digest", globalScope, true, false, []byte(whole), []byte(`{"v":1,"retrieval":{"damping_factor":0.5}}`))
	if err != nil || !strings.Contains(string(out), `"untrusted":true`) {
		t.Fatalf("merge over a decodable row: %s, %v", out, err)
	}
}

// TestUpdateConfig565_RequestCheckedAsSent: over a stored config that does not
// decode, the request is checked as sent before its nulls are dropped — a
// repeated key, an unknown key named null and a request over the size cap that
// its nulls carry are refused like MergeConfig refuses them, with the text
// DecodePolicy gives. RED on 9c701044: the checks ran on the copy without the
// nulls, which keeps the last of a repeated key (untrusted false) and is under
// the cap.
func TestUpdateConfig565_RequestCheckedAsSent(t *testing.T) {
	under := sizedConfig(t, maxConfigBytes-12)
	for _, tc := range []struct{ label, update, want string }{
		{"a repeated key", `{"v":1,"retrieval":{"untrusted":true,"untrusted":false}}`, "repeats"},
		{"an unknown key named null", `{"v":1,"zz_future":null}`, `unknown field "zz_future"`},
		{"over the cap by its nulls", strings.TrimSuffix(under, "}") + `,"parent":null,"workflow":null}`, "exceeds"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			out, err := UpdateConfig("opsx", globalScope, false, false, []byte(`[]`), []byte(tc.update))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("UpdateConfig over a broken row = %.80s…, %v — want an error naming %q", out, err, tc.want)
			}
			_, decErr := DecodePolicy("opsx", globalScope, false, false, []byte(tc.update))
			if decErr == nil || decErr.Error() != err.Error() {
				t.Errorf("error %v, want the text DecodePolicy gives: %v", err, decErr)
			}
		})
	}
	if out, err := UpdateConfig("opsx", globalScope, false, false, []byte(`[]`), []byte(under)); err != nil || !jsonSame(out, under) {
		t.Fatalf("the same config without the nulls: %.60s…, %v — want it written as sent", out, err)
	}
}

// sizedConfig is a valid config of exactly n bytes, compact: classify arrays of
// entries of up to maxArrayItemLen characters.
func sizedConfig(t testing.TB, n int) string {
	t.Helper()
	keys := []string{"title_patterns", "source_prefixes", "metadata_flags"}
	lists := make([][]string, len(keys))
	render := func() string {
		var parts []string
		for i, k := range keys {
			if len(lists[i]) > 0 {
				parts = append(parts, `"`+k+`":["`+strings.Join(lists[i], `","`)+`"]`)
			}
		}
		return `{"v":1,"classify":{` + strings.Join(parts, ",") + `}}`
	}
	cur := 0
	lists[0] = []string{"e"}
	for s := render(); len(s) != n; s = render() {
		last := &lists[cur][len(lists[cur])-1]
		gap := n - len(s)
		switch {
		case gap < 0:
			t.Fatalf("sizedConfig(%d) overshot: %d", n, len(s))
		case len(*last) < maxArrayItemLen:
			*last += strings.Repeat("x", min(gap, maxArrayItemLen-len(*last)))
		default:
			next, cost := cur, 4 // ,"e"
			if len(lists[cur]) == maxArrayEntries {
				next = cur + 1
				if next == len(keys) {
					t.Fatalf("sizedConfig(%d) is beyond three full arrays", n)
				}
				cost = len(`,"`+keys[next]+`":["e"]`)
			}
			if gap < cost {
				*last = (*last)[:len(*last)-(cost-gap)]
			}
			cur = next
			lists[cur] = append(lists[cur], "e")
		}
	}
	s := render()
	if _, err := DecodePolicy("sized", globalScope, false, false, []byte(s)); err != nil && n <= maxConfigBytes {
		t.Fatalf("sizedConfig(%d) does not decode: %v", n, err)
	}
	return s
}

func jsonSame(a []byte, b string) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// TestUpdateConfig565_NullSectionIsNamed: over a stored config that does not
// decode, a section the request names null is named — it resets the section,
// and the replacement goes without it; a section the request leaves out is
// still missing. RED on 9c701044: the nulls were dropped before the required
// sections were looked up, so the reset was refused as a missing section.
func TestUpdateConfig565_NullSectionIsNamed(t *testing.T) {
	const stored = `{"v":1,"Digest":{"include":false},"classify":{"priority":3}}`
	out, err := UpdateConfig("opsx", globalScope, false, false, []byte(stored), []byte(`{"v":1,"digest":null,"classify":{"priority":3}}`))
	if err != nil || !jsonSame(out, `{"v":1,"classify":{"priority":3}}`) {
		t.Fatalf("digest named null over a row that stores Digest: %s, %v — want the config without the section", out, err)
	}
	if _, err := UpdateConfig("opsx", globalScope, false, false, []byte(stored), []byte(`{"v":1,"classify":{"priority":3}}`)); err == nil || !strings.Contains(err.Error(), "must name digest") {
		t.Fatalf("digest left out: %v — want a refusal naming digest", err)
	}
}

// TestUpdateConfig565_ShippedRetrievalIsRequired: over a stored config that
// names no section at all, a replacement for a builtin must still name what
// its shipped definition sets in retrieval — session-digest's damped policy,
// factor and untrusted mark. Leaving retrieval out is refused naming it. Every
// other broken row in these tests stores a retrieval section, so this is the
// one probe that fails when retrieval drops out of the shipped requirement.
func TestUpdateConfig565_ShippedRetrievalIsRequired(t *testing.T) {
	const noRetrieval = `{"v":1,"guard":{"check":false,"candidate":false},"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`
	for _, stored := range []string{`[]`, `{"v":1,"zz_legacy":{"x":1}}`} {
		out, err := UpdateConfig("session-digest", globalScope, true, false, []byte(stored), []byte(noRetrieval))
		if err == nil || !strings.Contains(err.Error(), "retrieval") || !strings.Contains(err.Error(), "must name") {
			t.Fatalf("session-digest without retrieval over %s: %s, %v — want a refusal naming retrieval", stored, out, err)
		}
	}
}

// TestUpdateConfig565_ShippedFieldsNamed: over a stored config that does not
// decode, a replacement for a builtin must name each field its compiled
// definition sets, with a value — left out, named null, or dropped with its
// section named null, the field would take the decoder default and the type
// lose what it ships with (session-digest trusted again, insight claimable).
// Named false on purpose it is written. A type with no compiled definition is
// not affected. RED on 9c701044: a whole config whose retrieval left out
// untrusted, or was {}, repaired the row and the registry served the type
// untrusted=false.
func TestUpdateConfig565_ShippedFieldsNamed(t *testing.T) {
	const whole = `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.8,"untrusted":true},"guard":{"check":false,"candidate":false},"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`
	insight, _ := json.Marshal(map[string]any{"v": 1,
		"retrieval": map[string]any{"policy": "excluded", "intent_patterns": insightPatterns, "untrusted": true},
		"guard":     map[string]any{"check": false, "candidate": false}, "dream": map[string]any{"linkable": false},
		"digest": map[string]any{"include": false}, "overview": map[string]any{"include": false},
		"write":    map[string]any{"internal_only": true},
		"classify": map[string]any{"priority": 17, "title_patterns": []string{"session insights "}}})
	for _, stored := range []string{`[]`, strings.Replace(whole, `{"v":1,`, `{"v":1,"zz_legacy":{"x":1},`, 1)} {
		for _, tc := range []struct {
			label, name, update, want string
		}{
			{"untrusted left out", "session-digest", strings.Replace(whole, `,"untrusted":true`, ``, 1), "retrieval.untrusted"},
			{"retrieval empty", "session-digest", strings.Replace(whole, `{"policy":"damped","damping_factor":0.8,"untrusted":true}`, `{}`, 1), "retrieval.damping_factor, retrieval.policy, retrieval.untrusted"},
			{"untrusted null", "session-digest", strings.Replace(whole, `"untrusted":true`, `"untrusted":null`, 1), "retrieval.untrusted"},
			{"retrieval null", "session-digest", strings.Replace(whole, `{"policy":"damped","damping_factor":0.8,"untrusted":true}`, `null`, 1), "retrieval.untrusted"},
			{"guard.candidate left out", "session-digest", strings.Replace(whole, `,"candidate":false`, ``, 1), "guard.candidate"},
			{"insight without its write section", "insight", strings.Replace(string(insight), `,"write":{"internal_only":true}`, ``, 1), "write.internal_only"},
			{"untrusted false on purpose", "session-digest", strings.Replace(whole, `"untrusted":true`, `"untrusted":false`, 1), ""},
			{"the whole config", "session-digest", whole, ""},
			{"insight whole", "insight", string(insight), ""},
			{"own type, envelope only", "opsx", `{"v":1}`, ""},
		} {
			t.Run(tc.label+" over "+stored[:2], func(t *testing.T) {
				if tc.name == "opsx" && stored != `[]` {
					t.Skip("an own type's row names its sections")
				}
				out, err := UpdateConfig(tc.name, globalScope, tc.name != "opsx", false, []byte(stored), []byte(tc.update))
				if tc.want != "" {
					if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "shipped definition") ||
						!strings.Contains(err.Error(), "false") {
						t.Fatalf("%s: %s, %v — want a refusal naming %q and how to switch a field off", tc.label, out, err, tc.want)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s: %v", tc.label, err)
				}
				if !jsonSame(out, strings.Replace(tc.update, `,"zz_legacy":{"x":1}`, ``, 1)) {
					t.Fatalf("%s: wrote %s, want the request as sent", tc.label, out)
				}
			})
		}
	}
}
