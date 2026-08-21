package config

import "testing"

// TestKeyDescriptionsComplete pins keyDescriptions to the registry both
// ways: every registry key carries a non-empty description (the keyInfos
// panic makes this unmissable, the test makes it a readable failure), and
// no orphan description outlives its key — a renamed key must move its
// description or this fails.
func TestKeyDescriptionsComplete(t *testing.T) {
	keys := make(map[string]bool, len(registry()))
	for _, e := range registry() {
		keys[e.Key] = true
		desc, ok := keyDescriptions[e.Key]
		if !ok || desc == "" {
			t.Errorf("key %s has no description", e.Key)
		}
	}
	for k := range keyDescriptions {
		if !keys[k] {
			t.Errorf("keyDescriptions entry %s has no registry key (orphan)", k)
		}
	}
}

// TestKeyInfosCarryDesc pins the API surface: Keys() serves the description
// so GET /api/settings renders it (the settings UI hint + search corpus).
func TestKeyInfosCarryDesc(t *testing.T) {
	for _, info := range Keys() {
		if info.Desc == "" {
			t.Errorf("KeyInfo %s has empty Desc", info.Key)
		}
	}
}

// TestSupersededExposed died with its subject in β9 (E11): it pinned which keys
// Keys() exposed as superseded — first a shrinking count (29 → 26 → 22 → 17 →
// 11 → 6 → 0 across β3…β8), from β8 on the end state "no key is". With
// KeyInfo.Superseded gone there is no surface left to assert about.
//
// It was deliberately never the completeness statement of the retirement: a
// wave that removed a tuple and simply lowered its number would have looked
// identical to one that LOST a key. That statement lives in retired_test.go,
// where the golden list stays at 29 and the ratchet names every key the
// registry lost — and it is unaffected by this removal.
