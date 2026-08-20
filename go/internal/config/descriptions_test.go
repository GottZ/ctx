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
