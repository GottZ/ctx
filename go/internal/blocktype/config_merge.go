package blocktype

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
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
	return marshalConfig(base)
}

// marshalConfig writes a config as compact JSON WITHOUT HTML escaping and
// without the encoder's trailing newline: json.Marshal turns each & < > into
// a six-byte \u escape, so a valid config of title patterns made of those
// characters (3 KiB as sent) grew past maxConfigBytes and the unchanged config
// sent back was refused (PR #567 review).
func marshalConfig(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// UpdateConfig is the config a type UPDATE writes over a row's stored config:
// MergeConfig when the stored config decodes — and a REPLACEMENT when it does
// not.
//
// A stored config DecodePolicy rejects — a key a newer release wrote, read
// after a rollback; a key in another spelling from before the spelling check;
// a value planted by SQL; no object at all — means nothing the registry could
// load: a '_global' row of it fails every Reload, a tenant row its tenant's
// whole set. Merging over it keeps the broken part, so not even the whole
// valid config could repair the row (PR #567 review, executed: 422 on both
// transports). Over such a row the update's config is the whole new config,
// with its nulls dropped (null means "absent" when nothing is kept).
//
// A partial config must still not pass as a whole one, or the silent reset of
// issue #565 returns through the broken row: the replacement must name every
// section the stored config names — in any spelling, since a misspelled key
// still says which section the row meant; a section named null is named, it
// resets the section — and, for a builtin name, every
// section its shipped definition sets (shippedSections). Otherwise 422, naming
// the broken stored config and the sections. A row that names no section, of a
// name with no shipped definition, has nothing a partial config could drop.
func UpdateConfig(name, scope string, builtin, isDefault bool, stored, update []byte) ([]byte, error) {
	_, storedErr := DecodePolicy(name, scope, builtin, isDefault, stored)
	if storedErr == nil {
		return MergeConfig(name, stored, update)
	}
	// The request as sent is checked first, as MergeConfig checks it (PR #567
	// review): the copy below is a map, which keeps only the last of a repeated
	// key, loses an unknown key named null and is smaller than the request by
	// its nulls — the checks of the written config alone would pass all three.
	if _, err := decodeEnvelope(name, update); err != nil {
		return nil, err
	}
	patch, err := decodeConfigObject(update)
	if err != nil {
		return nil, fmt.Errorf("blocktype %q: config: %w", name, err)
	}
	// A section named null is named: it resets the section (PR #567 review).
	// So the request's own keys are read before its nulls are dropped.
	var missing []string
	for _, key := range requiredSections(name, stored) {
		if _, ok := patch[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key, val := range patch {
		section, isObject := val.(map[string]any)
		switch {
		case val == nil:
			delete(patch, key)
		case isObject:
			for field, fv := range section {
				if fv == nil {
					delete(section, field)
				}
			}
		}
	}
	whole, err := marshalConfig(patch)
	if err != nil {
		return nil, fmt.Errorf("blocktype %q: config: %w", name, err)
	}
	if _, err := decodeEnvelope(name, whole); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("blocktype %q: the stored config does not decode (%v), so an update cannot keep its "+
			"other fields — send the whole config, which replaces the stored one; it must name %s",
			name, storedErr, strings.Join(missing, ", "))
	}
	return whole, nil
}

// requiredSections are the top-level keys a replacement of a broken stored
// config must name (UpdateConfig): the sections the stored config names,
// matched to the envelope's keys without regard to case, and the sections the
// shipped definition of a builtin name sets. Sorted; v is not a section.
func requiredSections(name string, stored []byte) []string {
	keys := map[string]bool{}
	for _, k := range shippedSections(name) {
		keys[k] = true
	}
	if m, err := decodeConfigObject(stored); err == nil {
		for k := range m {
			if lk := strings.ToLower(k); envelopeKeys()[lk] && lk != "v" {
				keys[lk] = true
			}
		}
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// envelopeKeys are the top-level keys of the config envelope, read off the
// struct DecodePolicy decodes (one list, no copy to drift).
func envelopeKeys() map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < configEnvelopeType.NumField(); i++ {
		if tag := strings.Split(configEnvelopeType.Field(i).Tag.Get("json"), ",")[0]; tag != "" {
			keys[tag] = true
		}
	}
	return keys
}

// shippedSections are the sections in which the compiled definition of a
// builtin name differs from what an absent section decodes to — the settings
// a partial config would silently reset. nil for a name that is not builtin.
func shippedSections(name string) []string {
	b, ok := BuiltinPolicy(name)
	if !ok {
		return nil
	}
	d, err := DecodePolicy(name, b.Scope, true, b.IsDefault, []byte(`{"v":1}`))
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range []struct {
		key            string
		shipped, empty any
	}{
		{"retrieval", b.Retrieval, d.Retrieval},
		{"guard", b.Guard, d.Guard},
		{"dream", b.Dream, d.Dream},
		{"digest", b.Digest, d.Digest},
		{"overview", b.Overview, d.Overview},
		{"parent", b.Parent, d.Parent},
		{"write", b.Write, d.Write},
		{"workflow", b.Workflow, d.Workflow},
		{"structural_link_classes", b.StructuralLinkClasses, d.StructuralLinkClasses},
		{"classify", b.Classify, d.Classify},
	} {
		if !reflect.DeepEqual(s.shipped, s.empty) {
			out = append(out, s.key)
		}
	}
	return out
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
