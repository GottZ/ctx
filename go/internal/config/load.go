package config

import (
	"encoding/json"
	"fmt"
	"math"
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

// maxDurationSeconds is the largest whole second a time.Duration can hold —
// beyond it the `n * time.Second` multiply below wraps.
const maxDurationSeconds = int64(math.MaxInt64) / int64(time.Second)

// parseDurationSeconds keeps the legacy bare-int-seconds form of
// CTX_DREAM_IDLE_WAIT and every other time.Duration key.
//
// The range check is the parser's own class of statement — "unrepresentable
// as this type", the same class as "abc" — and it can live NOWHERE else: the
// wrap produces a PLAUSIBLE SMALL POSITIVE duration (9223372036854 s renders
// as -775.808ms, 9223372036855 s as 224.192ms), so no downstream range check
// can tell it from a configured value. Symmetric on purpose — a large
// negative input wraps into the positive range just as invisibly.
//
// The SIGN is deliberately NOT checked here: -30 is representable, and
// rejecting it in the parser would degrade 33 of the 37 seconds keys to
// WARN + registry default (only the four parse:"strict" dispatch.* keys
// abort). Negatives are V17's job in Validate, which is fatal for every key.
func parseDurationSeconds(raw string, _ any) (any, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid seconds value %q", raw)
	}
	if int64(n) > maxDurationSeconds || int64(n) < -maxDurationSeconds {
		return nil, fmt.Errorf("seconds value %q out of range (max ±%d s)", raw, maxDurationSeconds)
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
// backend seed; β1 removed that seed, so no production code calls it. It is
// kept because it is the only exported form of "the registry defaults as a
// Config", and 11 call sites across 9 test files rest on it (internal/events
// 4 files, internal/handler 4, internal/config 1) — deleting it would make
// each of them build that base by hand. The earlier note here tied the
// function to the Achse-01 key retirement; that retirement has landed.
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
