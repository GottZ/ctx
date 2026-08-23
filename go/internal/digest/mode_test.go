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
// The three properties hold in EVERY language: a translation that outgrows the
// gate, loses the address or loses the search command breaks the stub in exactly
// the way that makes it worthless.
func TestStubTextShape(t *testing.T) {
	for _, lang := range []string{"", "de", "de-CH", "en", "fr"} {
		text := stubText("private", lang)
		if len(text) > 512 {
			t.Errorf("%q: stub is %d B, over the 512 B gate", lang, len(text))
		}
		if !strings.Contains(text, "root-map-private") {
			t.Errorf("%q: stub does not name its successor:\n%s", lang, text)
		}
		if !strings.Contains(text, "ctx search index query:root-map") {
			t.Errorf("%q: stub gives no way to FIND the successor — the whole reason it is not an archival:\n%s", lang, text)
		}
		if !strings.Contains(text, "ctx get <") {
			t.Errorf("%q: stub lost the second CLI line — a translated command does not run:\n%s", lang, text)
		}
		if text != stubText("private", lang) {
			t.Errorf("%q: stub text is not stable across calls", lang)
		}
		for _, moving := range []string{"20", ":0", "Blöcke geführt"} {
			if strings.Contains(text, moving) {
				t.Errorf("%q: stub contains a moving part (%q) — every digest cycle would rewrite it:\n%s", lang, moving, text)
			}
		}
		// Per scope, so a reader of the work map is not sent to the private one.
		if strings.Contains(stubText("work", lang), "root-map-private") {
			t.Errorf("%q: the stub points every scope at the same map", lang)
		}
	}
}

// TestStubTextLanguage is the issue-#34 half: the pointer speaks the language of
// the map it points AT (dream.language, same key, same two tables, same
// primary-subtag rule as the root map itself).
//
// The German branch is asserted BYTE-EXACT, not by keyword: it is the frozen
// legacy text, and any drift in it rewrites every deployed stub block once —
// writeStub compares content before it writes, so an accidental byte is a real
// write, not a no-op.
func TestStubTextLanguage(t *testing.T) {
	const frozenDE = "Diese Karte wurde abgelöst.\n" +
		"Die Wurzel-Map dieses Scopes heißt: root-map-private\n" +
		"  ctx search index query:root-map    ·    ctx get <id aus der Trefferliste>\n" +
		"Sie gliedert nach Themen-Clustern (Louvain über den Dream-Graphen) statt nach\n" +
		"Kategorien und ist auf ~15 KB gedeckelt. Erzeugt am Overview-Rebuild-Zyklus.\n"

	for _, lang := range []string{"", "  ", "de", "DE", "de-CH", "de-DE"} {
		if got := stubText("private", lang); got != frozenDE {
			t.Errorf("%q did not render the frozen German stub:\n%s", lang, got)
		}
	}
	for _, lang := range []string{"en", "en-GB", "fr", "ja"} {
		got := stubText("private", lang)
		if got == frozenDE {
			t.Errorf("%q still renders the German stub", lang)
		}
		for _, german := range []string{"Karte", "Wurzel-Map", "Scopes heißt", "Trefferliste",
			"Themen-Clustern", "Kategorien", "gedeckelt", "Erzeugt"} {
			if strings.Contains(got, german) {
				t.Errorf("%q: German scaffolding %q survived into the stub:\n%s", lang, german, got)
			}
		}
		if !strings.Contains(got, "superseded") {
			t.Errorf("%q: stub does not say it was superseded:\n%s", lang, got)
		}
	}
}

func TestStubLanguagePrimarySubtag(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "  ": "", "de": "de", "DE": "de", " de-CH ": "de",
		"en": "en", "en-GB": "en", "fr": "fr",
	} {
		if got := stubLanguage(in); got != want {
			t.Errorf("stubLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}
