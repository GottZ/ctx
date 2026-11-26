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
