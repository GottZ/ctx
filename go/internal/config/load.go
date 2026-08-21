package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// Severity classifies an Issue. Any SeverityError aborts startup (after all
// issues are logged) and rejects a Store.Replace.
type Severity int

// Severity levels.
const (
	SeverityWarn Severity = iota
	SeverityError
)

// String renders the severity for the boot dump / F2 API responses.
func (s Severity) String() string {
	if s == SeverityError {
		return "error"
	}
	return "warn"
}

// Issue is one parse or validation finding, addressed by canonical registry
// key. Msg is human-readable and goes 1:1 into the F2 API response.
// Convention: URLs are embedded ONLY via (*url.URL).Redacted() — never raw.
// On url.Parse FAILURES no *url.URL exists to redact and (*url.Error).Error()
// embeds the RAW input (`parse "<raw>": …`) — Msg must then carry only the
// field name and a generic reason, never the error text (§3.5 userinfo leak).
type Issue struct {
	Field    string
	Severity Severity
	Msg      string
}

// HasErrors reports whether any issue is fatal.
func HasErrors(issues []Issue) bool {
	for _, is := range issues {
		if is.Severity == SeverityError {
			return true
		}
	}
	return false
}

// parser converts one raw source value to the field's typed value. def is the
// entry's parsed default (only the scopes parser falls back to it). A non-nil
// error means "value unusable" — the caller substitutes the default and emits
// an Issue with the entry's strictness as severity.
type parser func(raw string, def any) (any, error)

func parserFor(t reflect.Type) parser {
	switch t {
	case typString:
		return func(raw string, _ any) (any, error) { return raw, nil }
	case typInt:
		return parseIntValue
	case typFloat:
		return parseFloatValue
	case typBool:
		// Deliberately exact-match like the legacy `getEnv(...) == "true"`:
		// "1", "TRUE", "yes" are all false. Equivalence over convenience.
		return func(raw string, _ any) (any, error) { return raw == "true", nil }
	case typProtocol:
		return func(raw string, _ any) (any, error) { return backends.Protocol(raw), nil }
	case typThink:
		return func(raw string, _ any) (any, error) { return backends.ThinkMode(raw), nil }
	case typDuration:
		return parseDurationSeconds
	case typHours:
		return parseHoursValue
	case typLocation:
		return parseLocationValue
	case typScopes:
		return parseScopesValue
	case typSensitivity:
		return parseSensitivityValue
	case typScopeFloor:
		return parseScopeFloorValue
	}
	return nil
}

// parseSensitivityValue admits exactly the four defined levels — the value
// feeds a numeric rank comparison in the trust gate, so an unknown level is a
// broken gate, not a "new tenant value" (F3 §2.3a).
func parseSensitivityValue(raw string, _ any) (any, error) {
	s := backends.Sensitivity(raw)
	if !backends.ValidSensitivity(s) {
		return nil, fmt.Errorf("invalid sensitivity %q (credentials|personal|internal|public)", raw)
	}
	return s, nil
}

// parseScopeFloorValue parses the scope→min-sensitivity JSON map (F3 §2.3d).
// The value travels as a JSON STRING through the scalar settings layer
// (consistent with scopes/timezone — the registry is string-sourced). Scope
// keys are free-form (scope is the generalized VARCHAR); levels are hard.
func parseScopeFloorValue(raw string, _ any) (any, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("invalid scope floor (want JSON object {\"scope\":\"sensitivity\"})")
	}
	out := make(ScopeFloor, len(m))
	for scope, v := range m {
		s := backends.Sensitivity(v)
		if !backends.ValidSensitivity(s) {
			return nil, fmt.Errorf("scope floor %q: invalid sensitivity %q", scope, v)
		}
		out[scope] = s
	}
	return out, nil
}

func parseIntValue(raw string, _ any) (any, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid integer %q", raw)
	}
	return n, nil
}

func parseFloatValue(raw string, _ any) (any, error) {
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid number %q", raw)
	}
	return f, nil
}

// parseDurationSeconds keeps the legacy bare-int-seconds form of
// CTX_CHAT_FALLBACK_TIMEOUT / CTX_DREAM_IDLE_WAIT.
func parseDurationSeconds(raw string, _ any) (any, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid seconds value %q", raw)
	}
	return time.Duration(n) * time.Second, nil
}

func parseHoursValue(raw string, _ any) (any, error) {
	h, ok := parseCooldownHours(raw)
	if !ok {
		return nil, fmt.Errorf("invalid duration %q (want h|d|w|m|y suffix or bare hours)", raw)
	}
	return Hours(h), nil
}

