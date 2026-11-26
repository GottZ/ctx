package blocktype

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MergeConfig lays the config of a type UPDATE over the config the row stores
// (issue #565) and returns the config the update writes.
//
// THE RULE. The envelope has two levels: sections (retrieval, guard, dream,
// digest, overview, parent, write, workflow, classify) and their fields, plus
// the top-level v and structural_link_classes. A field the update names
// replaces the stored field whole — an array or the forge_state_map map
// included; a field the update leaves out keeps its stored value; a field or
// section the update names as null is removed, so it decodes to its default
// again. A section the update does not name keeps all of its stored fields.
//
// WHY. type-update and PUT /api/types used to write the update's config as
// the whole new config, and DecodePolicy fills every absent axis with its
// default. An operator tuning one axis of a builtin type — the damping factor
// of session-digest — reset every other one: measured on 184c0012, the body
// {"v":1,"retrieval":{"damping_factor":0.45}} left session-digest full-pass,
// trusted, guard-checked, dream-linkable and in digest and overview, and the
// same body on audit-trail dropped its ten title rules, its intent patterns
// and its structural link class — every audit-trail-titled block written
// after it stayed knowledge. Nothing in the answer said so.
//
// A config that names every field the row stores — the stored config with one
// value changed, the shape the admin form sends — writes the same under both
// rules. What differs is a field the update leaves out: it is kept, where it
// was reset. Reset it by naming it null.
//
// The update's part is checked on its own for everything a config is checked
// for before the cross-field rules run (decodeEnvelope: size, unknown keys,
// spelling and repetition of keys, v=1); the merged config then goes through
// DecodePolicy like any config. A create has no stored config and writes its
// config as given.
func MergeConfig(name string, stored, update []byte) ([]byte, error) {
	if _, err := decodeEnvelope(name, update); err != nil {
		return nil, err
	}
	base, err := decodeConfigObject(stored)
	if err != nil {
		return nil, fmt.Errorf("blocktype %q: stored config: %w", name, err)
	}
	patch, err := decodeConfigObject(update)
	if err != nil {
		return nil, fmt.Errorf("blocktype %q: config: %w", name, err)
	}
	for key, val := range patch {
		section, isObject := val.(map[string]any)
		switch {
		case val == nil:
			delete(base, key)
		case isObject:
			merged, _ := base[key].(map[string]any)
			if merged == nil {
				merged = map[string]any{}
			}
			for field, fv := range section {
				if fv == nil {
					delete(merged, field)
					continue
				}
				merged[field] = fv
			}
			base[key] = merged
		default:
			base[key] = val
		}
	}
	return json.Marshal(base)
}

// decodeConfigObject reads a config as a JSON object, numbers kept as written
// (json.Number), so a merged config carries the stored values byte for byte.
func decodeConfigObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return m, nil
}
