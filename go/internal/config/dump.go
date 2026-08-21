package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// Surface selects the trust level of a redacted rendering.
type Surface int

// Render surfaces.
const (
	// SurfaceBootDump is the host log: host access == full trust. api_key
	// values of fpMinLen+ render a sha256 fingerprint so key rotation/drift
	// between deployments is provable without leaking the value.
	SurfaceBootDump Surface = iota
	// SurfaceAPI becomes the F2 GET /api/settings rendering: ALL secrets are
	// presence-only ("set"/""), never fingerprinted — one line with F2's
	// /api/secrets contract ("never values, never fingerprints"); a web admin
	// key is a weaker trust tier than host access.
	SurfaceAPI
)

// fpMinLen is the minimum api_key length for a fingerprint. Below it the
// value may be human-chosen (Delta 5 routes basic-auth proxy users onto
// api_key) and an unsalted 32-bit hash prefix would be an offline dictionary
// confirmation oracle — those render presence-only. Real provider keys
// (51–164 chars) sit far above the threshold.
const fpMinLen = 24

// inheritMarkers annotates zero-valued fields whose runtime value comes from
// another group (the resolved accessors in config.go).
var inheritMarkers = map[string]string{
	"dream.model":          "(inherit chat)",
	"dream.num_ctx":        "(inherit chat)",
	"dream.think":          "(inherit chat)",
	"dream_embed.host":     "(inherit embed)",
	"dream_embed.api_key":  "(inherit embed)",
	"dream_embed.protocol": "(inherit embed)",
	"dream_embed.model":    "(inherit embed)",
	"dream_embed.num_ctx":  "(inherit embed)",
}

// Redacted renders the effective config as nested maps: one object per key
// group, field names == registry key suffixes (1:1 greppable against the F2
// settings namespace), secrets masked per class and surface, plus a "sources"
// object (env|default per key — makes the compose gap visible: a declared-
// but-never-arriving env var shows as "default").
func (c *Config) Redacted(surface Surface) map[string]any {
	out := map[string]any{}
	rv := reflect.ValueOf(c).Elem()
	sources := make(map[string]string, len(registry()))

	for _, e := range registry() {
		group, field, _ := strings.Cut(e.Key, ".")
		g, ok := out[group].(map[string]any)
		if !ok {
			g = map[string]any{}
			out[group] = g
		}
		g[field] = renderField(e, rv.FieldByIndex(e.path), surface)
		sources[e.Key] = c.sources[e.Key]
	}
	out["sources"] = sources
	return out
}

// renderField converts one field value to its dump representation.
func renderField(e entry, v reflect.Value, surface Surface) any {
	if e.Secret != "" {
		return maskSecret(e, v.String(), surface)
	}
	if marker, ok := inheritMarkers[e.Key]; ok && v.IsZero() {
		return marker
	}
	if strings.HasSuffix(e.Key, ".host") {
		return redactHostURL(v.String())
	}
	switch val := v.Interface().(type) {
	case time.Duration:
		return val.String()
	case Hours:
		return renderHours(val)
	case *time.Location:
		if val == nil {
			return ""
		}
		return val.String()
	case backends.Protocol:
		return string(val)
	case backends.ThinkMode:
		return string(val)
	case backends.Sensitivity:
		return string(val)
	case ScopeFloor:
		if val == nil {
			return ScopeFloor{}
		}
		return val // JSON object; empty default renders as {}
	default:
		return val // string, int, float64, bool, []string render natively
	}
}

// redactHostURL renders a *.host field through (*url.URL).Redacted() — the
// §3.5 convention. V7 rejects userinfo hosts as ERROR anyway; this covers the
// window in which a value is rendered before/while Validate rejects it. On
// url.Parse FAILURE no *url.URL exists to redact and the error text would
// embed the raw input — the value is withheld entirely. The suffix rule is
// deliberately a namespace convention, not a field list: every current and
// future .host key (F3 pool) is redacted fail-safe; for credential-free
// values Redacted() returns the input verbatim, so the dump stays useful.
func redactHostURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL withheld)"
	}
	return u.Redacted()
}

// maskSecret applies the per-class masking (§3.5).
func maskSecret(e entry, value string, surface Surface) string {
	if value == "" {
		return ""
	}
	if e.Secret == "fp" && surface == SurfaceBootDump && len(value) >= fpMinLen {
		sum := sha256.Sum256([]byte(value))
		return "set:sha256:" + hex.EncodeToString(sum[:4])
	}
	return "set"
}

// renderHours renders an Hours value compactly: whole-day multiples as "Nd",
// everything else as hours ("12h", "45d", "1.5h").
func renderHours(h Hours) string {
	f := float64(h)
	if f >= 24 && math.Mod(f, 24) == 0 {
		return fmt.Sprintf("%gd", f/24)
	}
	return fmt.Sprintf("%gh", f)
}

// dumpGroupOrder fixes the attribute order of the boot record. A name whose
// keys have all left the registry is dropped here in the same wave: BootDumpArgs
// skips groups Redacted did not produce, so a stale name would be invisible
// rather than wrong — and invisible dead ordering is what this list must not
// accumulate over the cut train (design/01 §3, "dumpGroupOrder auf 6 Gruppen").
// chat_fallback left with its tuple in β4.
var dumpGroupOrder = []string{
	"server", "chat", "embed", "dream", "dream_embed",
	"rerank", "graph", "query", "scheduler",
}

// BootDumpArgs renders the effective config + issues as slog args for the
// single "config: effective" boot record. Secrets are masked per
// SurfaceBootDump; URLs only ever enter issues via (*url.URL).Redacted().
func BootDumpArgs(c *Config, issues []Issue) []any {
	groups := c.Redacted(SurfaceBootDump)
	args := make([]any, 0, 2*(len(dumpGroupOrder)+2))
	for _, name := range dumpGroupOrder {
		if g, ok := groups[name]; ok {
			args = append(args, name, g)
		}
	}
	args = append(args, "sources", groups["sources"])

	// Issue order is preserved: parse findings (registry order) before
	// validate findings (V-order).
	rendered := make([]map[string]string, 0, len(issues))
	for _, is := range issues {
		rendered = append(rendered, map[string]string{
			"field": is.Field, "severity": is.Severity.String(), "msg": is.Msg,
		})
	}
	args = append(args, "issues", rendered)
	return args
}
