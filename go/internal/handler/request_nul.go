package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/GottZ/ctx/internal/backends"
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
// "data.<key>" for a key of a data object (the key the first NUL in the text
// sits under; the object itself when a key carries it) — or "" when none
// does. It reads
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
			// One pass over the text, no decode (the review of #530: a 1 MB
			// object of many keys took 57 ms and 233,000 allocations to
			// name); a key that carries the NUL names its object, so the
			// answer does not echo the character.
			if k, found := jsonNULKey(f); found {
				if k == "" { // a key of the object carries it
					return name
				}
				return name + "." + k
			}
		}
	}
	return ""
}

// manageNULFieldForms names the actions whose form shows a refusal next to
// its field: the web UI's backend and model-alias dialogs read the fields[]
// of a 422 (ValidateBackend's answer), so a NUL in a key of their data is
// refused in that form, the field the key — the review of #530 found the
// dispatcher's 400 text a form error there, no longer a field error.
var manageNULFieldForms = map[string]bool{
	"backend-create":     true,
	"backend-update":     true,
	"model-alias-create": true,
	"model-alias-update": true,
}

// writeManageNULRefusal answers the dispatcher's NUL refusal: 400 naming the
// field, or for a data key of a form action its 422 field error.
func writeManageNULRefusal(w http.ResponseWriter, action, field string) {
	if key, ok := strings.CutPrefix(field, "data."); ok && manageNULFieldForms[action] {
		writeBackendValidation(w, []backends.FieldError{{Field: key, Message: nulFieldMessage}})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": field + " " + nulFieldMessage})
}
