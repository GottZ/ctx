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

// TestSupersededExposed pins the API surface of the superseded tag: which keys
// carry it through Keys() — the settings UI legacy section and the PUT 409 read
// it from there.
//
// The count followed the cut train down (29 → 26 → 22 → 17 → 11 → 6 → 0,
// β3…β8) and has arrived at ZERO: chat was the last marked tuple. What the pin
// asserts from here is the end state — no key is exposed as superseded — and it
// dies with the mechanism itself in β9 (E11), together with
// registry_test.go's TestRegistrySupersededSet.
//
// It was deliberately never the completeness statement of the retirement: a
// wave that removed a tuple and simply lowered this number would have looked
// identical to one that LOST a key. That statement lives in retired_test.go,
// where the golden list stays at 29 and the ratchet has to name every key the
// registry lost — which is also why zero here is not the wave's proof of work.
func TestSupersededExposed(t *testing.T) {
	for _, info := range Keys() {
		if info.Superseded != "" {
			t.Errorf("%s is exposed as superseded=%q — the marker has been unoccupied since β8; "+
				"a new carrier would claim a replacement in context_backends that the cut train already made",
				info.Key, info.Superseded)
		}
	}
}
