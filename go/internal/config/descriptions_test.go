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

// TestSupersededExposed pins the API surface of the superseded tag: the 29
// f3:context_backends keys carry it through Keys() (settings UI legacy
// section + PUT 409), and structural keys do not.
func TestSupersededExposed(t *testing.T) {
	superseded := 0
	for _, info := range Keys() {
		if info.Superseded != "" {
			superseded++
			if info.Superseded != "f3:context_backends" {
				t.Errorf("%s: unexpected superseded value %q", info.Key, info.Superseded)
			}
		}
	}
	if superseded != 29 {
		t.Errorf("superseded key count = %d, want 29 (chat/chat_fallback/embed/dream/dream_embed/rerank tuples)", superseded)
	}
	if info, _ := KeyByName("dream.backoff_mode"); info.Superseded != "" {
		t.Error("dream.backoff_mode must not be superseded")
	}
}
