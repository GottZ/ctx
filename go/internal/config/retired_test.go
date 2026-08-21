package config

import (
	"sort"
	"strings"
	"testing"
)

// retiredKeysGolden is the static 29-name expectation of the retirement — the
// half of the α15 pin that has to outlive its own subject. Until this wave the
// completeness check compared retiredSettingKeys against the live superseded
// registry set; that expectation evaporates tuple by tuple as the cut waves
// land and says nothing at all once the last one is gone. A hand-written golden
// is the only expectation that still holds after the cut: an edit to the map —
// a lost key would silently shrink the env sweep, the boot row sweep and the
// delete migration's second list, a stray one would sweep a living key — fails
// here without borrowing its truth from the thing being removed.
//
// Grouped by the wave that removes the tuple from the registry, in cut order.
var retiredKeysGolden = []string{
	// β3 — rerank
	"rerank.api_key", "rerank.host", "rerank.model",
	// β4 — chat_fallback
	"chat_fallback.api_key", "chat_fallback.host", "chat_fallback.protocol", "chat_fallback.timeout",
	// β5 — dream_embed
	"dream_embed.api_key", "dream_embed.host", "dream_embed.model", "dream_embed.num_ctx", "dream_embed.protocol",
	// β6 — dream
	"dream.api_key", "dream.host", "dream.model", "dream.num_ctx", "dream.protocol", "dream.think",
	// β7 — embed
	"embed.api_key", "embed.host", "embed.model", "embed.num_ctx", "embed.protocol",
	// β8 — chat
	"chat.api_key", "chat.host", "chat.model", "chat.num_ctx", "chat.protocol", "chat.think",
}

// retiredKeysAlreadyCut carries the retired keys whose registry entry is GONE.
// It is the ratchet of the cut train: every key wave moves its tuple here in
// the same commit that deletes the struct fields, and the pin below refuses
// both halves of a mismatch — a key cut without its line (the registry lookup
// misses where the pin expects a hit) and a line without the cut (the lookup
// hits where the pin expects a miss). The expectation can neither be loosened
// ahead of the removal nor be left stale behind one, which is what turns a
// per-wave count into an invariant.
//
// Empty at β2 by construction: the preparation wave writes the end form, the
// cut waves fill the list (29 → 26 → 22 → 17 → 11 → 6 → 0 registered). Once the
// chat tuple lands here (β8) this file asserts exactly
// `Registry ∩ retiredSettingKeys = ∅` plus the full EnvVars() inversion — the
// chat.host positive probe that no wave before the cut could run, since the
// registry is reflection-built and cannot be faked in a test (registry.go).
// Lexically sorted, not in cut order (the pin below requires it) — the wave
// comments name the commit each block arrived with.
var retiredKeysAlreadyCut = []string{
	// β4 — chat_fallback
	"chat_fallback.api_key", "chat_fallback.host", "chat_fallback.protocol", "chat_fallback.timeout",
	// β5 — dream_embed
	"dream_embed.api_key", "dream_embed.host", "dream_embed.model", "dream_embed.num_ctx", "dream_embed.protocol",
	// β3 — rerank
	"rerank.api_key", "rerank.host", "rerank.model",
}

// TestRetiredKeysMatchGoldenList pins the map contents against the static
// 29-name list: the retirement covers exactly the six backend role tuples, no
// more, no less. Together with TestRetiredKeysLeaveTheRegistryWithTheirWave it
// replaces the α15 set-equality against the superseded registry set — same two
// statements (the list is complete, no name on it is a live config key), but
// only the second one still consults a registry that is about to lose these
// keys.
func TestRetiredKeysMatchGoldenList(t *testing.T) {
	golden := map[string]bool{}
	for _, key := range retiredKeysGolden {
		if golden[key] {
			t.Errorf("retiredKeysGolden lists %s twice", key)
		}
		golden[key] = true
	}
	if len(golden) != 29 {
		t.Fatalf("retiredKeysGolden carries %d distinct keys, want 29 (the six backend role tuples)", len(golden))
	}
	if len(retiredSettingKeys) != len(golden) {
		t.Errorf("retiredSettingKeys has %d entries, golden list has %d", len(retiredSettingKeys), len(golden))
	}
	for key := range retiredSettingKeys {
		if !golden[key] {
			t.Errorf("retiredSettingKeys carries %s, which is not in the golden list", key)
		}
	}
	for key := range golden {
		if _, ok := retiredSettingKeys[key]; !ok {
			t.Errorf("golden key %s is missing from retiredSettingKeys", key)
		}
	}
}

