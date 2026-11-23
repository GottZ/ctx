package handler

// Issue #519: the dispatcher of /api/manage refuses a NUL character in any
// field of any action before the action runs (manageNULField); these pin the
// scan itself. The route test over every action of the production table is
// TestManageRefusesANULCharacterInEveryAction (integration).

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestJSONCarriesNUL(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`"a\u0000b"`, true},
		{`{"k":"\u0000"}`, true},
		{`{"\u0000":1}`, true},
		{`["x","y\u0000"]`, true},
		{`"\\\u0000"`, true},     // an escaped backslash, then the NUL escape
		{`"\\u0000"`, false},     // a backslash, then the letters u0000
		{`"\\\\u0000"`, false},   // two backslashes, then the letters
		{`"\\\\\u0000"`, true},   // two backslashes, then the NUL escape
		{`"\u00000"`, true},      // the NUL escape, then a 0
		{`"\u0001\u00a0"`, false}, // other escapes
		{`"x"`, false},
		{``, false},
		{`null`, false},
		{`"\\u0000\u0000"`, true}, // the letters first, the escape after them
	} {
		if got := jsonCarriesNUL([]byte(tc.raw)); got != tc.want {
			t.Errorf("jsonCarriesNUL(%s) = %v, want %v", tc.raw, got, tc.want)
		}
		var v any
		if tc.raw != "" && json.Unmarshal([]byte(tc.raw), &v) == nil && carriesNUL(v) != tc.want {
			t.Errorf("%s: the decoded value says %v, the scan %v", tc.raw, carriesNUL(v), tc.want)
		}
	}
}

func TestManageNULField(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  manageRequest
		want string
	}{
		{"clean", manageRequest{Action: "get", ID: "x", Data: json.RawMessage(`{"a":"b"}`)}, ""},
		{"id", manageRequest{Action: "get", ID: "x\x00"}, "id"},
		{"category", manageRequest{Category: "\x00"}, "category"},
		{"status", manageRequest{Status: "\x00"}, "status"},
		{"types", manageRequest{Types: []string{"a", "b\x00"}}, "types"},
		{"types_exclude", manageRequest{TypesExclude: []string{"\x00"}}, "types_exclude"},
		{"block_roles_exclude", manageRequest{BlockRolesExclude: []string{"\x00"}}, "block_roles_exclude"},
		{"a data key's value", manageRequest{Data: json.RawMessage(`{"b":"ok","tenant_id":"t\u0000"}`)}, "data.tenant_id"},
		{"a data key", manageRequest{Data: json.RawMessage(`{"\u0000":"v"}`)}, "data.\x00"},
		{"nested in data", manageRequest{Data: json.RawMessage(`{"metadata":{"note":["\u0000"]}}`)}, "data.metadata"},
		{"data that is no object", manageRequest{Data: json.RawMessage(`["\u0000"]`)}, "data"},
		{"after", manageRequest{After: json.RawMessage(`{"id":"\u0000"}`)}, "after.id"},
		{"fields", manageRequest{Fields: json.RawMessage(`["\u0000"]`)}, "fields"},
		{"an escaped backslash is no NUL", manageRequest{Data: json.RawMessage(`{"a":"\\u0000"}`)}, ""},
	} {
		if got := manageNULField(tc.req); got != tc.want {
			t.Errorf("%s: manageNULField = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestManageNULFieldReadsEveryField — every field of manageRequest is of a
// kind manageNULField reads (or of one that cannot carry text): a field of
// another kind would pass a NUL to the action unchecked.
func TestManageNULFieldReadsEveryField(t *testing.T) {
	read := map[reflect.Type]bool{
		reflect.TypeFor[string]():          true,
		reflect.TypeFor[[]string]():        true,
		reflect.TypeFor[json.RawMessage](): true,
		reflect.TypeFor[int]():             true, // no text
		reflect.TypeFor[bool]():            true, // no text
	}
	rt := reflect.TypeFor[manageRequest]()
	for i := range rt.NumField() {
		if f := rt.Field(i); !read[f.Type] {
			t.Errorf("manageRequest.%s is a %v, a kind manageNULField does not read — extend it", f.Name, f.Type)
		}
	}
}
