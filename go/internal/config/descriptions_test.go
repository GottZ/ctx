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

// TestSupersededExposed pins the API surface of the superseded tag: the
// f3:context_backends keys STILL IN THE REGISTRY carry it through Keys()
// (settings UI legacy section + PUT 409), and structural keys do not.
//
// The count follows the cut train down (29 → 26 → 22 → 17 → 11 → 6 → 0, β3…β8)
// and this pin dies with the mechanism in β9. It is deliberately NOT the
// completeness statement of the retirement — a wave that removed a tuple and
// simply lowered this number would look identical to one that lost a key. That
// statement lives in retired_test.go, where the golden list stays at 29 and the
// ratchet has to name every key the registry lost.
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
	if superseded != 11 {
		t.Errorf("superseded key count = %d, want 11 (chat/embed tuples — rerank cut in β3, chat_fallback in β4, dream_embed in β5, dream in β6)", superseded)
	}
	if info, _ := KeyByName("dream.backoff_mode"); info.Superseded != "" {
		t.Error("dream.backoff_mode must not be superseded")
	}
}
