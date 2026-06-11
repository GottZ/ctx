package config

import (
	"reflect"
	"sync"
)

// KeyInfo is the exported registry metadata for one settings key — the F2
// GET /api/settings contract (and later the F4 settings UI's widget source).
// It carries everything the registry knows about a key EXCEPT live values:
// rendering an effective value goes through RenderValue, which applies the
// SurfaceAPI redaction rules.
type KeyInfo struct {
	Key        string // canonical settings key ("rerank.blend_weight")
	EnvVar     string // env var name; "" = no env source (settings-only)
	Type       string // typeName() — drives the UI widget choice
	Mutability string // hot | restart | coupled | coupled:embed-cache
	Sensitive  bool   // secret-class field: values are masked everywhere
	Default    any    // registry default, rendered like RenderValue
}

// typeNames maps the registry leaf types to their wire names. The names are
// API contract (settings UI widget dispatch) — change them only with the
// consumers.
var typeNames = map[reflect.Type]string{
	typString:   "string",
	typInt:      "int",
	typFloat:    "float",
	typBool:     "bool",
	typProtocol: "protocol",
	typThink:    "think",
	typDuration: "seconds",
	typHours:    "hours",
	typLocation: "timezone",
	typScopes:   "scopes",
}

// keyInfos builds the exported view once. Registry order == struct order, the
// stable order GET /api/settings renders.
var keyInfos = sync.OnceValue(func() []KeyInfo {
	out := make([]KeyInfo, 0, len(registry()))
	for _, e := range registry() {
		out = append(out, KeyInfo{
			Key:        e.Key,
			EnvVar:     envVarByKey()[e.Key],
			Type:       typeNames[e.typ],
			Mutability: e.Mut,
			Sensitive:  e.Secret != "",
			Default:    renderField(e, reflect.ValueOf(e.defVal), SurfaceAPI),
		})
	}
	return out
})

// Keys returns the registry metadata for every settings key, in registry
// order. The slice is shared — callers must not mutate it.
func Keys() []KeyInfo {
	return keyInfos()
}

// KeyByName returns the metadata for one canonical key.
func KeyByName(key string) (KeyInfo, bool) {
	for _, k := range keyInfos() {
		if k.Key == key {
			return k, true
		}
	}
	return KeyInfo{}, false
}

// RenderValue renders the effective value of one key from this snapshot under
// the SurfaceAPI redaction rules: secret-class values are presence-only
// ("set"/""), .host URLs are userinfo-redacted, durations/hours/timezones
// render as their string forms. ok=false for unknown keys.
//
// Note for the settings API: for sensitive keys this is only the fail-safe
// floor — the handler applies the §3.4 masking rule on top ("(set via env)" /
// secret_ref name), because a DB-sourced sensitive value in the snapshot is
// the RESOLVED plaintext and must never be rendered from here.
func RenderValue(c *Config, key string) (any, bool) {
	e, ok := entryByKey()[key]
	if !ok {
		return nil, false
	}
	rv := reflect.ValueOf(c).Elem()
	return renderField(e, rv.FieldByIndex(e.path), SurfaceAPI), true
}
