package config

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// entry is one registry row: the single source of truth binding a canonical
// settings key to its env var, type, default, mutability, secret class and
// parse strictness. F2 validates settings-writes against exactly this table.
type entry struct {
	Key        string // canonical settings key, e.g. "rerank.blend_weight"
	EnvVar     string // env var name; "-" = no env source (settings-only from F2)
	Mut        string // hot | restart | coupled | coupled:embed-cache
	Secret     string // "" | "fp" | "presence"
	Strict     bool   // parse:"strict" — malformed value is a boot abort
	Superseded string // "" | "f3:context_backends" (lifetime marker)

	defRaw string       // raw default tag value
	defVal any          // default parsed through the same typed parser
	path   []int        // field index chain into Config
	typ    reflect.Type // leaf field type (drives the parser dispatch)
}

var (
	typString   = reflect.TypeOf("")
	typInt      = reflect.TypeOf(int(0))
	typFloat    = reflect.TypeOf(float64(0))
	typBool     = reflect.TypeOf(false)
	typProtocol = reflect.TypeOf(backends.Protocol(""))
	typThink    = reflect.TypeOf(backends.ThinkMode(""))
	typDuration = reflect.TypeOf(time.Duration(0))
	typHours    = reflect.TypeOf(Hours(0))
	typLocation = reflect.TypeOf((*time.Location)(nil))
	typScopes   = reflect.TypeOf([]string(nil))
)

var validMut = map[string]bool{
	"hot": true, "restart": true, "coupled": true, "coupled:embed-cache": true,
}

// registry returns the parsed registry, built once. A malformed struct tag is
// a programmer error and panics at first use (covered by the registry tests).
var registry = sync.OnceValue(func() []entry {
	entries, err := buildRegistry(reflect.TypeOf(Config{}))
	if err != nil {
		panic(fmt.Sprintf("config: invalid registry: %v", err))
	}
	return entries
})

// EnvVars returns every env var the registry reads, in registry order. Test
// surface (golden-test env hygiene) and F2 contract (key ↔ env mapping).
func EnvVars() []string {
	out := make([]string, 0, len(registry()))
	for _, e := range registry() {
		if e.EnvVar != "-" {
			out = append(out, e.EnvVar)
		}
	}
	return out
}

// envVarByKey maps canonical key -> env var name ("" for env:"-" fields).
var envVarByKey = sync.OnceValue(func() map[string]string {
	m := make(map[string]string, len(registry()))
	for _, e := range registry() {
		if e.EnvVar == "-" {
			m[e.Key] = ""
			continue
		}
		m[e.Key] = e.EnvVar
	}
	return m
})

// buildRegistry walks the tagged config struct. Every exported leaf field
// must carry key + env + default + mut tags — an untagged field is an error,
// so no config field can exist outside the F2 settings contract.
func buildRegistry(root reflect.Type) ([]entry, error) {
	var out []entry
	seenKey := map[string]bool{}
	seenEnv := map[string]bool{}

	var visit func(t reflect.Type, base []int) error
	visit = func(t reflect.Type, base []int) error {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue // sources map — loader-internal, not a config field
			}
			path := append(append([]int{}, base...), i)
			key, hasKey := f.Tag.Lookup("key")
			if !hasKey {
				if f.Type.Kind() == reflect.Struct {
					if err := visit(f.Type, path); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("field %s.%s has no key tag and is not a group struct", t.Name(), f.Name)
			}
			e, err := buildEntry(t.Name(), f, key, path)
			if err != nil {
				return err
			}
			if seenKey[e.Key] {
				return fmt.Errorf("duplicate key %q", e.Key)
			}
			seenKey[e.Key] = true
			if e.EnvVar != "-" {
				if seenEnv[e.EnvVar] {
					return fmt.Errorf("duplicate env var %q", e.EnvVar)
				}
				seenEnv[e.EnvVar] = true
			}
			out = append(out, e)
		}
		return nil
	}
	if err := visit(root, nil); err != nil {
		return nil, err
	}
	return out, nil
}

func buildEntry(owner string, f reflect.StructField, key string, path []int) (entry, error) {
	loc := fmt.Sprintf("%s.%s (%s)", owner, f.Name, key)

	env, hasEnv := f.Tag.Lookup("env")
	if !hasEnv || env == "" {
		return entry{}, fmt.Errorf("%s: missing env tag (use env:\"-\" for settings-only fields)", loc)
	}
	def, hasDef := f.Tag.Lookup("default")
	if !hasDef {
		return entry{}, fmt.Errorf("%s: missing default tag", loc)
	}
	mut := f.Tag.Get("mut")
	if !validMut[mut] {
		return entry{}, fmt.Errorf("%s: invalid mut tag %q", loc, mut)
	}
	secret := f.Tag.Get("secret")
	if secret != "" && secret != "fp" && secret != "presence" {
		return entry{}, fmt.Errorf("%s: invalid secret tag %q", loc, secret)
	}
	parse := f.Tag.Get("parse")
	if parse != "" && parse != "strict" && parse != "safe" {
		return entry{}, fmt.Errorf("%s: invalid parse tag %q", loc, parse)
	}

	e := entry{
		Key:        key,
		EnvVar:     env,
		Mut:        mut,
		Secret:     secret,
		Strict:     parse == "strict",
		Superseded: f.Tag.Get("superseded"),
		defRaw:     def,
		path:       path,
		typ:        f.Type,
	}
	if parserFor(e.typ) == nil {
		return entry{}, fmt.Errorf("%s: unsupported field type %s", loc, e.typ)
	}
	defVal, err := parserFor(e.typ)(def, nil)
	if err != nil {
		return entry{}, fmt.Errorf("%s: malformed default %q: %w", loc, def, err)
	}
	e.defVal = defVal
	return e, nil
}