func parseLocationValue(raw string, _ any) (any, error) {
	if raw == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", raw)
	}
	return loc, nil
}

// parseScopesValue keeps the legacy parseScopes semantics: comma-split, trim,
// drop empty parts; an input with no usable part falls back to the default
// (now with a WARN instead of silently).
func parseScopesValue(raw string, def any) (any, error) {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return def, fmt.Errorf("no non-empty scope in %q", raw)
	}
	return result, nil
}

// cooldownUnitHours maps the domain duration suffixes to hours. Deliberately
// NOT Go's time.ParseDuration: that uses m=minutes / s=seconds (meaningless
// for re-dream intervals, and m=minutes is a months trap). Here the units are
// the ones that make sense for cooldowns — h/d/w/m/y — with m = month (30d).
var cooldownUnitHours = map[byte]float64{
	'h': 1,
	'd': 24,
	'w': 24 * 7,
	'm': 24 * 30,
	'y': 24 * 365,
}

// parseCooldownHours parses a cooldown duration to hours. Accepts a unit
// suffix h|d|w|m|y (e.g. "12h", "45d", "1w", "1m", "1y") or a bare number
// (interpreted as hours). Returns (hours, true) on success; (0, false) on a
// malformed value so callers fall back to their default.
func parseCooldownHours(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	last := s[len(s)-1]
	if mult, ok := cooldownUnitHours[last|0x20]; ok { // |0x20 lowercases ASCII letter
		n, err := strconv.ParseFloat(strings.TrimSpace(s[:len(s)-1]), 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n * mult, true
	}
	// No recognized suffix → bare number is hours.
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// fromSources builds a Config from a lookup keyed by CANONICAL REGISTRY KEY
// (not env var) — F2 stacks its settings lookup in front of the env lookup
// without a signature break; fields without an env var (scheduler.home_scope)
// need no special path. lookup returns (value, true) when the source provides
// the key; empty/absent values return false (legacy getEnv semantics: empty
// env == unset).
func fromSources(lookup func(key string) (string, bool)) (*Config, []Issue) {
	entries := registry()
	c := &Config{sources: make(map[string]string, len(entries))}
	rv := reflect.ValueOf(c).Elem()
	var issues []Issue

	for _, e := range entries {
		val := e.defVal
		src := "default"
		if raw, ok := lookup(e.Key); ok {
			parsed, err := parserFor(e.typ)(raw, e.defVal)
			switch {
			case err == nil:
				val = parsed
				src = "env"
			case e.Strict:
				// Today's fatal-parse paths (getEnvInt + timezone): the value
				// stays default, the SeverityError aborts boot after logging.
				issues = append(issues, Issue{Field: e.Key, Severity: SeverityError, Msg: err.Error()})
			default:
				// Today's silent getEnv*Safe fallbacks — same default, now
				// visible (Delta 3).
				issues = append(issues, Issue{Field: e.Key, Severity: SeverityWarn,
					Msg: fmt.Sprintf("%s — using default %q", err.Error(), e.defRaw)})
			}
		}
		if s, isSlice := val.([]string); isSlice {
			// Fresh slice per config generation — snapshots never share
			// mutable backing arrays (immutability convention).
			val = slices.Clone(s)
		}
		rv.FieldByIndex(e.path).Set(reflect.ValueOf(val))
		c.sources[e.Key] = src
	}
	return c, issues
}

// Defaults builds a Config from the registry defaults alone — no env, no
// settings: what an installation on which nobody set anything runs on.
//
// It was introduced (A02-W5) as the comparison base of the conditional env
// backend seed; β1 removed that seed, so the function currently has no
// caller. It is kept rather than deleted because the surface it exposes —
// "the registry defaults as a Config" — is the one design/02 §9 hands to the
// Achse-01 key retirement to decide on ("W5s Default-Vergleich braucht die
// Registry-Defaults als exportierte Vergleichsbasis, solange die Keys
// leben"), and that decision is not this wave's.
//
// No Issues can arise — the default path never parses a raw value — so the
// slice is dropped rather than pushed onto every caller.
func Defaults() *Config {
	c, _ := fromSources(func(string) (string, bool) { return "", false })
	return c
}

// FromEnv parses the environment into a Config: fromSources with an env
// lookup (registry maps key → env var; env:"-" fields skip the env source).
// Callers run Validate afterwards and abort on HasErrors.
func FromEnv() (*Config, []Issue) {
	byKey := envVarByKey()
	return fromSources(func(key string) (string, bool) {
		envVar := byKey[key]
		if envVar == "" {
			return "", false
		}
		v := os.Getenv(envVar)
		if v == "" {
			return "", false
		}
		return v, true
	})
}
