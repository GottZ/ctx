package blocktype

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// ── one reading of a config for Go and for SQL ──────────────────────────────
//
// DecodePolicy reads a row's config with encoding/json; the database reads the
// same stored jsonb with its own operators — config->'retrieval'->>'policy' in
// ctx_block_type_never_embedded and trg_block_types_embed_excluded (migration
// 175), config->'retrieval'->'untrusted' in trg_block_retype (migration 174).
// The two disagree on two things a config can carry:
//
//   - Spelling. encoding/json matches a struct field without regard to case,
//     a jsonb operator matches the key byte for byte. {"Retrieval":{"policy":
//     "excluded"}} is an excluded policy for Go and no policy at all for SQL.
//   - Repetition. encoding/json decodes a repeated key into the value it
//     already holds, so two copies of an object merge; jsonb keeps the last
//     copy only, and keeps both of two spellings. {"retrieval":{"policy":
//     "excluded"},"retrieval":{"untrusted":true}} is excluded for Go and has
//     no policy for SQL.
//
// The narrowing gate and the registry read the config through DecodePolicy,
// the never-embedded rule reads it in SQL, so either difference let a tenant
// overlay pass the gate as `excluded` while SQL read it as lifting the
// exclusion for every tenant (#291, third review round). checkConfigKeys
// rejects both: every key of an object that decodes into a config struct is
// spelled exactly like its field, and no object repeats a key. What passes
// reads the same in Go and in SQL. A stored row that breaks either rule fails
// DecodePolicy on reload like any other corrupt config.

// configEnvelopeType is the root of the walk: the struct DecodePolicy decodes.
var configEnvelopeType = reflect.TypeOf(cfgEnvelope{})

// checkConfigKeys walks the first JSON value of raw, which the strict decoder
// has already accepted, and checks the spelling and the uniqueness of every
// key against the type that value decodes into.
func checkConfigKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return walkConfigKeys(dec, configEnvelopeType, "")
}

// walkConfigKeys reads one JSON value from dec. t is the Go type the value
// decodes into (nil where no struct type applies below a map or an
// interface); path is the key path of the value for the error text.
func walkConfigKeys(dec *json.Decoder, t reflect.Type, path string) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar carries no key
	}
	switch delim {
	case '[':
		var elem reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for i := 0; dec.More(); i++ {
			if err := walkConfigKeys(dec, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case '{':
		var fields map[string]reflect.Type
		var elem reflect.Type
		if t != nil {
			switch t.Kind() {
			case reflect.Struct:
				fields = configFields(t)
			case reflect.Map:
				elem = t.Elem()
			}
		}
		seen := make(map[string]bool)
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			at := key
			if path != "" {
				at = path + "." + key
			}
			if seen[key] {
				return fmt.Errorf("key %q repeats in one object (the database keeps its last copy, the decoder merges them)", at)
			}
			seen[key] = true
			vt := elem
			if fields != nil {
				ft, ok := fields[key]
				if !ok {
					return fmt.Errorf("key %q is not spelled like its field %s (config keys are case-sensitive: the database reads them byte for byte)", at, canonicalKey(fields, key))
				}
				vt = ft
			}
			if err := walkConfigKeys(dec, vt, at); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // the closing delimiter
	return err
}

// configFields maps each JSON key of struct type t to its field type, the key
// being the name encoding/json writes for the field.
func configFields(t reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields
}

// canonicalKey names the field a mis-spelled key matched in the decoder, for
// the error text.
func canonicalKey(fields map[string]reflect.Type, key string) string {
	for name := range fields {
		if strings.EqualFold(name, key) {
			return fmt.Sprintf("%q", name)
		}
	}
	return "(none)"
}
