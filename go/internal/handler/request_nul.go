package handler

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
)

// A NUL character (U+0000) in a value of a request is a value the database
// cannot store: PostgreSQL refuses it in text (22021) and in JSON (22P05), so
// every query it reaches fails (issue #519). Where such a refusal answered a
// write, #505 classified it as the caller's mistake (callerDataError); a
// query ahead of the write — a lookup by the request's value — still answered
// 500, often without a line (scope-create's tenant lookup). The request is
// refused before any action runs instead: 400 naming the field, no line.

// jsonCarriesNUL reports whether raw, valid JSON, holds a NUL character in a
// string — its only spelling there is the escape \u0000 (a raw control
// character is invalid JSON). An escape is one whose backslash is not itself
// escaped: an odd run of backslashes ends at it. No allocation, one pass.
func jsonCarriesNUL(raw []byte) bool {
	const esc = `\u0000`
	for i := 0; ; {
		j := bytes.Index(raw[i:], []byte(esc))
		if j < 0 {
			return false
		}
		at := i + j
		run := 0
		for k := at; k >= 0 && raw[k] == '\\'; k-- {
			run++
		}
		if run%2 == 1 {
			return true
		}
		i = at + 1
	}
}

// manageNULField returns the field of req that carries a NUL character —
// "data.<key>" for a key of a data object (nulField: the first key, in key
// order, whose name or value carries one) — or "" when none does. It reads
// every field of manageRequest by its kind (string, []string,
// json.RawMessage), so a field added later is checked without an edit here;
// a field of another kind fails TestManageNULFieldReadsEveryField until it
// is handled.
func manageNULField(req manageRequest) string {
	v := reflect.ValueOf(req)
	t := v.Type()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		switch f := v.Field(i).Interface().(type) {
		case string:
			if strings.ContainsRune(f, 0) {
				return name
			}
		case []string:
			if slices.ContainsFunc(f, func(s string) bool { return strings.ContainsRune(s, 0) }) {
				return name
			}
		case json.RawMessage:
			if !jsonCarriesNUL(f) {
				continue
			}
			var keys map[string]json.RawMessage
			if json.Unmarshal(f, &keys) == nil {
				if k := nulField(keys); k != "" {
					return name + "." + k
				}
			}
			return name
		}
	}
	return ""
}