// TestRetiredKeysLeaveTheRegistryWithTheirWave is the collision pin in its end
// form, ratcheted through the cut train. The invariant it protects is permanent
// and outlives the cut: no name in retiredSettingKeys is a live, unmarked
// config key — before its wave because the registry entry still carries the
// superseded marker, after its wave because there is no registry entry left.
// That is what stops a future release from re-registering one of these names:
// a re-registered name would silently make every stale row on it effective
// configuration again (build.go admits any registered key) and would shadow the
// retirement wherever it is documented.
//
// E13 (404, not 410) is why this file is the whole contract on the config side:
// the retired keys answer through the ordinary unknownKey path, so "retired"
// means precisely "not in the registry" — there is no tombstone response that
// could carry the statement instead.
func TestRetiredKeysLeaveTheRegistryWithTheirWave(t *testing.T) {
	cut := map[string]bool{}
	for _, key := range retiredKeysAlreadyCut {
		if _, ok := retiredSettingKeys[key]; !ok {
			t.Errorf("retiredKeysAlreadyCut lists %s, which is not a retired key at all", key)
		}
		if cut[key] {
			t.Errorf("retiredKeysAlreadyCut lists %s twice", key)
		}
		cut[key] = true
	}
	if !sort.StringsAreSorted(retiredKeysAlreadyCut) {
		t.Errorf("retiredKeysAlreadyCut is not sorted: %v", retiredKeysAlreadyCut)
	}

	for key := range retiredSettingKeys {
		info, registered := KeyByName(key)
		switch {
		case cut[key] && registered:
			t.Errorf("%s is listed as cut but the registry still carries it (superseded=%q) — "+
				"a retired name back in the registry revives every stale row on it", key, info.Superseded)
		case !cut[key] && !registered:
			t.Errorf("%s is gone from the registry but missing from retiredKeysAlreadyCut — "+
				"the cut wave moves its tuple into the ratchet in the same commit", key)
		case !cut[key] && info.Superseded == "":
			t.Errorf("%s is registered without a superseded marker — a retired name is either "+
				"marked for its remaining lifetime or gone", key)
		}
	}
}

// TestRetiredEnvNamesFollowTheCut pins the mechanical key → env derivation and
// inverts the α15 EnvVars() assertion wave by wave. Before its cut a retired
// key's derived name must BE the env var the registry reads for it and must be
// part of EnvVars() — that is what lets the boot WARN and the β13 tripwire
// consume a derived list instead of a third transcript of 29 strings. After its
// cut the same name must be ABSENT from EnvVars(): the cut wave is only done
// when the env surface is gone with the key, not just the struct field. With
// the ratchet empty the second half is dormant; with it full this test is the
// per-tuple env absence gate of every key wave (design/01 W3: "EnvVars()
// enthält keine CTX_CHAT_FALLBACK_*").
func TestRetiredEnvNamesFollowTheCut(t *testing.T) {
	live := map[string]bool{}
	for _, name := range EnvVars() {
		live[name] = true
	}
	cut := map[string]bool{}
	for _, key := range retiredKeysAlreadyCut {
		cut[key] = true
	}

	for key := range retiredSettingKeys {
		derived := retiredEnvName(key)
		if info, registered := KeyByName(key); registered && derived != info.EnvVar {
			t.Errorf("%s: derived env name %q, registry tag %q", key, derived, info.EnvVar)
		}
		if cut[key] {
			if live[derived] {
				t.Errorf("%s is cut but %q is still in EnvVars() — the env surface must go with the key", key, derived)
			}
			continue
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
