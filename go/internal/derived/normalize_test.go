package derived

import "testing"

// TestGate4_NormalizeFoldsFullwidth is gate 4 of §7 W01-1.
//
// "ｋｅｙ" (U+FF4B U+FF45 U+FF59) and "key" are the SAME text rendered
// differently. NFKC collapses them; NFC does not. For a containment gate that
// is the difference between a real quote in a different normalisation form
// being found and being thrown away — and the failure mode is a careless
// model, not an attacker.
//
// Red probe: swap norm.NFKC for norm.NFC in Normalize.
func TestGate4_NormalizeFoldsFullwidth(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{"fullwidth latin", "ｋｅｙ", "key"},
		{"ligature", "ﬁle", "file"},
		{"circled digit", "①", "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, want := Normalize(c.a), Normalize(c.b); got != want {
				t.Errorf("Normalize(%q) = %q, Normalize(%q) = %q — NFKC must fold them together",
					c.a, got, c.b, want)
			}
		})
	}
}

// TestNormalizeKeepsRealSpellingDifferences — NFKC folds rendering, not
// spelling. "Straße" and "Strasse" are a genuine difference and must survive,
// otherwise the gate starts accepting quotes that are not in the source.
func TestNormalizeKeepsRealSpellingDifferences(t *testing.T) {
	if Normalize("Straße") == Normalize("Strasse") {
		t.Error("Normalize collapsed ß and ss; that is a spelling difference, not a rendering one")
	}
}

// TestNormalizeCaseAndWhitespace pins the remaining three steps: case folding,
// whitespace collapse over every unicode.IsSpace run, and trim.
func TestNormalizeCaseAndWhitespace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Der  Trigram-Arm\tnutzt\nseinen Index nicht.  ", "der trigram-arm nutzt seinen index nicht."},
		{"GROSS und klein", "gross und klein"},
		{" geschütztes Leerzeichen ", "geschütztes leerzeichen"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeKeepsPunctuation — K4 explicitly excludes punctuation
// stripping: it would raise the false-ACCEPT rate for nothing.
func TestNormalizeKeepsPunctuation(t *testing.T) {
	if got, want := Normalize("A, B; C."), "a, b; c."; got != want {
		t.Errorf("Normalize(%q) = %q, want %q — punctuation must survive", "A, B; C.", got, want)
	}
}
