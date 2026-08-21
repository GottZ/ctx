package config

import (
	"sort"
	"strings"
	"testing"
)

// TestRetiredKeysMatchSupersededRegistry is the sync pin of the retirement
// list: retiredSettingKeys covers EXACTLY the keys the live registry marks
// superseded — no more, no less. As long as both lists exist (until the
// registry cut removes the 29 keys and the superseded tag with them), a key
// that joins or leaves the superseded set without following into retired.go
// fails here first — a hand-maintained second transcript of 29 strings can
// never drift in silently.
//
// It doubles as the TRANSITIONAL form of the collision pin: set equality with
// the superseded set proves that every retired key is today still registered
// AND carries a lifetime marker — no name in the map is a live, unmarked
// config key. After the cut the invariant flips to its end form
// (Registry ∩ retiredSettingKeys = ∅) and the expectation becomes the static
// 29-name list; the pin is permanent either way, because it is what stops a
// future release from re-registering one of these names.
func TestRetiredKeysMatchSupersededRegistry(t *testing.T) {
	superseded := map[string]bool{}
	for _, info := range Keys() {
		if info.Superseded != "" {
			superseded[info.Key] = true
		}
	}
	if len(superseded) != 29 {
		t.Fatalf("registry carries %d superseded keys, want 29 (the backend role tuples)", len(superseded))
	}
	if len(retiredSettingKeys) != len(superseded) {
		t.Errorf("retiredSettingKeys has %d entries, superseded registry set has %d",
			len(retiredSettingKeys), len(superseded))
	}
	for key := range retiredSettingKeys {
		if !superseded[key] {
			info, registered := KeyByName(key)
			t.Errorf("retired key %s is not superseded in the registry (registered=%v, superseded=%q)",
				key, registered, info.Superseded)
		}
	}
	for key := range superseded {
		if _, ok := retiredSettingKeys[key]; !ok {
			t.Errorf("superseded key %s is missing from retiredSettingKeys", key)
		}
	}
}

// TestRetiredEnvNamesMatchRegistry pins the mechanical key → env derivation
// against the living registry tags: every name RetiredEnvNames() produces is
// the env var the registry actually reads for that key, and it is still part
// of EnvVars(). This is what lets the boot WARN and the tombstone tripwire
// consume a DERIVED list instead of a third transcript — if the derivation
// were wrong for even one key, the sweep would watch a var nobody ever sets.
func TestRetiredEnvNamesMatchRegistry(t *testing.T) {
	live := map[string]bool{}
	for _, name := range EnvVars() {
		live[name] = true
	}
	for key := range retiredSettingKeys {
		info, ok := KeyByName(key)
		if !ok {
			t.Errorf("retired key %s is not in the registry — cannot verify its env name", key)
			continue
		}
		derived := retiredEnvName(key)
		if derived != info.EnvVar {
			t.Errorf("%s: derived env name %q, registry tag %q", key, derived, info.EnvVar)
		}
		if !live[derived] {
			t.Errorf("%s: derived env name %q is not in EnvVars()", key, derived)
		}
	}

	names := RetiredEnvNames()
	if len(names) != len(retiredSettingKeys) {
		t.Errorf("RetiredEnvNames() returned %d names, map has %d keys", len(names), len(retiredSettingKeys))
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("RetiredEnvNames() is not sorted: %v", names)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Errorf("RetiredEnvNames() lists %s twice", name)
		}
		seen[name] = true
	}
}

// TestRetiredTargetsNamePoolLocation pins the map VALUES as usable data: the
// docs, the runbook and the delete migration's notice quote them as the place
// the value moved to, so an empty or unspecific target would ship an operator
// message that answers nothing. The values never reach the wire (E13: the keys
// answer a plain 404), which is exactly why no test elsewhere would catch rot
// in them.
func TestRetiredTargetsNamePoolLocation(t *testing.T) {
	for key, target := range retiredSettingKeys {
		if target == "" {
			t.Errorf("retired key %s has no move target", key)
			continue
		}
		if !strings.Contains(target, "context_backends") {
			t.Errorf("retired key %s: target %q does not name a context_backends location", key, target)
		}
	}
}

// TestGamingKeysStayOutOfTheRetiredList separates the two retirement vintages.
// gaming.active / gaming.disabled_backends were retired in U01-W5: unregistered
// since, never env-sourced, no pool destination — they answer 404 through the
// unknownKey path today and need neither an env sweep nor a move target. The
// backend tuple keys are the newer vintage and reach the same 404 only after
// the registry cut (E13). Putting the gaming keys into retiredSettingKeys would
// invent env vars that never existed (CTX_GAMING_ACTIVE) and feed the tripwire
// a name it can never see set.
func TestGamingKeysStayOutOfTheRetiredList(t *testing.T) {
	for _, key := range []string{"gaming.active", "gaming.disabled_backends"} {
		if _, ok := retiredSettingKeys[key]; ok {
			t.Errorf("%s must not be in retiredSettingKeys (different retirement vintage)", key)
		}
		if _, ok := KeyByName(key); ok {
			t.Errorf("%s is registered again — the U01-W5 retirement expects an unknown key (404)", key)
		}
	}
	for _, name := range RetiredEnvNames() {
		if name == "CTX_GAMING_ACTIVE" || name == "CTX_GAMING_DISABLED_BACKENDS" {
			t.Errorf("RetiredEnvNames() lists %s — the gaming keys never had env vars", name)
		}
	}
}
