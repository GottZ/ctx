// Wave W-E unit gates (Cluster-Topic-Map, design/02 §4.6): the mode vocabulary
// and the shape of the stub — both decidable without a database.
package digest

import (
	"strings"
	"testing"
)

// TestNormalizeFallsBackToFull: a typo in the mode must never silently STOP the
// topic map. `off` is a legitimate value and a plausible typo result, and its
// symptom — a block that quietly stops moving — is one an operator finds weeks
// later. Falling back to the behaviour that already exists is the fail-closed
// direction here.
func TestNormalizeFallsBackToFull(t *testing.T) {
	for in, want := range map[string]string{
		"full": ModeFull, "stub": ModeStub, "off": ModeOff,
		"FULL": ModeFull, " Stub ": ModeStub, "OFF": ModeOff,
		"": ModeFull, "stubb": ModeFull, "disabled": ModeFull, "0": ModeFull,
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStubTextShape pins the three properties the stub lives by: it fits the
// gate size, it names its successor, and it carries NO moving part.
//
// The last one is load-bearing: the digest runs on a 60 s debounce. A stub that
// re-renders differently every cycle would swap an 80 KB pointless rewrite for a
// 300 B pointless rewrite instead of removing it, and the content comparison in
// writeStub could never skip anything.
func TestStubTextShape(t *testing.T) {
	text := stubText("private")
	if len(text) > 512 {
		t.Errorf("stub is %d B, over the 512 B gate", len(text))
	}
	if !strings.Contains(text, "root-map-private") {
		t.Errorf("stub does not name its successor:\n%s", text)
	}
	if !strings.Contains(text, "ctx search index query:root-map") {
		t.Errorf("stub gives no way to FIND the successor — the whole reason it is not an archival:\n%s", text)
	}
	if text != stubText("private") {
		t.Error("stub text is not stable across calls")
	}
	for _, moving := range []string{"20", ":0", "Blöcke geführt"} {
		if strings.Contains(text, moving) {
			t.Errorf("stub contains a moving part (%q) — every digest cycle would rewrite it:\n%s", moving, text)
		}
	}
	// Per scope, so a reader of the work map is not sent to the private one.
	if strings.Contains(stubText("work"), "root-map-private") {
		t.Error("the stub points every scope at the same map")
	}
}
