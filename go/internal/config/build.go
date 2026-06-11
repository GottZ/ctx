package config

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
)

// Override is one context_settings row addressed to this build: the canonical
// registry key plus the raw scalar string form of its JSONB value (the
// settings package unwraps JSON strings/numbers/booleans before calling Build,
// so both sources — env and DB — share one typed parser per field).
type Override struct {
	Key   string
	Value string
}

// SecretResolver resolves a secret_ref value (the NAME of a context_secrets
// row) to its plaintext. Implementations must keep error messages free of the
// ref value and of any plaintext: until the F2 write-gate (W6) enforces the
// ref format on settings writes, a provider key mistakenly stored AS the ref
// value would otherwise leak into WARN logs through the error path.
type SecretResolver func(name string) (string, error)

// entryByKey indexes the registry by canonical key, built once.
var entryByKey = sync.OnceValue(func() map[string]entry {
	m := make(map[string]entry, len(registry()))
	for _, e := range registry() {
		m[e.Key] = e
	}
	return m
})

// admittedOverride is one override that passed admission: its registry entry
// plus the parsed (or secret-resolved) typed value.
type admittedOverride struct {
	e   entry
	val any
}

// Build constructs the effective runtime config: env base overlaid with
// context_settings overrides (precedence DB override > env > default; the
// sources map records "settings" for overlaid keys). Secret-class values are
// secret_refs and resolve to plaintext via resolve — in-memory only, no issue
// message ever carries a resolved value (§3.3 log invariant, pinned by the
// leak-scan test).
//
// Boot tolerance (W17): nothing in the override layer is ever fatal. Every
// unusable override — unknown key, non-hot mutability, parse failure (even on
// parse:"strict" fields: strictness is an env-input contract, a DB row must
// never gain boot-abort power), failed secret resolution, validation error —
// degrades to a WARN naming field and reason while the env/default value
// stays active. SeverityError in the returned issues can therefore only
// originate from the env base itself, which the boot already fail-fasted on.
func Build(overrides []Override, resolve SecretResolver) (*Config, []Issue) {
	var issues []Issue
	adm := make([]admittedOverride, 0, len(overrides))
	for _, o := range overrides {
		a, warn := admitOverride(o, resolve)
		if warn != nil {
			issues = append(issues, *warn)
			continue
		}
		adm = append(adm, a)
	}

	// Validate-with-individual-drop: build a fresh candidate per iteration
	// (Validate normalizes in place, so every pass restarts from the env
	// base), then drop exactly the overrides whose fields carry a
	// SeverityError. An error not attributable to any override (env-level, or
	// cross-field through an env value) withdraws ALL overrides — the final
	// pass returns the pure env build: degraded, loud, never fatal.
	for {
		candidate, runIssues := buildCandidate(adm)
		if !HasErrors(runIssues) {
			return candidate, append(issues, runIssues...)
		}

		keep, dropWarns := dropOffenders(adm, runIssues)
		if len(dropWarns) > 0 {
			issues = append(issues, dropWarns...)
			adm = keep
			continue
		}
		if len(adm) > 0 {
			issues = append(issues, Issue{Field: "settings", Severity: SeverityWarn,
				Msg: fmt.Sprintf("validation failed on non-overridden fields — withdrawing all %d DB overrides for this generation", len(adm))})
			adm = nil
			continue
		}
		// Pure env build still carries errors: surface them unchanged (the
		// boot path has already exited on env errors before any Build call).
		return candidate, append(issues, runIssues...)
	}
}

// admitOverride gates one override against the registry: key known, mutability
// hot, value parseable (or secret_ref resolvable). Returns the typed value or
// the WARN explaining why the override is ignored.
func admitOverride(o Override, resolve SecretResolver) (admittedOverride, *Issue) {
	warn := func(msg string) *Issue {
		return &Issue{Field: o.Key, Severity: SeverityWarn, Msg: msg}
	}
	e, ok := entryByKey()[o.Key]
	if !ok {
		// Risk 7 (code downgrade): a row written by a newer binary stays
		// dormant until the binary catches up — visible, never fatal.
		return admittedOverride{}, warn("unknown settings key — override ignored")
	}
	if e.Mut != "hot" && e.Mut != "coupled:embed-cache" {
		// restart keys take effect only at process start (§7.3: no pending
		// state in F2), coupled keys need a re-embed migration no override can
		// deliver, and the server.* DSN group must stay env-only — the pool
		// that LOADED the override is bound to the env DSN (circular).
		// coupled:embed-cache keys (embed/dream_embed host+protocol) ARE
		// overridable since G16: the settings write path flushes
		// context_embed_cache when their effective values change (X2), which
		// removes the stale-vector hazard (R5) that kept them pinned.
		return admittedOverride{}, warn(fmt.Sprintf("key is not runtime-overridable (mutability %q) — override ignored", e.Mut))
	}
	if e.Secret != "" {
		if e.typ != typString {
			return admittedOverride{}, warn("secret-class field is not string-typed — override ignored")
		}
		if resolve == nil {
			return admittedOverride{}, warn("secret_ref set but no secret resolver available — override ignored, env/default value stays")
		}
		plaintext, err := resolve(o.Value)
		if err != nil {
			// err comes from the SecretResolver contract: name-free,
			// value-free — safe to embed.
			return admittedOverride{}, warn(fmt.Sprintf("secret_ref resolution failed (%v) — override ignored, env/default value stays", err))
		}
		return admittedOverride{e: e, val: plaintext}, nil
	}
	val, err := parserFor(e.typ)(o.Value, e.defVal)
	if err != nil {
		// WARN even on parse:"strict" fields — boot tolerance (W17): the env
		// path keeps its SeverityError, the DB path never aborts a boot.
		return admittedOverride{}, warn(fmt.Sprintf("%v — override ignored, env/default value stays", err))
	}
	return admittedOverride{e: e, val: val}, nil
}

// buildCandidate assembles env base + admitted overrides and validates. The
// env base is re-parsed per call: Validate normalizes in place, and snapshots
// must never share mutable state across generations.
func buildCandidate(adm []admittedOverride) (*Config, []Issue) {
	c, issues := FromEnv()
	rv := reflect.ValueOf(c).Elem()
	for _, a := range adm {
		val := a.val
		if s, isSlice := val.([]string); isSlice {
			// Same immutability rule as fromSources: fresh backing array per
			// generation.
			val = slices.Clone(s)
		}
		rv.FieldByIndex(a.e.path).Set(reflect.ValueOf(val))
		c.sources[a.e.Key] = "settings"
	}
	issues = append(issues, Validate(c)...)
	return c, issues
}

// dropOffenders removes every admitted override whose field carries a
// SeverityError in issues, emitting one WARN per drop that names field and
// reason. keep aliases nothing from adm's backing array growth path; the
// returned warns are empty when no error was attributable to an override.
func dropOffenders(adm []admittedOverride, issues []Issue) (keep []admittedOverride, warns []Issue) {
	reason := map[string]string{}
	for _, is := range issues {
		if is.Severity == SeverityError {
			if _, seen := reason[is.Field]; !seen {
				reason[is.Field] = is.Msg
			}
		}
	}
	for _, a := range adm {
		msg, offending := reason[a.e.Key]
		if !offending {
			keep = append(keep, a)
			continue
		}
		warns = append(warns, Issue{Field: a.e.Key, Severity: SeverityWarn,
			Msg: fmt.Sprintf("settings override dropped: %s — keeping env/default value", msg)})
	}
	return keep, warns
}
