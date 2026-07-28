// F3-P3 pool policy keys: typed parsers (hard level set — the value feeds a
// numeric rank comparison, an unknown level is a broken gate), the scope
// floor (raise-only) and the guard tag surfacing in KeyInfo.
package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

func TestParseSensitivityValue(t *testing.T) {
	for _, ok := range []string{"credentials", "personal", "internal", "public"} {
		v, err := parseSensitivityValue(ok, nil)
		if err != nil || v.(backends.Sensitivity) != backends.Sensitivity(ok) {
			t.Errorf("parse %q: v=%v err=%v", ok, v, err)
		}
	}
	for _, bad := range []string{"", "secret", "CREDENTIALS", "3"} {
		if _, err := parseSensitivityValue(bad, nil); err == nil {
			t.Errorf("parse %q: want error (hard level set)", bad)
		}
	}
}

func TestParseScopeFloorValue(t *testing.T) {
	v, err := parseScopeFloorValue(`{"friend":"personal","work":"internal"}`, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	floor := v.(ScopeFloor)
	if floor["friend"] != backends.SensPersonal || floor["work"] != backends.SensInternal {
		t.Errorf("floor = %v", floor)
	}

	if v, err = parseScopeFloorValue(`{}`, nil); err != nil || len(v.(ScopeFloor)) != 0 {
		t.Errorf("empty floor: v=%v err=%v", v, err)
	}

	for _, bad := range []string{`{"a":"secret"}`, `["personal"]`, `not-json`, `{"a":3}`} {
		if _, err := parseScopeFloorValue(bad, nil); err == nil {
			t.Errorf("parse %q: want error", bad)
		}
	}
}

func TestScopeFloorApplyOnlyRaises(t *testing.T) {
	floor := ScopeFloor{"friend": backends.SensPersonal}
	if got := floor.Apply(backends.SensPublic, "friend"); got != backends.SensPersonal {
		t.Errorf("public under personal floor = %q, want personal", got)
	}
	if got := floor.Apply(backends.SensCredentials, "friend"); got != backends.SensCredentials {
		t.Errorf("credentials under personal floor = %q, want credentials (never lowers)", got)
	}
	if got := floor.Apply(backends.SensPublic, "private"); got != backends.SensPublic {
		t.Errorf("unfloored scope = %q, want unchanged", got)
	}
}

// TestPoolKeysGuardAndDefaults pins the F3-P3 registry surface: both default
// keys carry the sensitivity-downgrade guard and the fail-closed defaults
// (block: credentials per E1; query: personal per E2).
func TestPoolKeysGuardAndDefaults(t *testing.T) {
	cases := map[string]struct {
		guard string
		def   any
	}{
		"pool.default_query_sensitivity": {"sensitivity-downgrade", "personal"},
		"pool.default_block_sensitivity": {"sensitivity-downgrade", "credentials"},
		"pool.scope_sensitivity_floor":   {"", nil},
		// B2: the blob write budget joined the pool surface. No downgrade guard
		// (a rate limit is not an egress border), but the same settings-only
		// birth and a default that must stay POSITIVE — 0 would hand the
		// fallback to query.rate_limit_write for every fresh install.
		"pool.blob_rate_limit_write": {"", 10},
	}
	for key, want := range cases {
		info, ok := KeyByName(key)
		if !ok {
			t.Errorf("%s: missing from registry", key)
			continue
		}
		if info.Guard != want.guard {
			t.Errorf("%s: guard = %q, want %q", key, info.Guard, want.guard)
		}
		if info.EnvVar != "" {
			t.Errorf("%s: env var = %q, want settings-only", key, info.EnvVar)
		}
		if want.def != nil && info.Default != want.def {
			t.Errorf("%s: default = %v, want %v", key, info.Default, want.def)
		}
	}
}
